package humanattributes

import (
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/profiles/scalar"
	"github.com/stretchr/testify/require"
)

func TestMigratePreservesIndependentOwnersAndExactValues(t *testing.T) {
	t.Parallel()
	current := legacyMigrationState()
	before := copyLegacyMigrationState(current)
	key := "work"
	result, err := profile(t).Migrate(current, &key, "migration-1", counter())
	require.NoError(t, err)
	require.Equal(t, before, current)
	require.Equal(t, &Fact[string]{Value: pointer("  Maya 李  "), Authority: "directory", Revision: "name-1"}, result.State.Facts.DisplayName)
	require.Equal(t, &Fact[string]{Value: pointer(""), Authority: "hr", Revision: "department-1"}, result.State.Facts.Department)
	require.Equal(t, &Fact[[]Email]{Value: pointer([]Email{{EntryID: "work", Value: "Maya+HR@Example.COM", Primary: true, Generation: "g1"}}), Authority: "contact-owner", Revision: "migration-1"}, result.State.Facts.Emails)
	require.Nil(t, result.State.Facts.Name)
	require.Nil(t, result.State.Facts.Locale)
	require.Nil(t, result.State.Facts.Timezone)
	require.Empty(t, result.Invalidated)
	require.Equal(t, map[string]bool{"retired": true, "work": true}, result.State.EntryIDs)
	require.Equal(t, map[string]bool{"retired-generation": true, "g1": true}, result.State.Generations)

	*result.State.Facts.DisplayName.Value = "result mutation"
	delete(result.State.EntryIDs, "retired")
	delete(result.State.Generations, "retired-generation")
	require.Equal(t, before, current, "result aliases legacy state")
	*current.Facts["department"].Value = "input mutation"
	*current.Facts["email"].Value = "input@example.test"
	key = "changed-key"
	require.Equal(t, "", *result.State.Facts.Department.Value)
	require.Equal(t, "Maya+HR@Example.COM", (*result.State.Facts.Emails.Value)[0].Value)
	require.Equal(t, "work", (*result.State.Facts.Emails.Value)[0].EntryID)
}

func TestMigrateAbsentAndClearedEmailPreserveTheirMeanings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		cleared bool
	}{
		{name: "absent"},
		{name: "cleared", cleared: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			current := legacyMigrationState()
			current.Facts["displayName"] = scalar.Fact{Authority: "directory", Revision: "name-1"}
			delete(current.Facts, "email")
			if tc.cleared {
				current.Facts["email"] = scalar.Fact{Authority: "contact-owner", Revision: "email-1"}
			}
			before := copyLegacyMigrationState(current)
			result, err := profile(t).Migrate(current, nil, "migration-1", func() (string, error) {
				t.Fatal("absent or cleared email reached allocator")
				return "", nil
			})
			require.NoError(t, err)
			require.Equal(t, before, current)
			require.Equal(t, &Fact[string]{Authority: "directory", Revision: "name-1"}, result.State.Facts.DisplayName)
			require.Equal(t, "", *result.State.Facts.Department.Value)
			if tc.cleared {
				require.Equal(t, &Fact[[]Email]{Authority: "contact-owner", Revision: "migration-1"}, result.State.Facts.Emails)
			} else {
				require.Nil(t, result.State.Facts.Emails)
			}
			require.Equal(t, current.EntryIDs, result.State.EntryIDs)
			require.Equal(t, current.Generations, result.State.Generations)
			require.Empty(t, result.Invalidated)
			failed, err := profile(t).Migrate(current, pointer("work"), "migration-1", nil)
			require.ErrorIs(t, err, ErrInvalidValue)
			require.Equal(t, Result{}, failed)
		})
	}
	result, err := profile(t).Migrate(LegacyState{}, nil, "migration-1", nil)
	require.NoError(t, err)
	require.Equal(t, Facts{}, result.State.Facts)
	require.Empty(t, result.State.EntryIDs)
	require.Empty(t, result.State.Generations)
}

