package server

import (
	"context"
	"errors"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/profiles/scalar"
)

// Application supplies durable operations. Implementations own all storage,
// transactions, enrollment, native account mapping and login enforcement.
// Methods must be safe for concurrent requests; New never enrolls an application.
type Application interface {
	Window(context.Context, string) (string, int64, error)
	// Apply atomically commits the account change, retained result and required
	// journal entries. It must coordinate with native writes and credential
	// publication. RecoverOnly must never admit new work.
	// A successful disable/retire must have blocked admission by Result.Time;
	// that completion evidence must be retained unchanged with the result.
	// Asynchronous admission blocking is unsupported by this bounded handler.
	// Return the documented application sentinels for known failures, optionally
	// wrapped with %w. Unknown errors must not imply a known rejection.
	Apply(context.Context, Mutation) (*Result, error)
	// Result returns immutable retained evidence, never reconstructed current state.
	Result(context.Context, string, string, string) (*Result, error)
	// Read resolves IDs across subject and source-reference namespaces. IDs must
	// be unambiguous across those namespaces; the handler checks the requested kind.
	// Return ErrNotFound for an absent record or ErrInvalidDependency for missing
	// causal evidence. Either sentinel may be wrapped with private context.
	Read(context.Context, string, []string) (*Subject, string, error)
	// Enumerate uses one coherent observation for candidate prefixes passed to fits.
	// It must retain continuation state through expiry and bind the entire selection.
	// Retrying a cursor starts at its original position, observing current records;
	// it may replace the page and successor but must not extend the traversal lease.
	Enumerate(context.Context, Enumeration, func(*EnumerationPage) (bool, error)) (*EnumerationPage, error)
	// ConsumeProof atomically refuses duplicate proof identifiers until expiry.
	ConsumeProof(context.Context, string, int64) error
	// BeginAudit returns a context carrying the application's durable attempt.
	// Apply must use that attempt in the same transaction as its retained result.
	BeginAudit(context.Context, string, string, string) (context.Context, error)
	IdentifyAudit(context.Context, string, string) error
	// FinishAudit preserves already journaled outcomes and unresolved attempts.
	// The response is withheld if this operation fails.
	FinishAudit(context.Context, AuditOutcome) error
}

// AuditOutcome describes a response, not proof that the account changed.
// Commit is not_committed only for a known rejection; an empty value is unknown.
type AuditOutcome struct{ Stage, Code, Commit string }

var (
	ErrNotFound     = errors.New("not found")
	ErrProofReplay  = errors.New("proof already consumed")
	ErrProofStorage = errors.New("proof replay storage unavailable")
	ErrWindowQuota  = errors.New("replay_window_quota")
	// ErrCapacity refuses new work without changing an existing operation result.
	ErrCapacity = errors.New("retained operation capacity")
)

// ScalarFact preserves an optional scalar with its own authority and revision.
// A nil Value means explicitly cleared, not an absent field.
type ScalarFact = scalar.Fact

// Subject is protocol identity without an application's native account identifier.
type Subject struct {
	ID, SourceID, SourceRevision, SourceReference, Revision, Lifecycle, DisplayName string
	Attributes                                                                      map[string]ScalarFact
}

// Mutation is validated single-account intent, including replay and authority checks
// that the application must enforce at its atomic commit boundary.
type Mutation struct {
	Principal, Window, ID, Fingerprint, Action, SubjectID, ExpectedRevision string
	SourceReference, DisplayName                                            string
	Authority                                                               string
	Set                                                                     map[string]string
	Clear                                                                   []string
	Deadline                                                                int64
	Dependencies                                                            []string
	CommandID                                                               string
	RecoverOnly                                                             bool
	// UnsupportedProfiles includes explicit and implied unsupported profile use.
	// Apply must reject new work only after retained lookup and intent equality.
	UnsupportedProfiles bool
}

// Result is immutable commit evidence retained atomically with the account change.
type Result struct {
	AuditAttempt             string
	Subject                  Subject
	Token                    string
	Time, RetainedUntil      int64
	Action, Error, CommandID string
}

// EnumerationError is a public failure code without private storage details.
type EnumerationError string

func (e EnumerationError) Error() string { return string(e) }

// Fixed bounds for this experimental extraction. The application must enforce
// these same bounds and the retention and quota declarations in discovery.
const (
	EnumerationMaxPage  = 100
	EnumerationMaxOpen  = 128
	EnumerationLifetime = 300
)

// Enumeration carries the normalized selection and current authorization context.
type Enumeration struct {
	Principal, Scope, Authorization, Epoch, Selection, Cursor string
	Types                                                     []string
	Visible                                                   bool
	PageSize                                                  int
	Dependencies                                              []string
}

// Record identifies a full direct record in the application's managed scope.
type Record struct {
	Type, ID string
	Subject  Subject
}

// EnumerationPage is one coherent observation; subsequent pages are not a snapshot.
type EnumerationPage struct {
	Records          []Record
	Frontier, Cursor string
	ExpiresAt        int64
}
