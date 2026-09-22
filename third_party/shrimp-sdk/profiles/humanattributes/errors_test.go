package humanattributes

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestErrorCategoriesSurviveWrapping(t *testing.T) {
	p := profile(t)
	created, err := p.Create(Values{DisplayName: pointer("private name"), Emails: pointer([]EmailInput{{EntryID: "work", Value: "private@example.test"}})}, "hr", "r1", counter())
	require.NoError(t, err)
	limited, err := New(Config{MaxEmails: 1, TZDBVersion: "2025b", Timezones: []string{"UTC"}})
	require.NoError(t, err)
	cases := []struct {
		name  string
		run   func() error
		kind  error
		field Field
	}{
		{"configuration", func() error { _, err := New(Config{}); return err }, ErrInvalidConfiguration, ""},
		{"invalid locale", func() error { return p.Validate(Values{Locale: pointer("invalid_locale")}) }, ErrInvalidValue, Locale},
		{"limit", func() error {
			return limited.Validate(Values{Emails: pointer([]EmailInput{{EntryID: "a", Value: "a@b"}, {EntryID: "b", Value: "b@b"}})})
		}, ErrLimitExceeded, Emails},
		{"ownership", func() error {
			_, err := p.Update(created.State, Changes{Clear: []Field{DisplayName}}, "other", "r2", nil)
			return err
		}, ErrAuthorityConflict, DisplayName},
		{"stale generation", func() error {
			_, err := p.Update(created.State, Changes{Set: Values{Emails: pointer([]EmailInput{{EntryID: "work", Value: "private@example.test", ExpectedGeneration: pointer("stale")}})}}, "hr", "r2", nil)
			return err
		}, ErrRevisionConflict, Emails},
		{"unavailable entry", func() error {
			_, err := p.Create(Values{Emails: pointer([]EmailInput{{EntryID: "work", Value: "private@example.test", ExpectedGeneration: pointer("g1")}})}, "hr", "r1", counter())
			return err
		}, ErrRevisionConflict, Emails},
		{"damaged history", func() error {
			damaged := cloneState(created.State)
			damaged.Generations = nil
			_, err := p.Update(damaged, Changes{Clear: []Field{DisplayName}}, "hr", "r2", nil)
			return err
		}, ErrInvalidState, Emails},
		{"invalid allocation", func() error {
			_, err := p.Create(Values{Emails: pointer([]EmailInput{{EntryID: "work", Value: "private@example.test"}})}, "hr", "r1", func() (string, error) { return "", nil })
			return err
		}, ErrGenerationAllocation, Emails},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			require.Error(t, err)
			wrapped := fmt.Errorf("prepare attributes: %w", err)
			require.ErrorIs(t, wrapped, tc.kind)
			var detail *Error
			require.ErrorAs(t, wrapped, &detail)
			require.Equal(t, tc.field, detail.Field)
			require.NotContains(t, wrapped.Error(), "private")
		})
	}
}

func TestAllocatorFailurePreservesCauseAndReturnsNoPartialState(t *testing.T) {
	p := profile(t)
	created, err := p.Create(Values{DisplayName: pointer("before"), Emails: pointer([]EmailInput{{EntryID: "work", Value: "a@b"}})}, "hr", "r1", counter())
	require.NoError(t, err)
	before := cloneState(created.State)
	changes := Changes{Set: Values{DisplayName: pointer("after"), Emails: pointer([]EmailInput{{EntryID: "new-a", Value: "b@b"}, {EntryID: "new-b", Value: "c@b"}})}}
	privateCause := errors.New("private allocator details")
	calls := 0
	result, err := p.Update(created.State, changes, "hr", "r2", func() (string, error) {
		calls++
		if calls == 1 {
			return "g2", nil
		}
		return "", privateCause
	})
	require.ErrorIs(t, err, ErrGenerationAllocation)
	require.ErrorIs(t, err, privateCause)
	require.NotContains(t, err.Error(), privateCause.Error())
	require.Equal(t, 2, calls)
	require.Equal(t, Result{}, result)
	require.Equal(t, before, created.State)
	// The first failed attempt did not consume entry IDs or publish generations.
	calls = 1
	result, err = p.Update(created.State, changes, "hr", "r2", func() (string, error) {
		calls++
		return fmt.Sprintf("g%d", calls), nil
	})
	require.NoError(t, err)
	require.Len(t, result.Invalidated, 1)
	require.Equal(t, "after", *result.State.Facts.DisplayName.Value)
	require.Equal(t, before, created.State)
}
