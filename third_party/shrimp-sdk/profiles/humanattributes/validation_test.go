package humanattributes

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func pointer[T any](v T) *T { return &v }

func profile(t *testing.T) *Profile {
	t.Helper()
	p, err := New(Config{MaxEmails: 16, TZDBVersion: "2025b", Timezones: []string{"UTC", "Etc/UTC", "Europe/Stockholm", "US/Eastern", "Etc/GMT+5", "EST5EDT"}})
	require.NoError(t, err)
	return p
}

func TestLocaleSyntaxAndUniqueness(t *testing.T) {
	for _, tag := range []string{"en", "EN-us", "zh-Hant-TW", "es-419", "en-US-u-ca-gregory", "x-acme", "i-klingon", "EN-gb-OED", "de-CH-1901-a-extend1-x-private", "sl-rozaj-biske", "en-x-a-a"} {
		t.Run("valid/"+tag, func(t *testing.T) { require.True(t, languageTag(tag)) })
	}
	for _, tag := range []string{"", "en_US", "en-US,en;q=0.9", "en-a-foo-A-bar", "sl-rozaj-ROZAJ", "en--US", "en-x", "é", "en\n", "i-ami i-bnn", "*", strings.Repeat("a", 256)} {
		t.Run("invalid/"+tag, func(t *testing.T) { require.False(t, languageTag(tag)) })
	}
}

func TestPinnedCatalogAndExactValues(t *testing.T) {
	names := []string{"UTC", "US/Eastern", "Etc/GMT+5", "EST5EDT"}
	p, err := New(Config{MaxEmails: 2, TZDBVersion: "2025b", Timezones: names})
	require.NoError(t, err)
	names[0] = "Mars/Olympus"
	require.Equal(t, "2025b", p.TZDBVersion())
	require.Equal(t, 2, p.MaxEmails())
	for _, zone := range []string{"UTC", "US/Eastern", "Etc/GMT+5", "EST5EDT"} {
		require.NoError(t, p.Validate(Values{Timezone: &zone}))
	}
	for _, zone := range []string{"utc", "Europe/Stockholm", "Mars/Olympus", "+02:00", "GMT+02:00", "localtime", "../Etc/UTC", "", "UTC\n"} {
		require.Error(t, p.Validate(Values{Timezone: &zone}))
	}
	for _, config := range []Config{{}, {MaxEmails: 17, TZDBVersion: "2025b", Timezones: []string{"UTC"}}, {MaxEmails: 1, TZDBVersion: "host", Timezones: []string{"UTC"}}, {MaxEmails: 1, TZDBVersion: "2025b", Timezones: []string{"UTC", "UTC"}}, {MaxEmails: 1, TZDBVersion: "2025b", Timezones: []string{"../UTC"}}} {
		_, err := New(config)
		require.Error(t, err)
	}
	var zero Profile
	require.Error(t, zero.Validate(Values{}))
}

func TestTypedValuesRejectMalformedCollectionsAndNames(t *testing.T) {
	p := profile(t)
	valid := []Values{{}, {DisplayName: pointer(""), Department: pointer(""), Name: &Name{Formatted: pointer("")}},
		{DisplayName: pointer(strings.Repeat("é", 1024))}, {Emails: pointer([]EmailInput{})},
		{Emails: pointer([]EmailInput{{EntryID: "work", Value: "Maya+HR@Example.COM"}, {EntryID: "other", Value: "maya+HR@example.com"}})}}
	for _, values := range valid {
		require.NoError(t, p.Validate(values))
	}
	invalid := []Values{{Name: &Name{}}, {DisplayName: pointer("\xff")}, {Name: &Name{FamilyName: pointer(strings.Repeat("é", 1025))}},
		{Emails: pointer([]EmailInput(nil))}, {Emails: pointer([]EmailInput{{EntryID: "bad/id", Value: "a@b"}})},
		{Emails: pointer([]EmailInput{{EntryID: "a", Value: "a@b", ExpectedGeneration: pointer("")}})},
		{Emails: pointer([]EmailInput{{EntryID: "a", Value: "a@b", Type: pointer("")}})},
		{Emails: pointer([]EmailInput{{EntryID: "a", Value: "a@b"}, {EntryID: "a", Value: "b@b"}})},
		{Emails: pointer([]EmailInput{{EntryID: "a", Value: "a@b"}, {EntryID: "b", Value: "a@b"}})},
		{Emails: pointer([]EmailInput{{EntryID: "a", Value: "a@b", Primary: true}, {EntryID: "b", Value: "b@b", Primary: true}})}}
	for i, values := range invalid {
		require.Error(t, p.Validate(values), "case %d", i)
	}
	for _, address := range []string{"a..b@example.test", ".a@example.test", "a.@example.test", "a@-example.test", "a@example-.test", "a@example..test", "a@example.test\n", "a b@example.test", "é@example.test", `"a"@example.test`, "a@[127.0.0.1]", strings.Repeat("a", 65) + "@example.test", "a@" + strings.Repeat("a", 64)} {
		require.False(t, mailbox(address), "accepted %q", address)
	}
	require.True(t, mailbox(strings.Repeat("a", 64)+"@example.test"))
	require.True(t, mailbox("a@localhost"), "one DNS label is part of the profile contract")
	limited, err := New(Config{MaxEmails: 1, TZDBVersion: "2025b", Timezones: []string{"UTC"}})
	require.NoError(t, err)
	require.Error(t, limited.Validate(valid[len(valid)-1]))
}
