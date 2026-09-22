package sqlite

import (
	"slices"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/profiles/scalar"

	"github.com/usememos/memos/store"
)

// applyShrimpAttributes runs inside the account transaction. Legacy rows are
// materialized at their currently observable revision before any unrelated edit.
func applyShrimpAttributes(s *store.ShrimpSubject, m store.ShrimpMutation, revision string) error {
	if s.Attributes == nil && m.Action == "create_subject" && m.Set != nil {
		s.Attributes = map[string]store.ShrimpScalarFact{}
	}
	if s.Attributes == nil {
		name := s.DisplayName
		s.Attributes = map[string]store.ShrimpScalarFact{"displayName": {Value: &name, Authority: m.Authority, Revision: s.Revision}}
	}
	if m.Action != "create_subject" && m.Action != "update_subject" {
		return nil
	}
	set := m.Set
	if set == nil {
		set = map[string]string{"displayName": m.DisplayName}
	}
	current := make(map[string]scalar.Fact, len(s.Attributes))
	for name, fact := range s.Attributes {
		authority := fact.Authority
		_, setting := set[name]
		if authority == "" && (setting || slices.Contains(m.Clear, name)) {
			// The legacy migration uses an empty owner for the fixed enrollment.
			// Resolve it only for changed facts; preserve omitted facts exactly.
			authority = m.Authority
		}
		current[name] = scalar.Fact{Value: fact.Value, Authority: authority, Revision: fact.Revision}
	}
	next, err := scalar.Apply(current, scalar.Changes{Set: set, Clear: m.Clear}, m.Authority, revision)
	if err != nil {
		return err
	}
	s.Attributes = make(map[string]store.ShrimpScalarFact, len(next))
	for name, fact := range next {
		s.Attributes[name] = store.ShrimpScalarFact{Value: fact.Value, Authority: fact.Authority, Revision: fact.Revision}
	}
	if fact, ok := s.Attributes["displayName"]; ok {
		s.DisplayName = ""
		if fact.Value != nil {
			s.DisplayName = *fact.Value
		}
	}
	return nil
}
