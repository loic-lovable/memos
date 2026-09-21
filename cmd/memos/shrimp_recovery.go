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

	"github.com/usememos/memos/internal/profile"
)

type recoveryBinding struct {
	Version  int    `json:"version"`
	Data     string `json:"data"`
	Database string `json:"database"`
}

type recoveryWitness struct {
	Binding recoveryBinding   `json:"binding"`
	Clean   bool              `json:"clean"`
	Files   map[string]string `json:"files"`
}

type recoveryGuard struct {
	binding recoveryBinding
	witness string
	release func()
}

func recoveryRegistry() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "memos", "shrimp-recovery"), nil
}

// beginRecovery runs before database open, even when SHRIMP configuration is omitted.
// Enrollment is explicit; losing evidence must never silently reenroll an installation.
func beginRecovery(p *profile.Profile, root string, enroll bool) (*recoveryGuard, error) {
	data, err := filepath.EvalSymlinks(p.Data)
	if err != nil {
		return nil, err
	}
	data, err = filepath.Abs(data)
	if err != nil {
		return nil, err
	}
	// Supported SQLite launches serialize before discovery, including the first
	// enrollment race with a launcher that has no SHRIMP flags.
	unlockData := func() {}
	if p.Driver == "sqlite" && recoveryLocksSupported {
		unlockData, err = lockRecovery(filepath.Join(data, "pilot.lock"))
		if err != nil {
			return nil, err
		}
		p.ShrimpDataLockHeld = true
	}
	keepLock := false
	defer func() {
		if !keepLock {
			unlockData()
		}
	}()
	key := sha256.Sum256([]byte(data))
	base := filepath.Join(root, hex.EncodeToString(key[:]))
	registration, witness := base+".enrolled", base+".json"
	marker := filepath.Join(data, ".shrimp-recovery")
	_, registeredErr := os.Stat(registration)
	_, markerErr := os.Lstat(marker)
	_, witnessErr := os.Lstat(witness)
	if os.IsNotExist(registeredErr) && os.IsNotExist(markerErr) && os.IsNotExist(witnessErr) && !enroll {
		keepLock = true
		return &recoveryGuard{release: unlockData}, nil
	}
	if witnessErr != nil && !os.IsNotExist(witnessErr) {
		return nil, witnessErr
	}
	if registeredErr != nil && !os.IsNotExist(registeredErr) {
		return nil, registeredErr
	}
	if markerErr != nil && !os.IsNotExist(markerErr) {
		return nil, markerErr
	}
	if p.Driver != "sqlite" || p.Demo || p.ShrimpConfig == "" {
		return nil, errors.New("recovery-guarded installation requires SQLite, non-demo mode and --shrimp-config")
	}
	if p.DSN == ":memory:" || strings.HasPrefix(p.DSN, "file:") {
		return nil, errors.New("recovery guard rejects SQLite URI and memory databases")
	}
	database, err := filepath.Abs(p.DSN)
	if err != nil {
		return nil, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(database))
	if err != nil {
		return nil, err
	}
	database = filepath.Join(parent, filepath.Base(database))
	p.DSN = database
	if parent != data || strings.ContainsAny(database, "?\x00") {
		return nil, errors.New("recovery guard requires an ordinary SQLite file directly inside the canonical data directory")
	}
	if info, err := os.Lstat(database); err == nil && !singleRecoveryFile(info) {
		return nil, errors.New("recovery database must be a regular file with one link")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if root == data || strings.HasPrefix(root, data+string(os.PathSeparator)) {
		return nil, errors.New("recovery evidence must be outside the data directory")
	}
	if err := durableRecoveryDirectory(root); err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	if root == data || strings.HasPrefix(root, data+string(os.PathSeparator)) {
		return nil, errors.New("recovery evidence resolves inside the data directory")
	}
	unlockRegistry, err := lockRecovery(base + ".lock")
	if err != nil {
		return nil, err
	}

	g := &recoveryGuard{binding: recoveryBinding{Version: 1, Data: data, Database: database}, witness: witness,
		release: func() { unlockRegistry(); unlockData() }}
	ready := false
	defer func() {
		if !ready {
			unlockRegistry()
		}
	}()
	// Recheck registration under the installation lock.
	raw, err := os.ReadFile(registration)
	if os.IsNotExist(err) {
		if !enroll || markerErr == nil {
			return nil, errors.New("recovery registration missing; operator recovery required")
		}
		if _, err := os.Stat(witness); !os.IsNotExist(err) {
			return nil, errors.New("orphan recovery witness; operator recovery required")
		}
		if err := durableJSON(registration, g.binding); err != nil {
			return nil, err
		}
		if err := durableJSON(marker, g.binding); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else {
		var binding recoveryBinding
		if err := json.Unmarshal(raw, &binding); err != nil || binding != g.binding {
			return nil, errors.New("recovery installation identity mismatch")
		}
		var saved recoveryWitness
		raw, err := os.ReadFile(witness)
		if err != nil {
			return nil, errors.Wrap(err, "recovery witness unavailable; operator recovery required")
		}
		if err := json.Unmarshal(raw, &saved); err != nil || saved.Binding != g.binding || !saved.Clean {
			return nil, errors.New("uncertain recovery state; operator recovery required")
		}
		files, err := recoveryFiles(database)
		if err != nil {
			return nil, err
		}
		if !sameRecoveryFiles(files, saved.Files) {
			return nil, errors.New("database differs from clean recovery checkpoint; startup refused")
		}
		markerData, err := os.ReadFile(marker)
		var bindingMarker recoveryBinding
		if err != nil || json.Unmarshal(markerData, &bindingMarker) != nil || bindingMarker != g.binding {
			return nil, errors.New("recovery enrollment marker missing or changed; startup refused")
		}
	}
	// Every database open can change state, including migration and startup errors.
	if err := durableJSON(witness, recoveryWitness{Binding: g.binding}); err != nil {
		return nil, err
	}
	p.ShrimpRecoveryGuard = true
	ready = true
	keepLock = true
	return g, nil
}

func (g *recoveryGuard) close() {
	if g != nil {
		g.release()
	}
}

func (g *recoveryGuard) seal() error {
	if g == nil || g.witness == "" {
		return nil
	}
	files, err := recoveryFiles(g.binding.Database)
	if err != nil {
		return err
	}
	if files[""] == "absent" {
		return errors.New("database missing at recovery checkpoint")
	}
	if err := syncRecoveryDirectory(filepath.Dir(g.binding.Database)); err != nil {
		return err
	}
	return durableJSON(g.witness, recoveryWitness{Binding: g.binding, Clean: true, Files: files})
}

func recoveryFiles(database string) (map[string]string, error) {
	files := make(map[string]string)
	for _, suffix := range []string{"", "-wal", "-journal"} {
		path := database + suffix
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			files[suffix] = "absent"
			continue
		}
		if err != nil {
			return nil, err
		}
		if !singleRecoveryFile(info) {
			return nil, errors.New("recovery checkpoint requires regular database files with one link")
		}
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return nil, err
		}
		hash := sha256.New()
		_, readErr := io.Copy(hash, file)
		syncErr := file.Sync()
		closeErr := file.Close()
		if readErr != nil {
			return nil, readErr
		}
		if syncErr != nil {
			return nil, syncErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		files[suffix] = hex.EncodeToString(hash.Sum(nil))
	}
	return files, nil
}

func sameRecoveryFiles(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func durableJSON(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".recovery-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(raw); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	return syncRecoveryDirectory(filepath.Dir(path))
}

func syncRecoveryDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Persist each new directory entry, not only files inside the final directory.
func durableRecoveryDirectory(path string) error {
	if info, err := os.Stat(path); err == nil {
		if !info.IsDir() {
			return errors.New("recovery registry is not a directory")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(path)
	if err := durableRecoveryDirectory(parent); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	return syncRecoveryDirectory(parent)
}
