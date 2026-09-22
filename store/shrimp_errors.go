package store

import "errors"

// SHRIMP request failures are classified before the transport adapter maps them
// to SDK errors. Retained ShrimpResult.Error values remain serialized wire codes.
var (
	// ErrShrimpReplayConflict rejects different intent under an existing operation ID.
	ErrShrimpReplayConflict = errors.New("replay_conflict")
	// ErrShrimpOperationResultUnavailable fences execution without recoverable evidence.
	ErrShrimpOperationResultUnavailable = errors.New("operation_result_unavailable")
	// ErrShrimpUnsupportedProfile rejects new work requiring an unsupported profile.
	ErrShrimpUnsupportedProfile = errors.New("unsupported_profile")
	// ErrShrimpInsufficientScope rejects new work on a recovery-only request.
	ErrShrimpInsufficientScope = errors.New("insufficient_scope")
	// ErrShrimpExecutionDeadlineExpired rejects execution before commit.
	ErrShrimpExecutionDeadlineExpired = errors.New("execution_deadline_expired")
	// ErrShrimpInvalidDependency identifies unavailable required causal evidence.
	ErrShrimpInvalidDependency = errors.New("invalid_dependency")
)
