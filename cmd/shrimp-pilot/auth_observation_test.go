package main

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/internal/profile"
	apiv1 "github.com/usememos/memos/server/api/v1"
	"github.com/usememos/memos/server/auth"
	"github.com/usememos/memos/store"
	"github.com/usememos/memos/store/db/sqlite"
)

func TestPrivateAuthenticationObservation(t *testing.T) {
	p := &profile.Profile{Driver: "sqlite", Data: t.TempDir(), Version: "0.31.0"}
	p.DSN = filepath.Join(p.Data, "memos.db")
	d, err := sqlite.NewDB(p)
	require.NoError(t, err)
	s := store.New(d, p)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ctx := t.Context()
	require.NoError(t, s.Migrate(ctx))
	require.NoError(t, s.EnableShrimpPilot(ctx, "https://pilot.example/shrimp/v1/tenants/acme/domains/A"))
	window, deadline, err := d.(store.ShrimpDriver).ShrimpWindow(ctx, "hr")
	require.NoError(t, err)
	created, err := s.ApplyShrimp(ctx, store.ShrimpMutation{Principal: "hr", Window: window, ID: "create", Fingerprint: "create", Action: "create_subject", SourceReference: "auth-fixture", DisplayName: "Pilot", Deadline: deadline - 1, CommandID: "c1"})
	require.NoError(t, err)
	user, err := s.GetUser(ctx, &store.FindUser{ID: &created.Subject.UserID})
	require.NoError(t, err)
	require.Equal(t, store.Archived, user.RowStatus)
	other, err := s.CreateUser(ctx, &store.User{Username: "other", Role: store.RoleUser, RowStatus: store.Archived})
	require.NoError(t, err)
	secret := "private-test-signing-secret"
	valid, _, err := auth.GenerateAccessTokenV2(user.ID, user.Username, "USER", "NORMAL", []byte(secret))
	require.NoError(t, err)
	wrongUser, _, err := auth.GenerateAccessTokenV2(other.ID, other.Username, "USER", "NORMAL", []byte(secret))
	require.NoError(t, err)
	wrongSignature, _, err := auth.GenerateAccessTokenV2(user.ID, user.Username, "USER", "NORMAL", []byte("wrong-secret"))
	require.NoError(t, err)
	claims, err := auth.ParseAccessTokenV2(valid, []byte(secret))
	require.NoError(t, err)
	claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
	expiredToken := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	expiredToken.Header["kid"] = auth.KeyID
	expired, err := expiredToken.SignedString([]byte(secret))
	require.NoError(t, err)
	api := apiv1.NewAPIV1Service(secret, p, s)
	e := echo.New()
	require.NoError(t, api.RegisterGateway(ctx, e))
	c := newControls(s, "private-control-secret")
	handler := admissionTransport(c.observeRequests(e))
	path := "/api/v1/users/" + user.Username + "/personalAccessTokens"
	arm := func() string {
		value, err := c.armAuth(ctx, created.Subject.ID, "pat")
		require.NoError(t, err)
		return value.(map[string]any)["handle"].(string)
	}
	inspect := func(handle, outcome string) {
		value, err := c.readAuth(handle)
		require.NoError(t, err)
		require.Equal(t, map[string]any{"handle": handle, "subject": created.Subject.ID, "kind": "pat", "outcome": outcome}, value)
	}
	request := func(token, handle, target string, change func(*http.Request)) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "https://pilot.example"+target, strings.NewReader(`{"description":"control","expiresInDays":1}`))
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		if handle != "" {
			r.Header.Set(observationHeader, handle)
		}
		if change != nil {
			change(r)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, test := range []struct {
		name, token, target, outcome string
		change                       func(*http.Request)
	}{
		{name: "verified archived account", token: valid, outcome: "archived_access_token"},
		{name: "malformed token", token: "invalid"},
		{name: "expired token", token: expired},
		{name: "wrong signing key", token: wrongSignature},
		{name: "missing credential"},
		{name: "different archived user", token: wrongUser},
		{name: "wrong route", token: valid, target: "/api/v1/users/other/personalAccessTokens"},
		{name: "query mismatch", token: valid, target: path + "?x=1"},
		{name: "wrong method", token: valid, change: func(r *http.Request) { r.Method = http.MethodGet }},
		{name: "cookie ambiguity", token: valid, change: func(r *http.Request) { r.Header.Set("Cookie", "memos_refresh=invalid") }},
		{name: "duplicate authorization", token: valid, change: func(r *http.Request) { r.Header.Add("Authorization", "Bearer invalid") }},
		{name: "duplicate correlation", token: valid, change: func(r *http.Request) { r.Header.Add(observationHeader, "other") }},
		{name: "no TLS", token: valid, change: func(r *http.Request) { r.TLS = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			target, outcome := test.target, test.outcome
			if target == "" {
				target = path
			}
			if outcome == "" {
				outcome = "unavailable"
			}
			handle := arm()
			baseline := request(test.token, "", target, test.change)
			observed := request(test.token, handle, target, test.change)
			require.Equal(t, baseline.Code, observed.Code)
			require.Equal(t, baseline.Body.String(), observed.Body.String(), "public body changed")
			require.Equal(t, baseline.Header(), observed.Header(), "public headers changed")
			require.NotContains(t, observed.Body.String(), "archived")
			require.Empty(t, observed.Header().Get(observationHeader))
			if outcome == "archived_access_token" {
				require.Equal(t, 401, observed.Code)
				var body map[string]any
				require.NoError(t, json.Unmarshal(observed.Body.Bytes(), &body))
				require.EqualValues(t, 16, body["code"])
			}
			inspect(handle, outcome)
			inspect(handle, "unavailable")
		})
	}
	t.Run("request replay invalidates observation", func(t *testing.T) {
		handle := arm()
		request(valid, handle, path, nil)
		request("invalid", handle, path, nil)
		inspect(handle, "unavailable")
	})
	t.Run("duplicate headers invalidate an earlier observation", func(t *testing.T) {
		handle := arm()
		request(valid, handle, path, nil)
		request(valid, handle, path, func(r *http.Request) { r.Header.Add(observationHeader, "other") })
		inspect(handle, "unavailable")
	})
	t.Run("expired and unfinished handles", func(t *testing.T) {
		handle := arm()
		c.observations[handle].expires = time.Now().Add(-time.Second)
		request(valid, handle, path, nil)
		inspect(handle, "unavailable")
		inspect(arm(), "unavailable")
	})
	t.Run("lost observer state cannot be recovered as denial", func(t *testing.T) {
		_, err := newControls(s, "private").readAuth(arm())
		require.Error(t, err)
	})
	t.Run("private inspection requires separate control authority and TLS", func(t *testing.T) {
		handle := arm()
		request(valid, handle, path, nil)
		for _, tlsEnabled := range []bool{false, true} {
			r := httptest.NewRequest(http.MethodPost, "http://pilot.example/__pilot/control", strings.NewReader(`{"action":"observe-auth","handle":"`+handle+`"}`))
			if tlsEnabled {
				r.TLS = &tls.ConnectionState{}
				r.Header.Set("Authorization", "Bearer "+valid)
			} else {
				r.Header.Set("Authorization", "Bearer "+c.token)
			}
			w := httptest.NewRecorder()
			c.ServeHTTP(w, r)
			require.Equal(t, 403, w.Code)
			require.NotContains(t, w.Body.String(), "archived")
		}
		inspect(handle, "archived_access_token")
	})
}
