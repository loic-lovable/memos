package humanattributes

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Config is trusted application configuration. Timezones must enumerate all Zone
// and Link names (including backward aliases) from the pinned upstream release.
// New validates the configuration shape, not the provenance/completeness of that
// dataset. The application must load and verify its release artifact explicitly.
type Config struct {
	MaxEmails   int
	TZDBVersion string
	Timezones   []string
}

// Profile validates against immutable limits and an explicitly pinned catalog.
type Profile struct {
	maxEmails int
	version   string
	zones     map[string]bool
}

var (
	entryIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]{0,127}$`)
	zonePattern    = regexp.MustCompile(`^[A-Za-z0-9._+-]+(?:/[A-Za-z0-9._+-]+)*$`)
	versionPattern = regexp.MustCompile(`^[0-9]{4}[a-z]{1,12}$`)
	mailboxPattern = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+/=?^_`{|}~-]+(?:\\.[A-Za-z0-9!#$%&'*+/=?^_`{|}~-]+)*@[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?(?:\\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$")
	// These captures follow the profile's RFC 5646 syntax, not registry membership.
	languagePattern = regexp.MustCompile(`^(?:[a-z]{2,3}(?:-[a-z]{3}){0,3}|[a-z]{4}|[a-z]{5,8})(?:-[a-z]{4})?(?:-[a-z]{2}|-[0-9]{3})?((?:-[a-z0-9]{5,8}|-[0-9][a-z0-9]{3})*)((?:-[0-9a-wy-z](?:-[a-z0-9]{2,8})+)*)(?:-x(?:-[a-z0-9]{1,8})+)?$`)
	privatePattern  = regexp.MustCompile(`^x(?:-[a-z0-9]{1,8})+$`)
)

// New copies the catalog. It never consults the host's locale or time-zone data.
func New(config Config) (*Profile, error) {
	if config.MaxEmails < 1 || config.MaxEmails > 16 || !versionPattern.MatchString(config.TZDBVersion) || len(config.Timezones) == 0 {
		return nil, failure(ErrInvalidConfiguration, "", "invalid configuration")
	}
	p := &Profile{maxEmails: config.MaxEmails, version: config.TZDBVersion, zones: make(map[string]bool, len(config.Timezones))}
	for _, zone := range config.Timezones {
		if !validZoneShape(zone) || p.zones[zone] {
			return nil, failure(ErrInvalidConfiguration, Timezone, "invalid time-zone catalog")
		}
		p.zones[zone] = true
	}
	return p, nil
}

// MaxEmails returns the limit that a supporting application must advertise.
func (p *Profile) MaxEmails() int { return p.maxEmails }

// TZDBVersion returns the release that a supporting application must advertise.
func (p *Profile) TZDBVersion() string { return p.version }

func scalarString(s string) bool { return utf8.ValidString(s) && utf8.RuneCountInString(s) <= 1024 }
func token(s string) bool        { return s != "" && scalarString(s) }
func validZoneShape(s string) bool {
	if len(s) == 0 || len(s) > 255 || !zonePattern.MatchString(s) || s == "localtime" {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}
func mailbox(s string) bool {
	at := strings.IndexByte(s, '@')
	return len(s) <= 254 && at >= 1 && at <= 64 && mailboxPattern.MatchString(s)
}
func languageTag(s string) bool {
	if len(s) == 0 || len(s) > 255 {
		return false
	}
	for i := range s {
		if s[i] >= 128 {
			return false
		}
	}
	lower := strings.ToLower(s)
	const grandfathered = " en-gb-oed i-ami i-bnn i-default i-enochian i-hak i-klingon i-lux i-mingo i-navajo i-pwn i-tao i-tay i-tsu sgn-be-fr sgn-be-nl sgn-ch-de art-lojban cel-gaulish no-bok no-nyn zh-guoyu zh-hakka zh-min zh-min-nan zh-xiang "
	for _, tag := range strings.Fields(grandfathered) {
		if lower == tag {
			return true
		}
	}
	if privatePattern.MatchString(lower) {
		return true
	}
	match := languagePattern.FindStringSubmatch(lower)
	if match == nil {
		return false
	}
	for index, capture := range match[1:] {
		seen := map[string]bool{}
		for _, part := range strings.Split(strings.TrimPrefix(capture, "-"), "-") {
			if part == "" || (index == 1 && len(part) != 1) {
				continue
			}
			if seen[part] {
				return false
			}
			seen[part] = true
		}
	}
	return true
}

func validName(n Name) bool {
	found := false
	for _, value := range []*string{n.Formatted, n.GivenName, n.FamilyName, n.MiddleName} {
		if value != nil {
			found = true
			if !scalarString(*value) {
				return false
			}
		}
	}
	return found
}

func validEmail(id, value string, kind *string) bool {
	return entryIDPattern.MatchString(id) && mailbox(value) &&
		(kind == nil || *kind == "work" || *kind == "home" || *kind == "other")
}

// Validate checks typed write values and collection invariants. Raw JSON must
// separately pass strict parsing and the profile schema before conversion; a Go
// struct cannot detect unknown, duplicate or missing JSON object members.
func (p *Profile) Validate(values Values) error {
	if p == nil || p.zones == nil {
		return failure(ErrInvalidConfiguration, "", "unconfigured profile")
	}
	for _, field := range []Field{DisplayName, Department} {
		s := values.DisplayName
		if field == Department {
			s = values.Department
		}
		if s != nil && !scalarString(*s) {
			return failure(ErrInvalidValue, field, "invalid scalar")
		}
	}
	if values.Name != nil && !validName(*values.Name) {
		return failure(ErrInvalidValue, NameField, "invalid name")
	}
	if values.Locale != nil && !languageTag(*values.Locale) {
		return failure(ErrInvalidValue, Locale, "invalid locale")
	}
	if values.Timezone != nil && !p.zones[*values.Timezone] {
		return failure(ErrInvalidValue, Timezone, "unsupported timezone")
	}
	if values.Emails != nil {
		if *values.Emails == nil {
			return failure(ErrInvalidValue, Emails, "invalid email collection")
		}
		if len(*values.Emails) > p.maxEmails {
			return failure(ErrLimitExceeded, Emails, "email collection exceeds limit")
		}
		ids, addresses, primaries := map[string]bool{}, map[string]bool{}, 0
		for _, email := range *values.Emails {
			if !validEmail(email.EntryID, email.Value, email.Type) || ids[email.EntryID] || addresses[email.Value] ||
				(email.ExpectedGeneration != nil && !token(*email.ExpectedGeneration)) {
				return failure(ErrInvalidValue, Emails, "invalid email entry")
			}
			if email.Primary {
				primaries++
			}
			ids[email.EntryID], addresses[email.Value] = true, true
		}
		if primaries > 1 {
			return failure(ErrInvalidValue, Emails, "multiple primary emails")
		}
	}
	return nil
}
