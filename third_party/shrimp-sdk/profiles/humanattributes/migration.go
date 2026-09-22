package humanattributes

// Migrate calculates a compatibility-to-human-attributes-v1 transition. Call it
// inside the application's transaction after retained-operation recovery and
// checks of the human subject's current representation, lifecycle and revision,
// administrative migration approval and approval for each affected owned fact.
// It preserves owners and does not establish that any approval exists.
//
// A non-null legacy email requires a fresh emailEntryID and generation; absent or
// cleared email requires nil emailEntryID and does not call allocate. Commit the
// returned state, representation fence, subject revision and retained result
// atomically. No verification evidence is created or invalidated by this helper.
func (p *Profile) Migrate(current LegacyState, emailEntryID *string, revision string, allocate GenerationAllocator) (Result, error) {
	if !token(revision) {
		return Result{}, failure(ErrInvalidConfiguration, "", "revision required")
	}
	if err := p.Validate(Values{}); err != nil {
		return Result{}, err
	}
	for name, fact := range current.Facts {
		if name != string(DisplayName) && name != string(Department) && name != "email" {
			return Result{}, failure(ErrInvalidState, "", "unsupported legacy fact")
		}
		if !token(fact.Authority) || !token(fact.Revision) || (fact.Value != nil && !scalarString(*fact.Value)) {
			return Result{}, failure(ErrInvalidState, "", "invalid legacy fact")
		}
	}
	retained := State{EntryIDs: current.EntryIDs, Generations: current.Generations}
	if err := validateState(retained); err != nil {
		return Result{}, err
	}

	email, present := current.Facts["email"]
	if present && email.Revision == revision {
		return Result{}, failure(ErrInvalidConfiguration, Emails, "revision must change")
	}
	var requested []EmailInput
	if present && email.Value != nil {
		if emailEntryID == nil {
			return Result{}, failure(ErrInvalidValue, Emails, "migration entry required")
		}
		requested = []EmailInput{{EntryID: *emailEntryID, Value: *email.Value, Primary: true}}
		if err := p.Validate(Values{Emails: &requested}); err != nil {
			return Result{}, err
		}
	} else if emailEntryID != nil {
		return Result{}, failure(ErrInvalidValue, Emails, "migration entry must be absent")
	}

	next := cloneState(retained)
	if fact, exists := current.Facts[string(DisplayName)]; exists {
		next.Facts.DisplayName = &Fact[string]{Value: clonePointer(fact.Value), Authority: fact.Authority, Revision: fact.Revision}
	}
	if fact, exists := current.Facts[string(Department)]; exists {
		next.Facts.Department = &Fact[string]{Value: clonePointer(fact.Value), Authority: fact.Authority, Revision: fact.Revision}
	}
	if present {
		next.Facts.Emails = &Fact[[]Email]{Authority: email.Authority, Revision: revision}
		if requested != nil {
			entries, _, err := replaceEmails(&next, requested, allocate)
			if err != nil {
				return Result{}, err
			}
			next.Facts.Emails.Value = &entries
		}
	}
	return Result{State: next, Invalidated: []EmailVersion{}}, nil
}
