// Package scalar implements the legacy HTTP/JSON 0.2 scalar representation.
// It is not an advertised conformance profile and does not implement
// human-attributes-v1. Applications own storage, transactions, and enforcement.
package scalar

import (
	"errors"
	"unicode/utf8"
)

// Fact records an owned value and its revision. A nil Value is an explicit clear;
// an absent map entry means that no fact exists.
type Fact struct {
	Value     *string `json:"value"`
	Authority string  `json:"authority"`
	Revision  string  `json:"revision"`
}

// Changes sets or clears scalar facts. An empty value differs from a clear.
type Changes struct {
	Set   map[string]string
	Clear []string
}

// Validate checks supported names and rejects overlapping or duplicate changes.
// It permits empty changes and does not validate values or ownership.
func Validate(change Changes) error {
	seen := make(map[string]bool, len(change.Set)+len(change.Clear))
	for name := range change.Set {
		if !supported(name) {
			return errors.New("unsupported scalar attribute")
		}
		seen[name] = true
	}
	for _, name := range change.Clear {
		if !supported(name) || seen[name] {
			return errors.New("invalid scalar clear")
		}
		seen[name] = true
	}
	return nil
}

// Decode parses untyped command fields and validates their names. Successful
// results always contain nonnil Set and Clear, including for empty input.
func Decode(set map[string]any, clear []any) (Changes, error) {
	change := Changes{Set: make(map[string]string, len(set)), Clear: make([]string, 0, len(clear))}
	for name, input := range set {
		value, ok := input.(string)
		if !ok {
			return Changes{}, errors.New("scalar: value must be a string")
		}
		change.Set[name] = value
	}
	for _, input := range clear {
		name, ok := input.(string)
		if !ok {
			return Changes{}, errors.New("scalar: attribute name must be a string")
		}
		change.Clear = append(change.Clear, name)
	}
	if err := Validate(change); err != nil {
		return Changes{}, err
	}
	return change, nil
}

// Apply returns an independent copy with all changes applied or an error without
// modifying current. Changed facts must belong to authority; applications must
// resolve legacy blank owners before calling. Values remain exact UTF-8 strings
// of at most 1024 Unicode scalars. Callers supply a nonempty authority and revision
// and must commit the result with their native changes and operation receipt.
func Apply(current map[string]Fact, change Changes, authority, revision string) (map[string]Fact, error) {
	if authority == "" || revision == "" {
		return nil, errors.New("scalar: authority and revision are required")
	}
	if err := Validate(change); err != nil {
		return nil, err
	}
	for name, value := range change.Set {
		if !utf8.ValidString(value) || utf8.RuneCountInString(value) > 1024 {
			return nil, errors.New("scalar: invalid value")
		}
		if previous, ok := current[name]; ok && previous.Authority != authority {
			return nil, errors.New("scalar: attribute authority conflict")
		}
	}
	for _, name := range change.Clear {
		if previous, ok := current[name]; ok && previous.Authority != authority {
			return nil, errors.New("scalar: attribute authority conflict")
		}
	}
	result := make(map[string]Fact, len(current)+len(change.Set)+len(change.Clear))
	for name, fact := range current {
		if fact.Value != nil {
			value := *fact.Value
			fact.Value = &value
		}
		result[name] = fact
	}
	for name, value := range change.Set {
		result[name] = Fact{Value: &value, Authority: authority, Revision: revision}
	}
	for _, name := range change.Clear {
		result[name] = Fact{Authority: authority, Revision: revision}
	}
	return result, nil
}

func supported(name string) bool {
	return name == "displayName" || name == "department" || name == "email"
}
