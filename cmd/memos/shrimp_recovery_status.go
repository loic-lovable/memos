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

type recoveryStatus struct {
	Version            int    `json:"version"`
	Status             string `json:"status"`
	CheckpointMatches  *bool  `json:"checkpoint_matches"`
	RecoveryAuthorized bool   `json:"recovery_authorized"`
	NextAction         string `json:"next_action"`
}

func newRecoveryStatusCommand() *cobra.Command {
	var data, database string
	command := &cobra.Command{
		Use:   "shrimp-recovery-status",
		Short: "Inspect recovery evidence without opening SQLite or changing enrollment",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, err := recoveryRegistry()
			if err != nil {
				return err
			}
			result, err := inspectRecovery(data, database, root)
			if err != nil {
				return err
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			return encoder.Encode(result)
		},
	}
	command.Flags().StringVar(&data, "data", "", "existing installation data directory")
	command.Flags().StringVar(&database, "dsn", "", "ordinary SQLite file; defaults to memos_prod.db in data")
	if err := command.MarkFlagRequired("data"); err != nil {
		panic(err)
	}
	return command
}

func inspectRecovery(data, database, root string) (*recoveryStatus, error) {
	result := &recoveryStatus{Version: 1, NextAction: "Keep the installation stopped and preserve its database and external recovery evidence for review."}
	canonical, err := filepath.EvalSymlinks(data)
	if err != nil {
		return nil, errors.New("data directory unavailable; inspection creates no directories")
	}
	if info, err := os.Stat(canonical); err != nil || !info.IsDir() {
		return nil, errors.New("inspection requires an existing data directory")
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return nil, err
	}
	if database == "" {
		database = filepath.Join(canonical, "memos_prod.db")
	}
	if database == ":memory:" || strings.HasPrefix(database, "file:") || strings.ContainsAny(database, "?\x00") {
		return nil, errors.New("inspection requires an ordinary SQLite file")
	}
	database, err = filepath.Abs(database)
	if err != nil {
		return nil, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(database))
	if err != nil || parent != canonical {
		return nil, errors.New("database must be directly inside the data directory")
	}
	database = filepath.Join(parent, filepath.Base(database))
	expected := recoveryBinding{Version: 1, Data: canonical, Database: database}
	key := sha256.Sum256([]byte(canonical))
	base := filepath.Join(root, hex.EncodeToString(key[:]))
	paths := []string{base + ".enrolled", base + ".json", filepath.Join(canonical, ".shrimp-recovery")}
	present := 0
	for _, path := range paths {
		_, err := os.Lstat(path)
		if err == nil {
			present++
		} else if !os.IsNotExist(err) {
			return nil, errors.New("recovery evidence cannot be inspected")
		}
	}
	// No enrollment is inferred from absence. This status does not authorize startup.
	if present == 0 {
		result.Status = "no_evidence"
		result.NextAction = "Confirm the installation and OS-user configuration directory; absence is not proof that this database was never enrolled."
		return result, nil
	}
	// Open existing locks only: diagnostics must never create a replacement lock.
	unlockData, err := inspectRecoveryLock(filepath.Join(canonical, "pilot.lock"))
	if err != nil {
		result.Status = "lock_unavailable"
		result.NextAction = "Stop the installation normally if it is running; otherwise preserve the missing or unavailable lock for investigation."
		return result, nil
	}
	defer unlockData()
	unlockRegistry, err := inspectRecoveryLock(base + ".lock")
	if err != nil {
		result.Status = "lock_unavailable"
		return result, nil
	}
	defer unlockRegistry()
	var binding, marker recoveryBinding
	var witness recoveryWitness
	for i, target := range []any{&binding, &witness, &marker} {
		if err := readRecoveryJSON(paths[i], target); err != nil {
			if os.IsNotExist(err) {
				result.Status = "missing_evidence"
			} else {
				result.Status = "invalid_evidence"
			}
			return result, nil
		}
	}
	if binding != expected || marker != expected || witness.Binding != expected {
		result.Status = "identity_mismatch"
		return result, nil
	}
	if !witness.Clean {
		result.Status = "uncertain"
		result.NextAction = "Preserve all files. Independent evidence of post-checkpoint account and credential changes is required; reenrollment cannot authorize recovery."
		return result, nil
	}
	files, err := fingerprintRecoveryFiles(database, false)
	if err != nil {
		result.Status = "database_unavailable"
		return result, nil
	}
	matches := files[""] != "absent" && sameRecoveryFiles(files, witness.Files)
	result.CheckpointMatches = &matches
	if matches {
		result.Status = "checkpoint_match"
		result.NextAction = "Use the normal launcher with the same installation and SHRIMP configuration; it will independently revalidate before opening SQLite."
	} else {
		result.Status = "checkpoint_mismatch"
	}
	return result, nil
}

func readRecoveryJSON(path string, value any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !singleRecoveryFile(info) || info.Size() > 65536 {
		return errors.New("invalid recovery evidence file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 65537))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("invalid trailing recovery evidence")
	}
	return nil
}
