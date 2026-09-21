package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/internal/profile"
)

func recoveryRestoreFixture(t *testing.T) (*profile.Profile, string, string, string) {
	t.Helper()
	p, root := recoveryFixture(t)
	require.NoError(t, os.WriteFile(p.DSN+"-wal", []byte("current WAL"), 0600))
	g, err := beginRecovery(p, root, true)
	require.NoError(t, err)
	require.NoError(t, g.seal())
	g.close()
	backup := filepath.Join(t.TempDir(), "copy.db")
	for _, suffix := range []string{"", "-wal"} {
		require.NoError(t, copyRecoveryFile(p.DSN+suffix, backup+suffix))
	}
	require.NoError(t, os.WriteFile(p.DSN, []byte("old database"), 0600))
	require.NoError(t, os.WriteFile(p.DSN+"-wal", []byte("old WAL"), 0600))
	require.NoError(t, os.WriteFile(p.DSN+"-shm", []byte("old coordination"), 0600))
	return p, root, backup, g.witness
}

func TestRecoveryRestorePreviewAndApply(t *testing.T) {
	p, root, backup, witness := recoveryRestoreFixture(t)
	before := recoveryTree(t, filepath.Dir(p.Data))
	external := recoveryTree(t, root)
	report, err := restoreRecovery(p.Data, backup, root, false, os.Rename)
	require.NoError(t, err)
	require.Equal(t, "verified_candidate", report.Status)
	require.Equal(t, before, recoveryTree(t, filepath.Dir(p.Data)), "preview must not change anything")
	report, err = restoreRecovery(p.Data, backup, root, true, os.Rename)
	require.NoError(t, err)
	require.Equal(t, "restored", report.Status)
	require.Equal(t, external, recoveryTree(t, root), "restore cannot rewrite authority or witness")
	for suffix, value := range map[string]string{"": "old database", "-wal": "old WAL", "-shm": "old coordination"} {
		raw, err := os.ReadFile(filepath.Join(report.PreviousFiles, filepath.Base(p.DSN)+suffix))
		require.NoError(t, err)
		require.Equal(t, value, string(raw))
	}
	require.NoFileExists(t, p.DSN+"-shm")
	restored, err := fingerprintRecoveryFiles(p.DSN, false)
	require.NoError(t, err)
	var saved recoveryWitness
	require.NoError(t, readRecoveryJSON(witness, &saved))
	require.Equal(t, saved.Files, restored)
	stable := recoveryTree(t, filepath.Dir(p.Data))
	retry, err := restoreRecovery(p.Data, backup, root, true, os.Rename)
	require.NoError(t, err)
	require.Equal(t, "already_current", retry.Status)
	require.Equal(t, stable, recoveryTree(t, filepath.Dir(p.Data)))
	g, err := beginRecovery(p, root, false)
	require.NoError(t, err, "normal startup must independently accept the restored checkpoint")
	g.close()
}

func TestRecoveryRestoreRejectsUntrustedEvidenceWithoutWrites(t *testing.T) {
	for _, scenario := range []string{"dirty", "wrong_backup", "missing_wal", "missing_witness", "corrupt_witness", "wrong_marker", "active"} {
		t.Run(scenario, func(t *testing.T) {
			p, root, backup, witness := recoveryRestoreFixture(t)
			switch scenario {
			case "dirty":
				var saved recoveryWitness
				require.NoError(t, readRecoveryJSON(witness, &saved))
				saved.Clean = false
				require.NoError(t, durableJSON(witness, saved))
			case "wrong_backup":
				require.NoError(t, os.WriteFile(backup, []byte("older valid-looking backup"), 0600))
			case "missing_wal":
				require.NoError(t, os.Remove(backup+"-wal"))
			case "missing_witness":
				require.NoError(t, os.Remove(witness))
			case "corrupt_witness":
				require.NoError(t, os.WriteFile(witness, []byte("{"), 0600))
			case "wrong_marker":
				require.NoError(t, durableJSON(filepath.Join(p.Data, ".shrimp-recovery"), recoveryBinding{Version: 1, Data: "other"}))
			case "active":
				unlock, err := lockRecovery(filepath.Join(p.Data, "pilot.lock"))
				require.NoError(t, err)
				defer unlock()
			}
			before := recoveryTree(t, filepath.Dir(p.Data))
			_, err := restoreRecovery(p.Data, backup, root, true, os.Rename)
			require.Error(t, err)
			require.Equal(t, before, recoveryTree(t, filepath.Dir(p.Data)))
		})
	}
}

func TestRecoveryRestoreInterruptedInstallStaysFencedAndCanRetry(t *testing.T) {
	p, root, backup, _ := recoveryRestoreFixture(t)
	external := recoveryTree(t, root)
	calls := 0
	_, err := restoreRecovery(p.Data, backup, root, true, func(from, to string) error {
		calls++
		if calls == 5 {
			return errors.New("simulated failure after database install, before WAL install")
		}
		return os.Rename(from, to)
	})
	require.ErrorContains(t, err, "restore interrupted")
	require.Equal(t, external, recoveryTree(t, root))
	_, err = beginRecovery(p, root, false)
	require.ErrorContains(t, err, "differs", "partial replacement must never open the database")
	report, err := restoreRecovery(p.Data, backup, root, true, os.Rename)
	require.NoError(t, err)
	require.Equal(t, "restored", report.Status)
	require.Equal(t, external, recoveryTree(t, root))
	g, err := beginRecovery(p, root, false)
	require.NoError(t, err)
	g.close()
}

func TestRecoveryRestoreCanRecreateMissingLocalMarkerFromExternalBinding(t *testing.T) {
	p, root, backup, _ := recoveryRestoreFixture(t)
	require.NoError(t, os.Remove(filepath.Join(p.Data, ".shrimp-recovery")))
	report, err := restoreRecovery(p.Data, backup, root, true, os.Rename)
	require.NoError(t, err)
	require.Equal(t, "restored", report.Status)
	g, err := beginRecovery(p, root, false)
	require.NoError(t, err)
	g.close()
}

func TestRecoveryRestoreCommandDefaultsToPreview(t *testing.T) {
	command := newRecoveryRestoreCommand()
	require.False(t, command.Flags().Lookup("apply").Changed)
	apply, err := command.Flags().GetBool("apply")
	require.NoError(t, err)
	require.False(t, apply)
	command.SetArgs([]string{"--data", t.TempDir()})
	require.ErrorContains(t, command.Execute(), "required flag")
}
