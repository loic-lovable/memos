package server

import "github.com/lovablelabs/shrimp-protocol/sdk/go/profiles/scalar"

// scalarChanges handles only the existing scalar representation. New profile
// selectors still pass through UnsupportedProfiles and cannot silently downgrade.
func scalarChanges(set map[string]any, clear []any) (map[string]string, []string, bool) {
	changes, err := scalar.Decode(set, clear)
	return changes.Set, changes.Clear, err == nil
}
