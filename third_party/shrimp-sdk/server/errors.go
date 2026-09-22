package server

import "errors"

// Application errors are explicit, known outcomes, not arbitrary storage errors.
// Adapters may wrap them with %w; handlers use errors.Is and never publish the
// wrapped text. Return an unclassified error when the commit outcome is unknown.
var (
	// ErrReplayConflict means retained intent differs from this operation's intent.
	ErrReplayConflict = errors.New("replay_conflict")
	// ErrOperationResultUnavailable means the operation cannot be safely executed
	// or recovered from retained evidence. It does not prove it never committed.
	ErrOperationResultUnavailable = errors.New("operation_result_unavailable")
	// ErrUnsupportedProfile rejects new work after retained intent and recovery checks.
	ErrUnsupportedProfile = errors.New("unsupported_profile")
	// ErrInsufficientScope rejects new work after authorized recovery checks.
	// The handler distinguishes missing token scope from application authorization.
	ErrInsufficientScope = errors.New("insufficient_scope")
	// ErrExecutionDeadlineExpired means execution was refused before commit.
	ErrExecutionDeadlineExpired = errors.New("execution_deadline_expired")
	// ErrInvalidDependency means a read's required causal evidence is unavailable.
	ErrInvalidDependency = errors.New("invalid_dependency")
)

// legacyApplicationError preserves the initial extraction's exact error-string
// contract while adapters migrate to sentinels. It deliberately does not search
// substrings or unwrap old strings: neither was part of the old contract. Typed
// sentinels and all other errors retain their original wrapping and identity.
func legacyApplicationError(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{ErrReplayConflict, ErrOperationResultUnavailable,
		ErrUnsupportedProfile, ErrInsufficientScope, ErrExecutionDeadlineExpired, ErrInvalidDependency} {
		if errors.Is(err, known) {
			return err
		}
	}
	switch err.Error() {
	case "replay_conflict":
		return ErrReplayConflict
	case "operation_result_unavailable":
		return ErrOperationResultUnavailable
	case "unsupported_profile":
		return ErrUnsupportedProfile
	case "insufficient_scope":
		return ErrInsufficientScope
	case "execution_deadline_expired":
		return ErrExecutionDeadlineExpired
	case "invalid_dependency":
		return ErrInvalidDependency
	default:
		return err
	}
}
