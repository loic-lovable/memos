package humanattributes

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateStatePreservesPastCatalogAndLimitValues(t *testing.T) {
	t.Parallel()
	created, err := profile(t).Create(Values{
		Timezone: pointer("US/Eastern"),
		Emails: pointer([]EmailInput{
			{EntryID: "work", Value: "work@example.test", Primary: true},
			{EntryID: "home", Value: "home@example.test"},
		}),
	}, "owner", "r1", counter())
	require.NoError(t, err)
	before := cloneState(created.State)
	current, err := New(Config{MaxEmails: 1, TZDBVersion: "2026a", Timezones: []string{"UTC"}})
	require.NoError(t, err)
	require.NoError(t, ValidateState(created.State))
	require.Equal(t, before, created.State)
	// Current write policy rejects these values, but cannot make a historical
	// representation unreadable or prevent an unrelated conditional change.
	require.ErrorIs(t, current.Validate(Values{Timezone: pointer("US/Eastern")}), ErrInvalidValue)
	require.ErrorIs(t, current.Validate(Values{Emails: pointer([]EmailInput{
		{EntryID: "work", Value: "work@example.test"},
		{EntryID: "home", Value: "home@example.test"},
	})}), ErrLimitExceeded)
	changed, err := current.Update(created.State, Changes{Set: Values{Department: pointer("Research")}}, "owner", "r2", nil)
	require.NoError(t, err)
	require.Equal(t, created.State.Facts.Timezone, changed.State.Facts.Timezone)
	require.Equal(t, created.State.Facts.Emails, changed.State.Facts.Emails)
}

func TestValidateStateRejectsInvalidMetadataAndHistory(t *testing.T) {
	t.Parallel()
	created, err := profile(t).Create(Values{Emails: pointer([]EmailInput{{EntryID: "work", Value: "work@example.test"}})}, "owner", "r1", counter())
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		damage func(*State)
	}{
		{name: "missing owner", damage: func(s *State) { s.Facts.Emails.Authority = "" }},
		{name: "missing revision", damage: func(s *State) { s.Facts.Emails.Revision = "" }},
		{name: "missing entry history", damage: func(s *State) { s.EntryIDs = nil }},
		{name: "missing generation history", damage: func(s *State) { s.Generations = nil }},
		{name: "retired entry flag", damage: func(s *State) { s.EntryIDs["retired"] = false }},
		{name: "retired generation token", damage: func(s *State) { s.Generations[""] = true }},
		{name: "duplicate email entry", damage: func(s *State) { *s.Facts.Emails.Value = append(*s.Facts.Emails.Value, (*s.Facts.Emails.Value)[0]) }},
		{name: "malformed mailbox", damage: func(s *State) { (*s.Facts.Emails.Value)[0].Value = "invalid address" }},
		{name: "malformed timezone", damage: func(s *State) { s.Facts.Timezone = newFact("../UTC", "owner", "r1") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			state := cloneState(created.State)
			tc.damage(&state)
			// Preserve nil histories when comparing the untouched failed input.
			entries, generations := state.EntryIDs, state.Generations
			before := cloneState(state)
			if entries == nil {
				before.EntryIDs = nil
			}
			if generations == nil {
				before.Generations = nil
			}
			require.ErrorIs(t, ValidateState(state), ErrInvalidState)
			require.Equal(t, before, state)
		})
	}
}

func TestValidateStateAllowsEmptyFactsWithRetainedHistory(t *testing.T) {
	t.Parallel()
	require.NoError(t, ValidateState(State{}))
	require.NoError(t, ValidateState(State{
		EntryIDs:    map[string]bool{"retired": true},
		Generations: map[string]bool{"retired-generation": true},
	}))
}
