package server

// scalarChanges handles only the existing scalar representation. New profile
// selectors still pass through UnsupportedProfiles and cannot silently downgrade.
func scalarChanges(set map[string]any, clear []any) (map[string]string, []string, bool) {
	values := make(map[string]string, len(set))
	removed := make([]string, 0, len(clear))
	seen := map[string]bool{}
	for name, input := range set {
		value, ok := input.(string)
		if !ok || !scalarField(name) {
			return nil, nil, false
		}
		values[name] = value
		seen[name] = true
	}
	for _, input := range clear {
		name, ok := input.(string)
		if !ok || !scalarField(name) || seen[name] {
			return nil, nil, false
		}
		seen[name] = true
		removed = append(removed, name)
	}
	return values, removed, true
}
func scalarField(name string) bool {
	return name == "displayName" || name == "department" || name == "email"
}