func TestMigrateFirstUseAllocatesWithNilHistory(t *testing.T) {
	t.Parallel()
	current := LegacyState{Facts: map[string]scalar.Fact{
		"email": {Value: pointer("first@example.test"), Authority: "contact-owner", Revision: "email-1"},
	}}
	result, err := profile(t).Migrate(current, pointer("work"), "migration-1", counter())
	require.NoError(t, err)
	require.Nil(t, current.EntryIDs)
	require.Nil(t, current.Generations)
	require.Equal(t, map[string]bool{"work": true}, result.State.EntryIDs)
	require.Equal(t, map[string]bool{"g1": true}, result.State.Generations)
	require.Equal(t, "first@example.test", (*result.State.Facts.Emails.Value)[0].Value)
}

func TestMigrateRetainedHistorySurvivesRestoreAndFencesReuse(t *testing.T) {
	t.Parallel()
	p := profile(t)
	current := legacyMigrationState()
	persisted, err := json.Marshal(current)
	require.NoError(t, err)
	var restored LegacyState
	require.NoError(t, json.Unmarshal(persisted, &restored))
	result, err := p.Migrate(restored, pointer("retired"), "migration-1", func() (string, error) {
		t.Fatal("retired entry key reached allocator")
		return "", nil
	})
	require.ErrorIs(t, err, ErrRevisionConflict)
	require.Equal(t, Result{}, result)
	result, err = p.Migrate(restored, pointer("new-entry"), "migration-1", func() (string, error) { return "retired-generation", nil })
	require.ErrorIs(t, err, ErrGenerationAllocation)
	require.Equal(t, Result{}, result)
	require.Equal(t, current, restored)
	result, err = p.Migrate(restored, pointer("new-entry"), "migration-1", counter())
	require.NoError(t, err)
	_, err = p.Update(result.State, Changes{Set: Values{Emails: pointer([]EmailInput{{EntryID: "retired", Value: "a@b"}})}}, "contact-owner", "next-1", counter())
	require.ErrorIs(t, err, ErrRevisionConflict)
}

func TestMigrateRejectsMalformedLegacyStateBeforeAllocation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		damage func(*LegacyState)
	}{
		{name: "unknown fact", damage: func(s *LegacyState) { s.Facts["private-field"] = s.Facts["email"] }},
		{name: "rich facts are not legacy", damage: func(s *LegacyState) { s.Facts["locale"] = s.Facts["department"] }},
		{name: "missing authority", damage: func(s *LegacyState) { f := s.Facts["department"]; f.Authority = ""; s.Facts["department"] = f }},
		{name: "missing revision", damage: func(s *LegacyState) { f := s.Facts["email"]; f.Revision = ""; s.Facts["email"] = f }},
		{name: "invalid authority", damage: func(s *LegacyState) { f := s.Facts["email"]; f.Authority = "\xff"; s.Facts["email"] = f }},
		{name: "overlong revision", damage: func(s *LegacyState) {
			f := s.Facts["displayName"]
			f.Revision = strings.Repeat("r", 1025)
			s.Facts["displayName"] = f
		}},
		{name: "invalid UTF-8", damage: func(s *LegacyState) { *s.Facts["displayName"].Value = "\xff" }},
		{name: "overlong scalar", damage: func(s *LegacyState) { *s.Facts["department"].Value = strings.Repeat("李", 1025) }},
		{name: "false entry history", damage: func(s *LegacyState) { s.EntryIDs["retired"] = false }},
		{name: "malformed entry history", damage: func(s *LegacyState) { s.EntryIDs["bad/id"] = true }},
		{name: "false generation history", damage: func(s *LegacyState) { s.Generations["retired-generation"] = false }},
		{name: "malformed generation history", damage: func(s *LegacyState) { s.Generations[""] = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			current := legacyMigrationState()
			tc.damage(&current)
			before := copyLegacyMigrationState(current)
			result, err := profile(t).Migrate(current, pointer("new-entry"), "migration-1", func() (string, error) {
				t.Fatal("malformed state reached allocator")
				return "", nil
			})
			require.ErrorIs(t, err, ErrInvalidState)
			require.Equal(t, Result{}, result)
			require.Equal(t, before, current)
			require.NotContains(t, err.Error(), "private-field")
			require.NotContains(t, err.Error(), "Maya+HR@Example.COM")
		})
	}
}

