//go:build !shrimptest

package server

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/internal/profile"
)

func TestRegularBuildRejectsFaultConfiguration(t *testing.T) {
	for _, body := range []string{`{"test": {}}`, `{"test": null}`} {
		t.Run(body, func(t *testing.T) {
			directory := t.TempDir()
			path := filepath.Join(directory, "shrimp.json")
			require.NoError(t, os.WriteFile(path, []byte(body), 0600))
			s := &Server{
				Profile:    &profile.Profile{Data: directory, Driver: "sqlite", Addr: "127.0.0.1", ShrimpConfig: path},
				echoServer: echo.New(),
			}
			require.ErrorContains(t, s.configureShrimp(t.Context()), `unknown field "test"`)
		})
	}
}
