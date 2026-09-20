//go:build shrimptest

package server

import (
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v5"
	"github.com/pkg/errors"

	apiv1 "github.com/usememos/memos/server/api/v1"
	"github.com/usememos/memos/server/shrimp"
)

// Test configuration is recognized only by explicitly tagged builds. The
// ordinary loopback/TLS/SQLite checks still apply before these controls mount.
type shrimpTestSettings struct {
	Test *struct {
		ControlToken string `json:"control_token"`
		AuditToken   string `json:"audit_token"`
	} `json:"test,omitempty"`
	controls *controls
}

func (t *shrimpTestSettings) configure(s *Server, handler *shrimp.Handler) error {
	if t.Test == nil {
		return nil
	}
	if len(t.Test.ControlToken) < 32 || len(t.Test.AuditToken) < 32 {
		return errors.New("test control credentials must be randomly generated")
	}
	if t.Test.ControlToken == t.Test.AuditToken {
		return errors.New("test audit credential must have separate authority")
	}
	t.controls = newControls(s.Store, t.Test.ControlToken)
	handler.Fault = t.controls.fault
	s.echoServer.POST("/__pilot/control", echo.WrapHandler(t.controls))
	s.echoServer.Any("/__pilot/audit", echo.WrapHandler(handler.OperationalAuditHandler(t.Test.AuditToken)))
	slog.Warn("SHRIMP fault-test controls enabled; this build is for disposable tests only")
	return nil
}

func (t *shrimpTestSettings) register(api *apiv1.APIV1Service) {
	if t.controls != nil {
		api.BeforeCredentialPublication = t.controls.beforePublication
	}
}

func (t *shrimpTestSettings) wrap(next http.Handler) http.Handler {
	if t.controls != nil {
		return t.controls.observeRequests(next)
	}
	return next
}
