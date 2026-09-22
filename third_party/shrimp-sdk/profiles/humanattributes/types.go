// Package humanattributes supplies typed values and transitions for the
// human-attributes-v1 draft, including pure legacy migration calculations.
// It does not authorize requests, own storage, or enable a server profile.
// Applications commit its results with subject revisions, verification
// invalidations and retained operations.
package humanattributes

import "github.com/lovablelabs/shrimp-protocol/sdk/go/profiles/scalar"

// ID is the representation selector; importing this package does not enable it.
const ID = "human-attributes-v1"

// Field identifies one independently owned fact.
type Field string

const (
	DisplayName Field = "displayName"
	NameField   Field = "name"
	Department  Field = "department"
	Emails      Field = "emails"
	Locale      Field = "locale"
	Timezone    Field = "timezone"
)

// Name preserves supplied components without inferring or normalizing others.
type Name struct {
	Formatted  *string `json:"formatted,omitempty"`
	GivenName  *string `json:"givenName,omitempty"`
	FamilyName *string `json:"familyName,omitempty"`
	MiddleName *string `json:"middleName,omitempty"`
}

// EmailInput names a new entry with nil ExpectedGeneration, or an existing entry
// with its current generation. Type is absent or work, home, or other.
type EmailInput struct {
	EntryID            string  `json:"entry_id"`
	Value              string  `json:"value"`
	Type               *string `json:"type,omitempty"`
	Primary            bool    `json:"primary"`
	ExpectedGeneration *string `json:"expected_generation"`
}

// Email is an observed entry with a target-allocated generation.
type Email struct {
	EntryID    string  `json:"entry_id"`
	Value      string  `json:"value"`
	Type       *string `json:"type,omitempty"`
	Primary    bool    `json:"primary"`
	Generation string  `json:"generation"`
}

// Values contains supplied fields. Nil pointers mean omitted; an Emails pointer
// must point to a nonnil slice (an empty slice means an explicitly empty list).
type Values struct {
	DisplayName *string       `json:"displayName,omitempty"`
	Name        *Name         `json:"name,omitempty"`
	Department  *string       `json:"department,omitempty"`
	Emails      *[]EmailInput `json:"emails,omitempty"`
	Locale      *string       `json:"locale,omitempty"`
	Timezone    *string       `json:"timezone,omitempty"`
}

// Changes replaces complete fields or explicitly clears them.
type Changes struct {
	Set   Values  `json:"set"`
	Clear []Field `json:"clear"`
}

// Fact distinguishes an owned clear (nil Value) from an absent fact (nil Fact).
type Fact[T any] struct {
	Value     *T     `json:"value"`
	Authority string `json:"authority"`
	Revision  string `json:"revision"`
}

// Facts is the full typed attribute representation, independent of native fields.
type Facts struct {
	DisplayName *Fact[string]  `json:"displayName,omitempty"`
	Name        *Fact[Name]    `json:"name,omitempty"`
	Department  *Fact[string]  `json:"department,omitempty"`
	Emails      *Fact[[]Email] `json:"emails,omitempty"`
	Locale      *Fact[string]  `json:"locale,omitempty"`
	Timezone    *Fact[string]  `json:"timezone,omitempty"`
}

// State includes application-owned history for one subject. EntryIDs and
// Generations retain every committed identifier, including removed entries and
// superseded values. Persist them with Facts; never rebuild history from a read.
// This is a transition input, not a prescribed database representation.
type State struct {
	Facts       Facts           `json:"facts"`
	EntryIDs    map[string]bool `json:"entry_ids"`
	Generations map[string]bool `json:"generations"`
}

// LegacyState is the compatibility representation and any retained email
// identifier history for the same subject. Facts accepts only displayName,
// department and email. History must include identifiers retained across restore
// or earlier transitions; nil history is valid only when no identifiers existed.
// Applications must resolve missing legacy owners before migration.
type LegacyState struct {
	Facts       map[string]scalar.Fact `json:"facts"`
	EntryIDs    map[string]bool        `json:"entry_ids"`
	Generations map[string]bool        `json:"generations"`
}

// EmailVersion identifies a removed or replaced value whose verification
// assertions the application must invalidate in the same transaction.
type EmailVersion struct {
	EntryID    string
	Generation string
	Value      string
}

// Result is a proposed transition, not evidence of a commit or authorization.
type Result struct {
	State       State
	Invalidated []EmailVersion
}

// GenerationAllocator supplies a fresh target-owned generation. It must not
// publish other effects; unused allocations may be discarded on failure.
type GenerationAllocator func() (string, error)
