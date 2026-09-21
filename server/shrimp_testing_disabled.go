//go:build !shrimptest

package server

import (
	"net/http"

	apiv1 "github.com/usememos/memos/server/api/v1"
	"github.com/usememos/memos/server/shrimp"
)

// No test configuration fields or control implementation exist in regular builds.
// The strict runtime decoder rejects a configuration containing "test".
type shrimpTestSettings struct{}

func (*shrimpTestSettings) configure(*Server, *shrimp.Handler) error { return nil }

func (*shrimpTestSettings) register(*apiv1.APIV1Service) {}

func (*shrimpTestSettings) wrap(next http.Handler) http.Handler { return next }

func (*shrimpTestSettings) validateAuditAuthority(string) error { return nil }
