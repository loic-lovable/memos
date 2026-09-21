package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/labstack/echo/v5"
	"github.com/pkg/errors"

	"github.com/usememos/memos/server/shrimp"
	"github.com/usememos/memos/store"
)

// SHRIMP is opt-in on the normal Memos server. Private fault controls are
// compiled separately with the shrimptest build tag and require explicit setup.
type shrimpRuntime struct {
	tlsConfig *tls.Config
	release   func()
	testing   shrimpTestSettings
}

func (s *Server) configureShrimp(ctx context.Context) error {
	if s.Profile.ShrimpConfig == "" {
		if driver, ok := s.Store.GetDriver().(interface {
			ShrimpEnrolled(context.Context) (bool, error)
		}); ok {
			enrolled, err := driver.ShrimpEnrolled(ctx)
			if err != nil {
				return err
			}
			if enrolled {
				return errors.New("this database requires --shrimp-config to preserve account enforcement")
			}
		}
		return nil
	}
	if s.Profile.Demo || s.Profile.Driver != "sqlite" || s.Profile.UNIXSock != "" || net.ParseIP(s.Profile.Addr) == nil || !net.ParseIP(s.Profile.Addr).IsLoopback() {
		return errors.New("experimental SHRIMP requires a non-demo SQLite instance on a loopback IP")
	}
	release := func() {}
	if !s.Profile.ShrimpDataLockHeld {
		var err error
		release, err = lockShrimp(filepath.Join(s.Profile.Data, "pilot.lock"))
		if err != nil {
			return err
		}
	}
	ready := false
	defer func() {
		if !ready {
			release()
		}
	}()
	file, err := os.Open(s.Profile.ShrimpConfig)
	if err != nil {
		return err
	}
	defer file.Close()
	var config struct {
		shrimpTestSettings
		OperationalAuditToken     string        `json:"operational_audit_token,omitempty"`
		Shrimp                    shrimp.Config `json:"shrimp"`
		Certificate               string        `json:"certificate"`
		Key                       string        `json:"key"`
		DeploymentConfigDirectory string        `json:"deployment_config_directory"`
	}
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return errors.Wrap(err, "invalid SHRIMP configuration")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("SHRIMP configuration must contain exactly one JSON object")
	}
	certificate, err := tls.LoadX509KeyPair(config.Certificate, config.Key)
	if err != nil {
		return errors.Wrap(err, "invalid SHRIMP TLS certificate")
	}
	if config.DeploymentConfigDirectory != "" {
		if err := s.Store.LoadDeploymentConfigurationDir(ctx, config.DeploymentConfigDirectory); err != nil {
			return err
		}
	}
	if config.OperationalAuditToken != "" && len(config.OperationalAuditToken) < 32 {
		return errors.New("operational audit credential must be randomly generated with at least 32 characters")
	}
	if err := config.shrimpTestSettings.validateAuditAuthority(config.OperationalAuditToken); err != nil {
		return err
	}
	handler, err := shrimp.New(ctx, s.Store, config.Shrimp)
	if err != nil {
		return err
	}
	if err := config.shrimpTestSettings.configure(s, handler); err != nil {
		return err
	}
	if config.OperationalAuditToken != "" {
		s.echoServer.Any("/__shrimp/audit", echo.WrapHandler(handler.OperationalAuditHandler(config.OperationalAuditToken)))
	}
	s.echoServer.Any("/shrimp/*", echo.WrapHandler(handler))
	s.echoServer.GET("/.well-known/oauth-protected-resource/*", echo.WrapHandler(handler))
	s.shrimpRuntime = &shrimpRuntime{
		tlsConfig: &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}},
		release:   release,
		testing:   config.shrimpTestSettings,
	}
	ready = true
	return nil
}

func (s *Server) closeShrimp() {
	if s.shrimpRuntime != nil && s.shrimpRuntime.release != nil {
		s.shrimpRuntime.release()
		s.shrimpRuntime.release = nil
	}
}

// Keep admission protection through the response flush on the real transport.
func shrimpAdmissionTransport(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, release := store.WithAdmissionScope(r.Context())
		defer release()
		next.ServeHTTP(w, r.WithContext(ctx))
		_ = http.NewResponseController(w).Flush()
	})
}
