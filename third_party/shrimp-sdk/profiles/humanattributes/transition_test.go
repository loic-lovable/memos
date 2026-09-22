package humanattributes

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func counter() GenerationAllocator {
	n := 0
	return func() (string, error) { n++; return fmt.Sprintf("g%d", n), nil }
}

func TestTransitionsPreserveGenerationsUntilAddressChanges(t *testing.T) {
	p, allocate := profile(t), counter()
	values := Values{DisplayName: pointer("  Maya Å  "), Name: &Name{GivenName: pointer("Maya")}, Department: pointer(""), Locale: pointer("EN-us"), Timezone: pointer("US/Eastern"),
		Emails: pointer([]EmailInput{{EntryID: "work", Value: "Maya+HR@Example.COM", Primary: true}, {EntryID: "home", Value: "maya@example.test"}})}
	created, err := p.Create(values, "hr", "r1", allocate)
	require.NoError(t, err)
	original := cloneState(created.State)
	entries := *created.State.Facts.Emails.Value
	metadata := []EmailInput{{EntryID: "home", Value: entries[1].Value, Primary: true, ExpectedGeneration: &entries[1].Generation}, {EntryID: "work", Value: entries[0].Value, Type: pointer("work"), ExpectedGeneration: &entries[0].Generation}}
	changed, err := p.Update(created.State, Changes{Set: Values{Emails: &metadata}}, "hr", "r2", nil)
	require.NoError(t, err)
	require.Empty(t, changed.Invalidated)
	require.Equal(t, "g2", (*changed.State.Facts.Emails.Value)[0].Generation)
	require.Equal(t, "g1", (*changed.State.Facts.Emails.Value)[1].Generation)
	require.Equal(t, original.Facts.Name, changed.State.Facts.Name)
	metadata[1].Value = "new@example.test"
	changed, err = p.Update(changed.State, Changes{Set: Values{Emails: &metadata}}, "hr", "r3", allocate)
	require.NoError(t, err)
	require.Equal(t, []EmailVersion{{EntryID: "work", Generation: "g1", Value: "Maya+HR@Example.COM"}}, changed.Invalidated)
	require.Equal(t, "g3", (*changed.State.Facts.Emails.Value)[1].Generation)
	metadata[1].Value, metadata[1].ExpectedGeneration = entries[0].Value, pointer("g3")
	changed, err = p.Update(changed.State, Changes{Set: Values{Emails: &metadata}}, "hr", "r4", allocate)
	require.NoError(t, err)
	require.Equal(t, "g4", (*changed.State.Facts.Emails.Value)[1].Generation)
	require.True(t, changed.State.Generations["g1"])
	require.Equal(t, original, created.State, "later transitions changed an earlier result")
	*changed.State.Facts.Name.Value.GivenName = "mutated result"
	*values.Name.GivenName = "mutated input"
	require.Equal(t, "Maya", *created.State.Facts.Name.Value.GivenName)
	*(*changed.State.Facts.Emails.Value)[1].Type = "home"
	require.Equal(t, "work", *metadata[1].Type)
}

func TestClearsEmptyCollectionsAndRetiredEntryHistory(t *testing.T) {
	p := profile(t)
	created, err := p.Create(Values{Emails: pointer([]EmailInput{{EntryID: "work", Value: "a@b"}})}, "hr", "r1", counter())
	require.NoError(t, err)
	for _, clear := range []bool{false, true} {
		t.Run(fmt.Sprint(clear), func(t *testing.T) {
			changes := Changes{Set: Values{Emails: pointer([]EmailInput{})}}
			if clear {
				changes = Changes{Clear: []Field{Emails}}
			}
			removed, err := p.Update(created.State, changes, "hr", "r2", nil)
			require.NoError(t, err)
			require.Equal(t, []EmailVersion{{EntryID: "work", Generation: "g1", Value: "a@b"}}, removed.Invalidated)
			raw, err := json.Marshal(removed.State.Facts)
			require.NoError(t, err)
			if clear {
				require.Contains(t, string(raw), `"value":null`)
			} else {
				require.Contains(t, string(raw), `"value":[]`)
			}
			require.True(t, removed.State.EntryIDs["work"])
			// Persist and restore history, then refuse reuse of a removed entry.
			persisted, err := json.Marshal(removed.State)
			require.NoError(t, err)
			var restored State
			require.NoError(t, json.Unmarshal(persisted, &restored))
			_, err = p.Update(restored, Changes{Set: Values{Emails: pointer([]EmailInput{{EntryID: "work", Value: "a@b"}})}}, "hr", "r3", counter())
			require.Error(t, err)
			_, err = p.Update(restored, Changes{Set: Values{Emails: pointer([]EmailInput{{EntryID: "new-key", Value: "a@b"}})}}, "hr", "r3", counter())
			require.Error(t, err, "allocator reused retired generation g1")
		})
	}
	absent, err := p.Create(Values{}, "hr", "r1", nil)
	require.NoError(t, err)
	raw, err := json.Marshal(absent.State.Facts)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(raw))
}

