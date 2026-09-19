package sqlite

import (
	"encoding/hex"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/store"
)

func enumerationFixture(t *testing.T) (*store.Store, *DB, store.ShrimpEnumeration) {
	t.Helper()
	s, d := pilotStore(t)
	for range 3 {
		result, err := s.ApplyShrimp(t.Context(), pilotIntent(t, d, "create_subject", nil))
		require.NoError(t, err)
		require.Empty(t, result.Error)
	}
	return s, d, store.ShrimpEnumeration{Principal: "hr", Scope: "enrolled-resource", Authorization: "hr-authority",
		Epoch: "memos-pilot-1", Selection: "normalized-selection", Types: []string{"source_reference", "subject"}, Visible: true, PageSize: 2}
}

func enumerationFits(*store.ShrimpEnumerationPage) (bool, error) { return true, nil }

func TestShrimpEnumerationRestartRetryAndCurrentRecords(t *testing.T) {
	s, d, request := enumerationFixture(t)
	ctx := t.Context()
	first, err := d.EnumerateShrimp(ctx, request, enumerationFits)
	require.NoError(t, err)
	require.Len(t, first.Records, 2)
	require.NotEmpty(t, first.Cursor)
	require.GreaterOrEqual(t, first.ExpiresAt-time.Now().Unix(), int64(store.ShrimpEnumerationLifetime))
	request.Cursor = first.Cursor
	second, err := d.EnumerateShrimp(ctx, request, enumerationFits)
	require.NoError(t, err)
	require.Equal(t, first.ExpiresAt, second.ExpiresAt)
	// A repeated input cursor observes updated data without revisiting the
	// previously returned position. The retry replaces this page's old branch.
	subject := second.Records[1].Subject
	intent := pilotIntent(t, d, "update_subject", &subject)
	intent.DisplayName = "Changed between page attempts"
	updated, err := s.ApplyShrimp(ctx, intent)
	require.NoError(t, err)
	require.Empty(t, updated.Error)
	again, err := d.EnumerateShrimp(ctx, request, enumerationFits)
	require.NoError(t, err)
	require.Equal(t, updated.Subject.Revision, again.Records[1].Subject.Revision)
	require.Equal(t, updated.Token, again.Frontier)
	require.Equal(t, first.ExpiresAt, again.ExpiresAt)
	request.Cursor = again.Cursor
	// Close every connection, reopen the same database and recover its key/lease.
	require.NoError(t, s.Close())
	driver, err := NewDB(d.profile)
	require.NoError(t, err)
	reopened := driver.(*DB)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	require.NoError(t, reopened.ConfigureShrimp(ctx, "https://pilot.example/shrimp/v1/tenants/acme/domains/A"))
	last, err := reopened.EnumerateShrimp(ctx, request, enumerationFits)
	require.NoError(t, err)
	require.Empty(t, last.Cursor)
	require.Zero(t, last.ExpiresAt)
	lost, err := reopened.EnumerateShrimp(ctx, request, enumerationFits)
	require.NoError(t, err)
	require.Equal(t, last, lost, "lost terminal response must remain recoverable")
	keys := []string{}
	for _, page := range []*store.ShrimpEnumerationPage{first, again, last} {
		for _, record := range page.Records {
			keys = append(keys, record.Type+"/"+record.ID)
		}
	}
	require.Len(t, keys, 6)
	require.True(t, slices.IsSorted(keys))
	require.Len(t, slices.Compact(keys), 6)
	var count int
	require.NoError(t, reopened.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM shrimp_enumeration").Scan(&count))
	require.Equal(t, 1, count, "pages and retries consume one retained traversal slot")
}

func TestShrimpEnumerationRejectsChangedContextAndBrokenHistory(t *testing.T) {
	for _, name := range []string{"tamper", "principal", "scope", "authorization", "selection", "epoch", "expiry", "missing_state", "regression"} {
		t.Run(name, func(t *testing.T) {
			_, d, request := enumerationFixture(t)
			ctx := t.Context()
			first, err := d.EnumerateShrimp(ctx, request, enumerationFits)
			require.NoError(t, err)
			request.Cursor = first.Cursor
			code := "view_changed"
			switch name {
			case "tamper":
				request.Cursor += "x"
				code = "invalid_cursor"
			case "principal":
				request.Principal = "another-client"
				code = "cursor_scope_mismatch"
			case "scope":
				request.Scope = "another-enrollment"
				code = "cursor_scope_mismatch"
			case "authorization":
				request.Authorization = "new-visibility"
			case "selection":
				request.Selection = "different-page-size-or-view"
			case "epoch":
				request.Epoch = "replacement-history"
				code = "cursor_epoch_mismatch"
			case "expiry":
				var secret string
				require.NoError(t, d.db.QueryRowContext(ctx, "SELECT secret FROM shrimp_enumeration_key WHERE id=1").Scan(&secret))
				key, err := hex.DecodeString(secret)
				require.NoError(t, err)
				cursor, err := decodeShrimpCursor(key, request.Cursor)
				require.NoError(t, err)
				cursor.Expires = time.Now().Unix()
				request.Cursor, err = encodeShrimpCursor(key, cursor)
				require.NoError(t, err)
				code = "cursor_expired"
			case "missing_state":
				_, err = d.db.ExecContext(ctx, "DELETE FROM shrimp_enumeration")
				require.NoError(t, err)
			case "regression":
				_, err = d.db.ExecContext(ctx, "DELETE FROM shrimp_event WHERE id=?", first.Frontier)
				require.NoError(t, err)
				code = "consistency_unavailable"
			}
			page, err := d.EnumerateShrimp(ctx, request, enumerationFits)
			require.Nil(t, page)
			if name == "missing_state" {
				require.Error(t, err)
				var public store.ShrimpEnumerationError
				require.NotErrorAs(t, err, &public, "storage loss must map to unavailable")
			} else {
				require.ErrorIs(t, err, store.ShrimpEnumerationError(code))
			}
		})
	}
}

func TestShrimpEnumerationQuotaSizeAndRetainedDependencies(t *testing.T) {
	_, d, request := enumerationFixture(t)
	ctx := t.Context()
	var oldest string
	require.NoError(t, d.db.QueryRowContext(ctx, "SELECT id FROM shrimp_event ORDER BY sequence LIMIT 1").Scan(&oldest))
	request.Dependencies = []string{oldest}
	first, err := d.EnumerateShrimp(ctx, request, func(page *store.ShrimpEnumerationPage) (bool, error) {
		return len(page.Records) <= 1, nil
	})
	require.NoError(t, err)
	require.Len(t, first.Records, 1)
	require.NotEmpty(t, first.Cursor)
	_, err = d.db.ExecContext(ctx, "DELETE FROM shrimp_event WHERE id=?", oldest)
	require.NoError(t, err)
	request.Cursor = first.Cursor
	_, err = d.EnumerateShrimp(ctx, request, enumerationFits)
	require.NoError(t, err, "continuation retains the validated closure after original token collection")
	request.Cursor, request.Dependencies = "", nil
	_, err = d.EnumerateShrimp(ctx, request, func(*store.ShrimpEnumerationPage) (bool, error) { return false, nil })
	require.ErrorIs(t, err, store.ShrimpEnumerationError("limit_exceeded"))
	for range store.ShrimpEnumerationMaxOpen - 1 {
		_, err := d.EnumerateShrimp(ctx, request, enumerationFits)
		require.NoError(t, err)
	}
	_, err = d.EnumerateShrimp(ctx, request, enumerationFits)
	require.ErrorIs(t, err, store.ShrimpEnumerationError("throttled"))
	request.Cursor, request.Dependencies = first.Cursor, []string{oldest}
	_, err = d.EnumerateShrimp(ctx, request, enumerationFits)
	require.NoError(t, err, "quota must not evict existing continuations")
}

func TestShrimpEnumerationEmptyAndAuthorityFiltered(t *testing.T) {
	s, d := pilotStore(t)
	request := store.ShrimpEnumeration{Principal: "hr", Types: []string{"subject"}, Visible: true, PageSize: 2}
	page, err := d.EnumerateShrimp(t.Context(), request, enumerationFits)
	require.NoError(t, err)
	require.Empty(t, page.Records)
	require.Empty(t, page.Cursor)
	require.NotEmpty(t, page.Frontier)
	request.Dependencies = []string{page.Frontier}
	_, err = d.EnumerateShrimp(t.Context(), request, enumerationFits)
	require.NoError(t, err, "an empty observation must be a usable dependency")
	_, err = s.ApplyShrimp(t.Context(), pilotIntent(t, d, "create_subject", nil))
	require.NoError(t, err)
	page, err = d.EnumerateShrimp(t.Context(), request, enumerationFits)
	require.NoError(t, err, "genesis remains a usable dependency after the first mutation")
	require.Len(t, page.Records, 1)
	retired, err := s.ApplyShrimp(t.Context(), pilotIntent(t, d, "retire", &page.Records[0].Subject))
	require.NoError(t, err)
	require.Empty(t, retired.Error)
	page, err = d.EnumerateShrimp(t.Context(), request, enumerationFits)
	require.NoError(t, err)
	require.Len(t, page.Records, 1, "retired identities must remain enumerable")
	require.Equal(t, "retired", page.Records[0].Subject.Lifecycle)
	_, populated, selected := enumerationFixture(t)
	selected.Visible = false
	page, err = populated.EnumerateShrimp(t.Context(), selected, enumerationFits)
	require.NoError(t, err)
	require.Empty(t, page.Records)
	require.Empty(t, page.Cursor)
}

func TestShrimpEnumerationRejectsSlowPreparationWithoutPromisingCursor(t *testing.T) {
	_, d, request := enumerationFixture(t)
	delayed := false
	page, err := d.EnumerateShrimp(t.Context(), request, func(*store.ShrimpEnumerationPage) (bool, error) {
		if !delayed {
			time.Sleep(1100 * time.Millisecond)
			delayed = true
		}
		return true, nil
	})
	require.Nil(t, page)
	require.ErrorIs(t, err, store.ShrimpEnumerationError("consistency_unavailable"))
	var count int
	require.NoError(t, d.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM shrimp_enumeration").Scan(&count))
	require.Zero(t, count)
}