func TestMigrateRejectsUnrepresentableEmailAndInvalidEntrySelection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		value string
		key   *string
	}{
		{name: "missing key", value: "a@b"},
		{name: "empty key", value: "a@b", key: pointer("")},
		{name: "malformed key", value: "a@b", key: pointer("bad/id")},
		{name: "empty email", value: "", key: pointer("work")},
		{name: "no implicit trim", value: " a@b ", key: pointer("work")},
		{name: "non-ASCII mailbox", value: "李@example.test", key: pointer("work")},
		{name: "invalid mailbox", value: "a..b@example.test", key: pointer("work")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			current := legacyMigrationState()
			*current.Facts["email"].Value = tc.value
			before := copyLegacyMigrationState(current)
			result, err := profile(t).Migrate(current, tc.key, "migration-1", func() (string, error) {
				t.Fatal("invalid migration reached allocator")
				return "", nil
			})
			require.ErrorIs(t, err, ErrInvalidValue)
			require.Equal(t, Result{}, result)
			require.Equal(t, before, current)
			var detail *Error
			require.ErrorAs(t, err, &detail)
			require.Equal(t, Emails, detail.Field)
		})
	}
}

func TestMigrateAllocationFailureHasNoPartialResult(t *testing.T) {
	t.Parallel()
	cause := errors.New("private allocator details")
	for _, tc := range []struct {
		name     string
		allocate GenerationAllocator
		category error
	}{
		{name: "missing allocator", category: ErrInvalidConfiguration},
		{name: "failed allocation", allocate: func() (string, error) { return "unused-generation", cause }, category: ErrGenerationAllocation},
		{name: "empty generation", allocate: func() (string, error) { return "", nil }, category: ErrGenerationAllocation},
		{name: "invalid generation", allocate: func() (string, error) { return "\xff", nil }, category: ErrGenerationAllocation},
		{name: "overlong generation", allocate: func() (string, error) { return strings.Repeat("g", 1025), nil }, category: ErrGenerationAllocation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			current := legacyMigrationState()
			before := copyLegacyMigrationState(current)
			result, err := profile(t).Migrate(current, pointer("work"), "migration-1", tc.allocate)
			require.ErrorIs(t, err, tc.category)
			require.Equal(t, Result{}, result)
			require.Equal(t, before, current)
			require.NotContains(t, err.Error(), cause.Error())
			if tc.name == "failed allocation" {
				require.ErrorIs(t, err, cause)
			}
		})
	}
}

func TestMigrateRequiresConfiguredProfileAndFreshEmailRevision(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		profile  *Profile
		revision string
		clear    bool
	}{
		{name: "nil profile", revision: "migration-1"},
		{name: "zero profile", profile: &Profile{}, revision: "migration-1"},
		{name: "empty revision", profile: profile(t)},
		{name: "invalid revision", profile: profile(t), revision: "\xff"},
		{name: "reused email revision", profile: profile(t), revision: "email-1"},
		{name: "reused cleared email revision", profile: profile(t), revision: "email-1", clear: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			current := legacyMigrationState()
			key := pointer("work")
			if tc.clear {
				f := current.Facts["email"]
				f.Value = nil
				current.Facts["email"] = f
				key = nil
			}
			before := copyLegacyMigrationState(current)
			result, err := tc.profile.Migrate(current, key, tc.revision, func() (string, error) {
				t.Fatal("invalid configuration reached allocator")
				return "", nil
			})
			require.ErrorIs(t, err, ErrInvalidConfiguration)
			require.Equal(t, Result{}, result)
			require.Equal(t, before, current)
		})
	}
}

func legacyMigrationState() LegacyState {
	return LegacyState{
		Facts: map[string]scalar.Fact{
			"displayName": {Value: pointer("  Maya 李  "), Authority: "directory", Revision: "name-1"},
			"department":  {Value: pointer(""), Authority: "hr", Revision: "department-1"},
			"email":       {Value: pointer("Maya+HR@Example.COM"), Authority: "contact-owner", Revision: "email-1"},
		},
		EntryIDs:    map[string]bool{"retired": true},
		Generations: map[string]bool{"retired-generation": true},
	}
}

func copyLegacyMigrationState(current LegacyState) LegacyState {
	result := LegacyState{Facts: maps.Clone(current.Facts), EntryIDs: maps.Clone(current.EntryIDs), Generations: maps.Clone(current.Generations)}
	for name, fact := range result.Facts {
		fact.Value = clonePointer(fact.Value)
		result.Facts[name] = fact
	}
	return result
}