func TestInvalidChangesReturnNoPartialResult(t *testing.T) {
	p := profile(t)
	created, err := p.Create(Values{DisplayName: pointer("before"), Department: pointer("owned"), Emails: pointer([]EmailInput{{EntryID: "work", Value: "private@example.test"}})}, "hr", "r1", counter())
	require.NoError(t, err)
	created.State.Facts.Department.Authority = "other"
	for _, changes := range []Changes{
		{}, {Set: Values{DisplayName: pointer("new")}, Clear: []Field{DisplayName}}, {Clear: []Field{Emails, Emails}}, {Clear: []Field{"unknown"}},
		{Set: Values{DisplayName: pointer("new"), Department: pointer("unauthorized")}}, {Set: Values{DisplayName: pointer("new")}, Clear: []Field{Department}},
		{Set: Values{DisplayName: pointer("new"), Locale: pointer("en-a-foo-A-bar")}},
		{Set: Values{DisplayName: pointer("new"), Emails: pointer([]EmailInput{{EntryID: "work", Value: "new@example.test", ExpectedGeneration: pointer("stale")}})}},
	} {
		before := cloneState(created.State)
		result, err := p.Update(created.State, changes, "hr", "r2", func() (string, error) { t.Fatal("invalid request reached allocator"); return "", nil })
		require.Error(t, err)
		require.Equal(t, Result{}, result)
		require.Equal(t, before, created.State)
		require.NotContains(t, err.Error(), "private@example.test")
	}
	for _, allocate := range []GenerationAllocator{nil, func() (string, error) { return "g1", nil }, func() (string, error) { return "", errors.New("private allocator details") }} {
		before := cloneState(created.State)
		result, err := p.Update(created.State, Changes{Set: Values{DisplayName: pointer("new"), Emails: pointer([]EmailInput{{EntryID: "work", Value: "new@example.test", ExpectedGeneration: pointer("g1")}})}}, "hr", "r2", allocate)
		require.Error(t, err)
		require.Equal(t, Result{}, result)
		require.Equal(t, before, created.State)
		require.NotContains(t, err.Error(), "private allocator details")
	}
}

func TestCatalogUpgradePreservesExistingFactsAndAllowsClear(t *testing.T) {
	p := profile(t)
	created, err := p.Create(Values{Locale: pointer("EN-us"), Timezone: pointer("US/Eastern")}, "hr", "r1", nil)
	require.NoError(t, err)
	newProfile, err := New(Config{MaxEmails: 1, TZDBVersion: "2026a", Timezones: []string{"UTC"}})
	require.NoError(t, err)
	changed, err := newProfile.Update(created.State, Changes{Set: Values{DisplayName: pointer("new")}}, "hr", "r2", nil)
	require.NoError(t, err)
	require.Equal(t, created.State.Facts.Timezone, changed.State.Facts.Timezone)
	_, err = newProfile.Update(changed.State, Changes{Set: Values{Timezone: pointer("US/Eastern")}}, "hr", "r3", nil)
	require.Error(t, err)
	cleared, err := newProfile.Update(changed.State, Changes{Clear: []Field{Timezone}}, "hr", "r3", nil)
	require.NoError(t, err)
	require.Nil(t, cleared.State.Facts.Timezone.Value)
	require.Equal(t, "EN-us", *cleared.State.Facts.Locale.Value)
}

func TestInconsistentHistoryAndBlankOwnershipFailClosed(t *testing.T) {
	p := profile(t)
	created, err := p.Create(Values{Emails: pointer([]EmailInput{{EntryID: "work", Value: "a@b"}})}, "hr", "r1", counter())
	require.NoError(t, err)
	for _, damage := range []func(*State){func(s *State) { s.EntryIDs = nil }, func(s *State) { s.Generations = nil }, func(s *State) { s.Facts.Emails.Authority = "" }} {
		state := cloneState(created.State)
		damage(&state)
		_, err := p.Update(state, Changes{Set: Values{DisplayName: pointer("new")}}, "hr", "r2", nil)
		require.Error(t, err)
	}
	_, err = p.Update(created.State, Changes{Clear: []Field{Emails}}, "hr", "r1", nil)
	require.Error(t, err, "changed field reused its revision")
}
