package client

import "fmt"

// UnavailableError means a check lacks a usable observation: transport, TLS
// trust, enrollment, current authority, or retained evidence may be unavailable.
// It must not become a protocol failure or a passing conformance result.
type UnavailableError struct{ Reason string }

func (e *UnavailableError) Error() string { return e.Reason }

func unavailable(format string, args ...any) error {
	return &UnavailableError{Reason: fmt.Sprintf(format, args...)}
}
