package humanattributes

func validFact[T any](f *Fact[T], valid func(T) bool) bool {
	return f == nil || (token(f.Authority) && token(f.Revision) && (f.Value == nil || valid(*f.Value)))
}

// Validate stored shape independently of today's limits/catalog. A release
// change must not erase an existing timezone or make unrelated edits impossible.
func validateState(s State) error {
	if !validFact(s.Facts.DisplayName, scalarString) || !validFact(s.Facts.Department, scalarString) ||
		!validFact(s.Facts.Name, validName) || !validFact(s.Facts.Locale, languageTag) || !validFact(s.Facts.Timezone, validZoneShape) ||
		!validFact(s.Facts.Emails, func(emails []Email) bool { return emails != nil && len(emails) <= 16 }) {
		return failure(ErrInvalidState, "", "invalid stored facts")
	}
	for id, used := range s.EntryIDs {
		if !used || !entryIDPattern.MatchString(id) {
			return failure(ErrInvalidState, Emails, "invalid entry history")
		}
	}
	for generation, used := range s.Generations {
		if !used || !token(generation) {
			return failure(ErrInvalidState, Emails, "invalid generation history")
		}
	}
	if s.Facts.Emails == nil || s.Facts.Emails.Value == nil {
		return nil
	}
	ids, values, generations := map[string]bool{}, map[string]bool{}, map[string]bool{}
	primaries := 0
	for _, email := range *s.Facts.Emails.Value {
		if !validEmail(email.EntryID, email.Value, email.Type) || !token(email.Generation) ||
			ids[email.EntryID] || values[email.Value] || generations[email.Generation] ||
			!s.EntryIDs[email.EntryID] || !s.Generations[email.Generation] {
			return failure(ErrInvalidState, Emails, "inconsistent stored email history")
		}
		ids[email.EntryID], values[email.Value], generations[email.Generation] = true, true, true
		if email.Primary {
			primaries++
		}
	}
	if primaries > 1 {
		return failure(ErrInvalidState, Emails, "invalid stored email preference")
	}
	return nil
}
