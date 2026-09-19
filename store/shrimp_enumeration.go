package store

// ShrimpEnumerationError is a public enumeration failure without storage details.
type ShrimpEnumerationError string

func (e ShrimpEnumerationError) Error() string { return string(e) }

// Pilot enumeration bounds are shared with discovery and storage validation.
const (
	ShrimpEnumerationMaxPage  = 100
	ShrimpEnumerationMaxOpen  = 128
	ShrimpEnumerationLifetime = 300
)

// ShrimpEnumeration is a validated selection and current authorization context.
// Selection hashes the normalized wire selection, preserving omitted fields.
type ShrimpEnumeration struct {
	Principal, Scope, Authorization, Epoch, Selection, Cursor string
	Types                                                     []string
	Visible                                                   bool
	PageSize                                                  int
	Dependencies                                              []string
}

// ShrimpRecord identifies one full direct record in the pilot's managed scope.
type ShrimpRecord struct {
	Type, ID string
	Subject  ShrimpSubject
}

// ShrimpEnumerationPage is one coherent observation. A nonempty cursor indicates
// more records; it is not a promise of a frozen inventory across pages.
type ShrimpEnumerationPage struct {
	Records   []ShrimpRecord
	Frontier  string
	Cursor    string
	ExpiresAt int64
}
