package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/internal/profile"
)

func recoveryFixture(t *testing.T) (*profile.Profile, string) {
	t.Helper()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	require.NoError(t, os.Mkdir(data, 0700))
	p := &profile.Profile{Data: data, DSN: filepath.Join(data, "memos_prod.db"), Driver: "sqlite", ShrimpConfig: "pilot.json"}
	require.NoError(t, os.WriteFile(p.DSN, []byte("current database"), 0600))
	return p, filepath.Join(root, "external")
}

func TestRecoveryCleanRestartAndRollbackRefusal(t *testing.T) {
	p, root := recoveryFixture(t)
	before, err := os.ReadFile(p.DSN)
	require.NoError(t, err)
	g, err := beginRecovery(p, root, true)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p.DSN, []byte("disabled account"), 0600))
	require.NoError(t, g.seal())
	g.close()
	g, err = beginRecovery(p, root, false)
	require.NoError(t, err)
	require.NoError(t, g.seal())
	g.close()
	require.NoError(t, os.WriteFile(p.DSN, before, 0600))
	_, err = beginRecovery(p, root, false)
	require.ErrorContains(t, err, "differs")
	// Enrollment cannot reset a stale checkpoint.
	_, err = beginRecovery(p, root, true)
	require.ErrorContains(t, err, "differs")
}

func TestRecoveryUncleanStopAndMissingEvidenceRefused(t *testing.T) {
	for _, state := range []string{"dirty", "missing", "corrupt"} {
		t.Run(state, func(t *testing.T) {
			p, root := recoveryFixture(t)
			g, err := beginRecovery(p, root, true)
			require.NoError(t, err)
			path := g.witness
			if state != "dirty" {
				require.NoError(t, g.seal())
			}
			g.close()
			if state == "missing" {
				require.NoError(t, os.Remove(path))
			}
			if state == "corrupt" {
				require.NoError(t, os.WriteFile(path, []byte("{"), 0600))
			}
			_, err = beginRecovery(p, root, true)
			require.Error(t, err, "reenrollment must never bless uncertainty")
		})
	}
}

func TestRecoveryOmittedConfigAndChangedDatabaseRefused(t *testing.T) {
	for _, change := range []string{"config", "dsn", "driver", "marker", "registration", "wal"} {
		t.Run(change, func(t *testing.T) {
			p, root := recoveryFixture(t)
			g, err := beginRecovery(p, root, true)
			require.NoError(t, err)
			require.NoError(t, g.seal())
			g.close()
			switch change {
			case "config":
				p.ShrimpConfig = ""
				require.NoError(t, os.Remove(filepath.Join(p.Data, ".shrimp-recovery")))
			case "dsn":
				p.DSN = filepath.Join(p.Data, "other.db")
			case "driver":
				p.Driver = "postgres"
			case "marker":
				require.NoError(t, os.Remove(filepath.Join(p.Data, ".shrimp-recovery")))
			case "registration":
				require.NoError(t, os.Remove(g.witness[:len(g.witness)-len(".json")]+".enrolled"))
			case "wal":
				require.NoError(t, os.WriteFile(p.DSN+"-wal", []byte("changed WAL"), 0600))
			}
			_, err = beginRecovery(p, root, false)
			require.Error(t, err)
		})
	}
}

func TestRecoveryLockPrecedesDatabaseUse(t *testing.T) {
	p, root := recoveryFixture(t)
	unlock, err := lockRecovery(filepath.Join(p.Data, "pilot.lock"))
	require.NoError(t, err)
	_, err = beginRecovery(p, root, true)
	require.ErrorContains(t, err, "another process")
	unlock()
	g, err := beginRecovery(p, root, true)
	require.NoError(t, err)
	defer g.close()
	_, err = beginRecovery(p, root, true)
	require.ErrorContains(t, err, "another process")
}

func TestRecoveryRejectsEvidenceInsideDataAndDatabaseSymlink(t *testing.T) {
	p, root := recoveryFixture(t)
	_, err := beginRecovery(p, filepath.Join(p.Data, "witness"), true)
	require.ErrorContains(t, err, "data directory")
	original := p.DSN
	p.DSN = filepath.Join(p.Data, "alias.db")
	require.NoError(t, os.Symlink(original, p.DSN))
	_, err = beginRecovery(p, root, true)
	require.ErrorContains(t, err, "regular file")
}

func TestRecoveryOrphanWitnessCannotDisableGuard(t *testing.T) {
	p, root := recoveryFixture(t)
	g, err := beginRecovery(p, root, true)
	require.NoError(t, err)
	require.NoError(t, g.seal())
	g.close()
	require.NoError(t, os.Remove(filepath.Join(p.Data, ".shrimp-recovery")))
	require.NoError(t, os.Remove(g.witness[:len(g.witness)-len(".json")]+".enrolled"))
	_, err = beginRecovery(p, root, false)
	require.ErrorContains(t, err, "registration missing")
}

func TestRecoveryRejectsSQLiteSpecialDSN(t *testing.T) {
	for _, dsn := range []string{":memory:", "file:other.db", "file::memory:?cache=shared"} {
		t.Run(dsn, func(t *testing.T) {
			p, root := recoveryFixture(t)
			p.DSN = dsn
			_, err := beginRecovery(p, root, true)
			require.ErrorContains(t, err, "URI and memory")
		})
	}
}

func TestRecoveryUnguardedLaunchStillSerializesEnrollment(t *testing.T) {
	p, root := recoveryFixture(t)
	ordinary, err := beginRecovery(p, root, false)
	require.NoError(t, err)
	require.False(t, p.ShrimpRecoveryGuard)
	_, err = beginRecovery(p, root, true)
	require.ErrorContains(t, err, "another process")
	require.NoError(t, ordinary.seal())
	ordinary.close()
	guarded, err := beginRecovery(p, root, true)
	require.NoError(t, err)
	guarded.close()
}

func TestRecoveryRejectsHardLinkedDatabase(t *testing.T) {
	p, root := recoveryFixture(t)
	require.NoError(t, os.Link(p.DSN, filepath.Join(p.Data, "alias.db")))
	_, err := beginRecovery(p, root, true)
	require.ErrorContains(t, err, "one link")
}
