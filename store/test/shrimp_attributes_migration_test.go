package test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/usememos/memos/store"
)

func TestMigrationShrimpScalarFactsPreservesLegacyValueAndRevision(t *testing.T) {
	ctx := context.Background()
	ts := NewTestingStore(ctx, t)
	db := ts.GetDriver().GetDB()
	removeShrimpTypedSchema(ctx, t, ts)

	_, err := db.ExecContext(ctx, "ALTER TABLE shrimp_subject DROP COLUMN attributes")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO shrimp_subject(id,user_id,source_id,source_revision,source_reference,source_key,revision,lifecycle,display_name)
 VALUES('legacy-subject',99999,'legacy-source','source-r1','employee-legacy','source-hash','subject-r7','disabled','  Maya Å  ')`)
	require.NoError(t, err)
	setSchemaVersion(ctx, t, ts, "0.31.11")
	require.NoError(t, ts.Migrate(ctx))
	var raw, name, revision string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT attributes,display_name,revision FROM shrimp_subject WHERE id='legacy-subject'").Scan(&raw, &name, &revision))
	var facts map[string]store.ShrimpScalarFact
	require.NoError(t, json.Unmarshal([]byte(raw), &facts))
	require.Len(t, facts, 1)
	require.Equal(t, "  Maya Å  ", *facts["displayName"].Value)
	require.Equal(t, "subject-r7", facts["displayName"].Revision)
	require.Equal(t, "  Maya Å  ", name)
	require.Equal(t, "subject-r7", revision)
	// Re-running normal startup must not recreate or change the migrated facts.
	require.NoError(t, ts.Migrate(ctx))
	var again string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT attributes FROM shrimp_subject WHERE id='legacy-subject'").Scan(&again))
	require.Equal(t, raw, again)
}

// removeShrimpTypedSchema makes fixtures that rewind migration history match
// the older schema before applying migrations again.
func removeShrimpTypedSchema(ctx context.Context, t *testing.T, ts *store.Store) {
	t.Helper()
	for _, query := range []string{"ALTER TABLE shrimp_subject DROP COLUMN attribute_profile", "ALTER TABLE shrimp_subject DROP COLUMN human_attributes", "DROP TABLE shrimp_attribute_approval", "DROP TABLE shrimp_policy"} {
		_, err := ts.GetDriver().GetDB().ExecContext(ctx, query)
		require.NoError(t, err)
	}
}
