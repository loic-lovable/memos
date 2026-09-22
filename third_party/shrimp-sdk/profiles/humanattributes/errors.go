package humanattributes

import "errors"

// These categories are stable for errors.Is. Error messages are diagnostic text,
// not an API or a wire error code. The application decides the public response.
var (
	// ErrInvalidConfiguration identifies missing or inconsistent application inputs.
	ErrInvalidConfiguration = errors.New("invalid profile configuration")
	// ErrInvalidValue identifies malformed values or collection invariants.
	ErrInvalidValue = errors.New("invalid attribute value")
	// ErrLimitExceeded identifies a collection or JSON fragment above its limit.
	ErrLimitExceeded = errors.New("attribute limit exceeded")
	// ErrAuthorityConflict identifies an attempt to change another owner's fact.
	ErrAuthorityConflict = errors.New("attribute authority conflict")
	// ErrRevisionConflict identifies stale email generations or unavailable entry IDs.
	// Re-read current state before preparing a new operation; do not blindly retry.
	ErrRevisionConflict = errors.New("attribute revision conflict")
	// ErrInvalidState identifies inconsistent application-owned facts or history.
	ErrInvalidState = errors.New("invalid stored attribute state")
	// ErrGenerationAllocation identifies a failed, malformed or reused allocation.
	ErrGenerationAllocation = errors.New("email generation allocation failed")
)

// Error adds a known field to an error category. Field is empty when no single
// known field identifies the failure. Inspect it with errors.As.
// Error never formats attribute values, supplied identifiers or allocator errors.
// Unwrap preserves categories and, when present, the allocator's original cause
// for internal handling. Applications must not serialize the error chain.
type Error struct {
	Field  Field
	detail string
	err    error
}

func (e *Error) Error() string { return "humanattributes: " + e.detail }
func (e *Error) Unwrap() error { return e.err }

func failure(kind error, field Field, detail string) error {
	return &Error{Field: field, detail: detail, err: kind}
}
