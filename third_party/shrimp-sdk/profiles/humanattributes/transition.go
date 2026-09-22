package humanattributes

import (
	"errors"
	"maps"
)

// Create prepares initial facts for a new selected-profile subject. It does not
// adopt a legacy subject; representation migration requires separate authority.
func (p *Profile) Create(values Values, authority, revision string, allocate GenerationAllocator) (Result, error) {
	return p.apply(State{}, Changes{Set: values}, authority, revision, allocate, true)
}

// Update prepares an all-or-nothing change to one selected-profile subject.
// Call inside the application's transaction after authenticating/delegating the
// writer, checking the subject revision/lifecycle and recovering retained work.
// Commit State, all Invalidated effects and native changes with the receipt.
func (p *Profile) Update(current State, changes Changes, authority, revision string, allocate GenerationAllocator) (Result, error) {
	return p.apply(current, changes, authority, revision, allocate, false)
}

func fields(v Values) map[Field]bool {
	result := map[Field]bool{}
	for field, present := range map[Field]bool{DisplayName: v.DisplayName != nil, NameField: v.Name != nil,
		Department: v.Department != nil, Emails: v.Emails != nil, Locale: v.Locale != nil, Timezone: v.Timezone != nil} {
		if present {
			result[field] = true
		}
	}
	return result
}

func metadata[T any](fact *Fact[T]) (string, string) {
	if fact == nil {
		return "", ""
	}
	return fact.Authority, fact.Revision
}
func (f Facts) metadata(field Field) (string, string) {
	switch field {
	case DisplayName:
		return metadata(f.DisplayName)
	case NameField:
		return metadata(f.Name)
	case Department:
		return metadata(f.Department)
	case Emails:
		return metadata(f.Emails)
	case Locale:
		return metadata(f.Locale)
	case Timezone:
		return metadata(f.Timezone)
	default:
		return "", ""
	}
}
func known(field Field) bool {
	return field == DisplayName || field == NameField || field == Department || field == Emails || field == Locale || field == Timezone
}

func (p *Profile) apply(current State, changes Changes, authority, revision string, allocate GenerationAllocator, creating bool) (Result, error) {
	if !token(authority) || !token(revision) {
		return Result{}, failure(ErrInvalidConfiguration, "", "authority and revision required")
	}
	if err := p.Validate(changes.Set); err != nil {
		return Result{}, err
	}
	if err := validateState(current); err != nil {
		return Result{}, err
	}
	changed := fields(changes.Set)
	for _, field := range changes.Clear {
		if !known(field) || changed[field] {
			return Result{}, failure(ErrInvalidValue, "", "invalid clear")
		}
		changed[field] = true
	}
	if !creating && len(changed) == 0 {
		return Result{}, failure(ErrInvalidValue, "", "empty update")
	}
	for field := range changed {
		owner, oldRevision := current.Facts.metadata(field)
		if owner != "" && owner != authority {
			return Result{}, failure(ErrAuthorityConflict, field, "attribute authority conflict")
		}
		if oldRevision == revision {
			return Result{}, failure(ErrInvalidConfiguration, field, "revision must change")
		}
	}
	next := cloneState(current)
	result := Result{Invalidated: []EmailVersion{}}
	if changed[Emails] {
		requested := []EmailInput{}
		if changes.Set.Emails != nil {
			requested = *changes.Set.Emails
		}
		entries, invalidated, err := replaceEmails(&next, requested, allocate)
		if err != nil {
			return Result{}, err
		}
		result.Invalidated = invalidated
		next.Facts.Emails = &Fact[[]Email]{Value: &entries, Authority: authority, Revision: revision}
	}
	if v := changes.Set.DisplayName; v != nil {
		next.Facts.DisplayName = newFact(*v, authority, revision)
	}
	if v := changes.Set.Department; v != nil {
		next.Facts.Department = newFact(*v, authority, revision)
	}
	if v := changes.Set.Name; v != nil {
		next.Facts.Name = newFact(cloneName(*v), authority, revision)
	}
	if v := changes.Set.Locale; v != nil {
		next.Facts.Locale = newFact(*v, authority, revision)
	}
	if v := changes.Set.Timezone; v != nil {
		next.Facts.Timezone = newFact(*v, authority, revision)
	}
	for _, field := range changes.Clear {
		switch field {
		case DisplayName:
			next.Facts.DisplayName = &Fact[string]{Authority: authority, Revision: revision}
		case Department:
			next.Facts.Department = &Fact[string]{Authority: authority, Revision: revision}
		case NameField:
			next.Facts.Name = &Fact[Name]{Authority: authority, Revision: revision}
		case Emails:
			next.Facts.Emails.Value = nil
		case Locale:
			next.Facts.Locale = &Fact[string]{Authority: authority, Revision: revision}
		case Timezone:
			next.Facts.Timezone = &Fact[string]{Authority: authority, Revision: revision}
		}
	}
	result.State = next
	return result, nil
}

