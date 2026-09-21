package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

type recoveryRestoreReport struct {
	Version       int    `json:"version"`
	Status        string `json:"status"`
	PreviousFiles string `json:"previous_files,omitempty"`
	NextAction    string `json:"next_action"`
}

func newRecoveryRestoreCommand() *cobra.Command {
	var data, candidate string
	var apply bool
	command := &cobra.Command{
		Use:   "shrimp-recovery-restore",
		Short: "Preview or explicitly restore a backup matching the external clean checkpoint",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, err := recoveryRegistry()
			if err != nil {
				return err
			}
			result, err := restoreRecovery(data, candidate, root, apply, os.Rename)
			if err != nil {
				return err
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			return encoder.Encode(result)
		},
	}
	command.Flags().StringVar(&data, "data", "", "existing installation data directory")
	command.Flags().StringVar(&candidate, "from", "", "stopped byte-identical backup database, including its WAL/journal sidecars")
	command.Flags().BoolVar(&apply, "apply", false, "replace database files after verification; preserve previous files in a private archive")
	for _, flag := range []string{"data", "from"} {
		if err := command.MarkFlagRequired(flag); err != nil {
			panic(err)
		}
	}
	return command
}

// restoreRecovery never changes the external witness. A dirty witness cannot be
// repaired from an older checkpoint, even if the candidate itself is consistent.
func restoreRecovery(data, candidate, root string, apply bool, rename func(string, string) error) (*recoveryRestoreReport, error) {
	data, err := filepath.EvalSymlinks(data)
	if err != nil {
		return nil, errors.New("restore requires the existing installation directory")
	}
	data, err = filepath.Abs(data)
	if err != nil {
		return nil, err
	}
	unlockData, err := inspectRecoveryLock(filepath.Join(data, "pilot.lock"))
	if err != nil {
		return nil, errors.New("installation must be stopped with its original lock available")
	}
	defer unlockData()
	key := sha256.Sum256([]byte(data))
	base := filepath.Join(root, hex.EncodeToString(key[:]))
	unlockRegistry, err := inspectRecoveryLock(base + ".lock")
	if err != nil {
		return nil, errors.New("external recovery registry must be available and unlocked")
	}
	defer unlockRegistry()
	var binding, marker recoveryBinding
	var witness recoveryWitness
	if err := readRecoveryJSON(base+".enrolled", &binding); err != nil {
		return nil, errors.New("valid external enrollment is required")
	}
	if err := readRecoveryJSON(base+".json", &witness); err != nil {
		return nil, errors.New("valid external witness is required")
	}
	if binding.Version != 1 || binding.Data != data || filepath.Dir(binding.Database) != data ||
		!filepath.IsAbs(binding.Database) || filepath.Clean(binding.Database) != binding.Database || strings.ContainsAny(binding.Database, "?\x00") || witness.Binding != binding {
		return nil, errors.New("recovery installation identity mismatch")
	}
	if !witness.Clean {
		return nil, errors.New("uncertain witness: backup restore cannot authorize recovery")
	}
	markerPath := filepath.Join(data, ".shrimp-recovery")
	markerErr := readRecoveryJSON(markerPath, &marker)
	if markerErr != nil && !os.IsNotExist(markerErr) {
		return nil, errors.New("local enrollment evidence is invalid")
	}
	if markerErr == nil && marker != binding {
		return nil, errors.New("local enrollment identity mismatch")
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return nil, err
	}
	files, err := fingerprintRecoveryFiles(candidate, false)
	if err != nil {
		return nil, errors.New("candidate database files cannot be verified")
	}
	if files[""] == "absent" || !sameRecoveryFiles(files, witness.Files) {
		return nil, errors.New("candidate does not match the external clean checkpoint")
	}
	current, err := fingerprintRecoveryFiles(binding.Database, false)
	if err != nil {
		return nil, errors.New("current database files must be regular files or absent; preserve unexpected entries for review")
	}
	if info, err := os.Lstat(binding.Database + "-shm"); err == nil && !singleRecoveryFile(info) {
		return nil, errors.New("unexpected SQLite shared-memory file")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	report := &recoveryRestoreReport{Version: 1, Status: "verified_candidate",
		NextAction: "Review this preview, preserve external evidence, then repeat with --apply to replace database files. The witness will not be reset."}
	if sameRecoveryFiles(current, witness.Files) && markerErr == nil {
		report.Status = "already_current"
		report.NextAction = "Start normally with the same SHRIMP configuration; startup independently revalidates the checkpoint."
		return report, nil
	}
	if !apply {
		return report, nil
	}
	// Verify every staged byte before touching the current database.
	stage, err := os.MkdirTemp(data, ".shrimp-restore-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)
	stagedDatabase := filepath.Join(stage, "checkpoint.db")
	for _, suffix := range []string{"", "-wal", "-journal"} {
		if witness.Files[suffix] == "absent" {
			continue
		}
		if err := copyRecoveryFile(candidate+suffix, stagedDatabase+suffix); err != nil {
			return nil, err
		}
	}
	staged, err := fingerprintRecoveryFiles(stagedDatabase, true)
	if err != nil {
		return nil, err
	}
	if !sameRecoveryFiles(staged, witness.Files) {
		return nil, errors.New("candidate changed during staging; current database was not replaced")
	}
	if err := syncRecoveryDirectory(stage); err != nil {
		return nil, err
	}
	archive, err := os.MkdirTemp(data, ".shrimp-before-restore-")
	if err != nil {
		return nil, err
	}
	// Interrupted installation remains fenced by the unchanged checkpoint. Keep
	// originals and permit an explicit retry with the same verified candidate.
	for _, suffix := range []string{"", "-wal", "-journal", "-shm"} {
		path := binding.Database + suffix
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, err
		}
		if err := syncPreviousRecoveryFile(path); err != nil {
			return nil, err
		}
		if err := rename(path, filepath.Join(archive, filepath.Base(path))); err != nil {
			return nil, errors.Wrapf(err, "restore interrupted; previous files retained in %s", archive)
		}
	}
	if err := syncRecoveryDirectory(archive); err != nil {
		return nil, err
	}
	if err := syncRecoveryDirectory(data); err != nil {
		return nil, err
	}
	for _, suffix := range []string{"", "-wal", "-journal"} {
		if witness.Files[suffix] == "absent" {
			continue
		}
		if err := rename(stagedDatabase+suffix, binding.Database+suffix); err != nil {
			return nil, errors.Wrapf(err, "restore interrupted; previous files retained in %s", archive)
		}
	}
	if markerErr != nil {
		if err := durableJSON(markerPath, binding); err != nil {
			return nil, err
		}
	}
	installed, err := fingerprintRecoveryFiles(binding.Database, true)
	if err != nil {
		return nil, err
	}
	if !sameRecoveryFiles(installed, witness.Files) {
		return nil, errors.New("installed database failed checkpoint verification; keep the installation stopped")
	}
	if err := syncRecoveryDirectory(data); err != nil {
		return nil, err
	}
	report.Status = "restored"
	report.PreviousFiles = archive
	report.NextAction = "Start normally with the same SHRIMP configuration; startup independently revalidates the unchanged witness. Preserve the archived previous files for review."
	return report, nil
}

func copyRecoveryFile(source, destination string) error {
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !singleRecoveryFile(info) {
		return errors.New("candidate is not a regular file with one link")
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, file)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func syncPreviousRecoveryFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
