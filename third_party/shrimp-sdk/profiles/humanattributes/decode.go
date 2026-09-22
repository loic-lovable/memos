package humanattributes

// DecodeValues decodes one human-attributes-v1 attributes or set object. It
// rejects ambiguous JSON, unknown/exact-case mismatches, null field values and
// missing email members, then validates the values against this Profile.
// It does not decode an envelope, select a profile, authorize a request or check
// current-state preconditions. On any error it returns zero Values.
func (p *Profile) DecodeValues(raw []byte) (Values, error) {
	object, err := decodeObject(raw)
	if err != nil {
		return Values{}, err
	}
	return p.decodeValues(object)
}

// DecodeChanges decodes exactly {"set":{...},"clear":[...]}, requiring both
// members. It also rejects an empty update, duplicate/unknown clear fields and
// set/clear overlap. A caller extracting these members from a larger command must
// first strictly parse and validate that complete envelope; extraction cannot
// restore information lost by a permissive parser. Errors return zero Changes.
func (p *Profile) DecodeChanges(raw []byte) (Changes, error) {
	object, err := decodeObject(raw)
	if err != nil {
		return Changes{}, err
	}
	set, hasSet := object["set"].(map[string]any)
	clear, hasClear := object["clear"].([]any)
	if len(object) != 2 || !hasSet || !hasClear {
		return Changes{}, failure(ErrInvalidValue, "", "expected set and clear")
	}
	values, err := p.decodeValues(set)
	if err != nil {
		return Changes{}, err
	}
	changes := Changes{Set: values, Clear: make([]Field, 0, len(clear))}
	for _, entry := range clear {
		name, ok := entry.(string)
		if !ok {
			return Changes{}, failure(ErrInvalidValue, "", "invalid clear")
		}
		changes.Clear = append(changes.Clear, Field(name))
	}
	if _, err := changedFields(changes, false); err != nil {
		return Changes{}, err
	}
	return changes, nil
}

func (p *Profile) decodeValues(object map[string]any) (Values, error) {
	var values Values
	for name, value := range object {
		field := Field(name)
		switch field {
		case DisplayName, Department, Locale, Timezone:
			text, ok := value.(string)
			if !ok {
				return Values{}, failure(ErrInvalidValue, field, "expected string")
			}
			switch field {
			case DisplayName:
				values.DisplayName = &text
			case Department:
				values.Department = &text
			case Locale:
				values.Locale = &text
			case Timezone:
				values.Timezone = &text
			}
		case NameField:
			name, ok := decodeName(value)
			if !ok {
				return Values{}, failure(ErrInvalidValue, field, "invalid name members")
			}
			values.Name = &name
		case Emails:
			entries, ok := value.([]any)
			if !ok {
				return Values{}, failure(ErrInvalidValue, field, "expected email array")
			}
			emails := make([]EmailInput, 0, len(entries))
			for _, entry := range entries {
				email, ok := decodeEmail(entry)
				if !ok {
					return Values{}, failure(ErrInvalidValue, field, "invalid email members")
				}
				emails = append(emails, email)
			}
			values.Emails = &emails
		default:
			return Values{}, failure(ErrInvalidValue, "", "unknown attribute")
		}
	}
	if err := p.Validate(values); err != nil {
		return Values{}, err
	}
	return values, nil
}

func decodeName(value any) (Name, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return Name{}, false
	}
	var name Name
	for key, value := range object {
		text, ok := value.(string)
		if !ok {
			return Name{}, false
		}
		switch key {
		case "formatted":
			name.Formatted = &text
		case "givenName":
			name.GivenName = &text
		case "familyName":
			name.FamilyName = &text
		case "middleName":
			name.MiddleName = &text
		default:
			return Name{}, false
		}
	}
	return name, true
}

func decodeEmail(value any) (EmailInput, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return EmailInput{}, false
	}
	for key := range object {
		switch key {
		case "entry_id", "value", "primary", "expected_generation", "type":
		default:
			return EmailInput{}, false
		}
	}
	id, hasID := object["entry_id"].(string)
	address, hasValue := object["value"].(string)
	primary, hasPrimary := object["primary"].(bool)
	generation, hasGeneration := object["expected_generation"]
	if !hasID || !hasValue || !hasPrimary || !hasGeneration {
		return EmailInput{}, false
	}
	email := EmailInput{EntryID: id, Value: address, Primary: primary}
	if generation != nil {
		text, ok := generation.(string)
		if !ok {
			return EmailInput{}, false
		}
		email.ExpectedGeneration = &text
	}
	if kind, exists := object["type"]; exists {
		text, ok := kind.(string)
		if !ok {
			return EmailInput{}, false
		}
		email.Type = &text
	}
	return email, true
}
