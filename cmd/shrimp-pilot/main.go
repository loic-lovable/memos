// The shrimp-pilot command runs a disposable, single-process Memos backend over
// loopback TLS. It is intentionally separate from the production Memos launcher.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/pkg/errors"
	"golang.org/x/crypto/bcrypt"

	"github.com/usememos/memos/internal/profile"
	apiv1 "github.com/usememos/memos/server/api/v1"
	"github.com/usememos/memos/server/shrimp"
	"github.com/usememos/memos/store"
	"github.com/usememos/memos/store/db/sqlite"
)

type configuration struct {
	Shrimp        shrimp.Config `json:"shrimp"`
	Address       string        `json:"address"`
	Data          string        `json:"data"`
	Certificate   string        `json:"certificate"`
	Key           string        `json:"key"`
	SessionSecret string        `json:"session_secret"`
	AdminPassword string        `json:"admin_password"`
	ControlToken  string        `json:"control_token"`
}

func main() {
	path := flag.String("config", "", "private pilot configuration file")
	flag.Parse()
	if err := run(*path); err != nil {
		log.Fatal(err)
	}
}

func run(path string) error {
	configBytes, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var config configuration
	if err := json.Unmarshal(configBytes, &config); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(config.Address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("pilot listener must use a loopback IP")
	}
	if len(config.SessionSecret) < 32 || len(config.AdminPassword) < 20 || len(config.ControlToken) < 32 {
		return errors.New("pilot secrets must be randomly generated")
	}
	if err := os.MkdirAll(config.Data, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(config.Data, "pilot.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("another pilot process owns this database")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	p := &profile.Profile{Driver: "sqlite", DSN: filepath.Join(config.Data, "memos.db"), Data: config.Data, Version: "0.31.0", Addr: host, TrustedProxies: []string{"none"}}
	driver, err := sqlite.NewDB(p)
	if err != nil {
		return err
	}
	s := store.New(driver, p)
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		return err
	}
	handler, err := shrimp.New(ctx, s, config.Shrimp)
	if err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(config.AdminPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	if _, _, err := s.CreateUserIfNoUsers(ctx, &store.User{Username: "pilot-admin", Role: store.RoleAdmin, PasswordHash: string(hash)}); err != nil {
		return err
	}
	api := apiv1.NewAPIV1Service(config.SessionSecret, p, s)
	control := newControls(s, config.ControlToken)
	api.BeforeCredentialPublication = control.beforePublication
	handler.Fault = control.fault
	e := echo.New()
	if err := api.RegisterGateway(ctx, e); err != nil {
		return err
	}
	e.Any("/shrimp/*", echo.WrapHandler(handler))
	e.GET("/.well-known/oauth-protected-resource/*", echo.WrapHandler(handler))
	e.POST("/__pilot/control", echo.WrapHandler(control))
	e.GET("/healthz", func(c *echo.Context) error { return c.String(200, "pilot ready") })
	server := &http.Server{Addr: config.Address, Handler: admissionTransport(e), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 30 * time.Second}
	ended := make(chan error, 1)
	go func() { ended <- server.ListenAndServeTLS(config.Certificate, config.Key) }()
	select {
	case err := <-ended:
		if err != http.ErrServerClosed {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}

// Keep publication protected through net/http's final buffered write.
func admissionTransport(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, release := store.WithAdmissionScope(r.Context())
		defer release()
		next.ServeHTTP(w, r.WithContext(ctx))
		_ = http.NewResponseController(w).Flush()
	})
}
