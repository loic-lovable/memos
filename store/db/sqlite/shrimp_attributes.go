package sqlite

import (
	"unicode/utf8"

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
	seen := map[string]bool{}
	for name, value := range set {
		if !validShrimpScalar(name) || !utf8.ValidString(value) || utf8.RuneCountInString(value) > 1024 {
			return errShrimpConflict
		}
		if previous, ok := s.Attributes[name]; ok && previous.Authority != "" && previous.Authority != m.Authority {
			return errShrimpConflict
		}
		seen[name] = true
	}
	for _, name := range m.Clear {
		if !validShrimpScalar(name) || seen[name] {
			return errShrimpConflict
		}
		if previous, ok := s.Attributes[name]; ok && previous.Authority != "" && previous.Authority != m.Authority {
			return errShrimpConflict
		}
		seen[name] = true
	}
	for name, value := range set {
		v := value
		s.Attributes[name] = store.ShrimpScalarFact{Value: &v, Authority: m.Authority, Revision: revision}
	}
	for _, name := range m.Clear {
		s.Attributes[name] = store.ShrimpScalarFact{Authority: m.Authority, Revision: revision}
	}
	if fact, ok := s.Attributes["displayName"]; ok {
		s.DisplayName = ""
		if fact.Value != nil {
			s.DisplayName = *fact.Value
		}
	}
	return nil
}
func validShrimpScalar(name string) bool {
	return name == "displayName" || name == "department" || name == "email"
}