func replaceEmails(next *State, requested []EmailInput, allocate GenerationAllocator) ([]Email, []EmailVersion, error) {
	old := map[string]Email{}
	if next.Facts.Emails != nil && next.Facts.Emails.Value != nil {
		for _, e := range *next.Facts.Emails.Value {
			old[e.EntryID] = e
		}
	}
	// Check every precondition before requesting any new generations.
	for _, e := range requested {
		previous, exists := old[e.EntryID]
		if exists {
			if e.ExpectedGeneration == nil || *e.ExpectedGeneration != previous.Generation {
				return nil, nil, failure(ErrRevisionConflict, Emails, "email generation conflict")
			}
		} else if e.ExpectedGeneration != nil || next.EntryIDs[e.EntryID] {
			return nil, nil, failure(ErrRevisionConflict, Emails, "email entry unavailable")
		}
	}
	entries := make([]Email, 0, len(requested))
	preserved := map[string]bool{}
	for _, e := range requested {
		previous, exists := old[e.EntryID]
		generation := previous.Generation
		if !exists || previous.Value != e.Value {
			if allocate == nil {
				return nil, nil, failure(ErrInvalidConfiguration, Emails, "generation allocator required")
			}
			var err error
			generation, err = allocate()
			if err != nil {
				return nil, nil, failure(errors.Join(ErrGenerationAllocation, err), Emails, "generation allocation failed")
			}
			if !token(generation) || next.Generations[generation] {
				return nil, nil, failure(ErrGenerationAllocation, Emails, "generation allocation failed")
			}
			next.Generations[generation] = true
		} else {
			preserved[e.EntryID] = true
		}
		next.EntryIDs[e.EntryID] = true
		entries = append(entries, Email{EntryID: e.EntryID, Value: e.Value, Type: clonePointer(e.Type), Primary: e.Primary, Generation: generation})
	}
	invalidated := []EmailVersion{}
	// Preserve observation order rather than map iteration order.
	if next.Facts.Emails != nil && next.Facts.Emails.Value != nil {
		for _, e := range *next.Facts.Emails.Value {
			if !preserved[e.EntryID] {
				invalidated = append(invalidated, EmailVersion{EntryID: e.EntryID, Generation: e.Generation, Value: e.Value})
			}
		}
	}
	return entries, invalidated, nil
}

func newFact[T any](value T, authority, revision string) *Fact[T] {
	return &Fact[T]{Value: &value, Authority: authority, Revision: revision}
}
func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
func cloneName(n Name) Name {
	return Name{Formatted: clonePointer(n.Formatted), GivenName: clonePointer(n.GivenName), FamilyName: clonePointer(n.FamilyName), MiddleName: clonePointer(n.MiddleName)}
}
func cloneFact[T any](f *Fact[T]) *Fact[T] {
	result := clonePointer(f)
	if result != nil {
		result.Value = clonePointer(f.Value)
	}
	return result
}
func cloneState(s State) State {
	f := s.Facts
	f.DisplayName, f.Department = cloneFact(f.DisplayName), cloneFact(f.Department)
	f.Locale, f.Timezone = cloneFact(f.Locale), cloneFact(f.Timezone)
	f.Name, f.Emails = cloneFact(f.Name), cloneFact(f.Emails)
	if f.Name != nil && f.Name.Value != nil {
		*f.Name.Value = cloneName(*f.Name.Value)
	}
	if f.Emails != nil && f.Emails.Value != nil {
		entries := make([]Email, len(*f.Emails.Value))
		for i, entry := range *f.Emails.Value {
			entry.Type = clonePointer(entry.Type)
			entries[i] = entry
		}
		f.Emails.Value = &entries
	}
	return State{Facts: f, EntryIDs: maps.Clone(nonNil(s.EntryIDs)), Generations: maps.Clone(nonNil(s.Generations))}
}
func nonNil(m map[string]bool) map[string]bool {
	if m == nil {
		return map[string]bool{}
	}
	return m
}
