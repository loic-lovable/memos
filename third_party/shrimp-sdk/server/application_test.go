package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/require"
)

type attemptKey struct{}
type contractApp struct {
	Application
	t                                                  *testing.T
	events                                             []string
	beginError, identifyError, applyError, finishError error
	mutation                                           Mutation
	outcome                                            AuditOutcome
	cancel                                             context.CancelFunc
}

func (a *contractApp) BeginAudit(ctx context.Context, principal, authority, action string) (context.Context, error) {
	a.events = append(a.events, "begin")
	require.Equal(a.t, "client", principal)
	require.Equal(a.t, "hr", authority)
	require.Equal(a.t, "mutation", action)
	return context.WithValue(ctx, attemptKey{}, "native-attempt"), a.beginError
}
func (a *contractApp) IdentifyAudit(ctx context.Context, window, id string) error {
	a.events = append(a.events, "identify")
	require.Equal(a.t, "native-attempt", ctx.Value(attemptKey{}))
	require.Equal(a.t, "window", window)
	require.Equal(a.t, "operation", id)
	return a.identifyError
}
func (a *contractApp) Apply(ctx context.Context, m Mutation) (*Result, error) {
	a.events = append(a.events, "apply")
	require.Equal(a.t, "native-attempt", ctx.Value(attemptKey{}))
	a.mutation = m
	if a.cancel != nil {
		a.cancel()
	}
	return &Result{Subject: Subject{ID: "subject", Revision: "revision"}, Time: 1, RetainedUntil: 86401, Action: m.Action, CommandID: m.CommandID, Token: "token"}, a.applyError
}
func (a *contractApp) FinishAudit(ctx context.Context, outcome AuditOutcome) error {
	a.events = append(a.events, "finish")
	require.Equal(a.t, "native-attempt", ctx.Value(attemptKey{}))
	require.NoError(a.t, ctx.Err(), "journal completion survives caller cancellation")
	_, bounded := ctx.Deadline()
	require.True(a.t, bounded)
	a.outcome = outcome
	return a.finishError
}

const contractMutation = `{"operation":{"id":"operation","replay_window":"window"},"execute_before":"2030-01-01T00:00:00Z","expected_authority":"hr","required_capabilities":[],"required_dependencies":[],"commands":[{"command_id":"command","action":"disable","resource":{"id":"subject"},"expected_revision":"old"}]}`

func TestApplicationOwnsAtomicMutationAndAuditPublication(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		begin, identify, apply, finish error
		wantEvents                     []string
		status                         int
		commit                         string
	}{
		{name: "success", wantEvents: []string{"begin", "identify", "apply", "finish"}, status: 200},
		{name: "begin_failure", begin: errors.New("private"), wantEvents: []string{"begin"}, status: 503},
		{name: "identify_failure", identify: errors.New("private"), wantEvents: []string{"begin", "identify", "finish"}, status: 503},
		{name: "unknown_storage_outcome", apply: errors.New("private"), wantEvents: []string{"begin", "identify", "apply", "finish"}, status: 503},
		{name: "known_rejection", apply: errors.New("replay_conflict"), wantEvents: []string{"begin", "identify", "apply", "finish"}, status: 409, commit: "not_committed"},
		{name: "publication_failure", finish: errors.New("private"), wantEvents: []string{"begin", "identify", "apply", "finish"}, status: 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			app := &contractApp{t: t, beginError: tc.begin, identifyError: tc.identify, applyError: tc.apply, finishError: tc.finish, cancel: cancel}
			h := &Handler{driver: app, path: "/shrimp", config: Config{Authority: "hr", AllowWrite: true, AdmissionConsumer: "application-admission"}, resolved: map[string]*jsonschema.Resolved{"mutation": objectSchema(t), "receipt": objectSchema(t)}}
			request := httptest.NewRequest(http.MethodPost, "/shrimp/mutations", strings.NewReader(contractMutation)).WithContext(ctx)
			request.Header.Set("SHRIMP-Version", "0.2")
			w := httptest.NewRecorder()
			claims := &accessClaims{Scope: "shrimp.read shrimp.write"}
			claims.Subject = "client"
			h.auditedMutation(w, request, claims)
			require.Equal(t, tc.wantEvents, app.events)
			require.Equal(t, tc.status, w.Code)
			require.Equal(t, tc.commit, app.outcome.Commit)
			require.NotContains(t, w.Body.String(), "private")
			if tc.status == 200 {
				require.Equal(t, "operation", app.mutation.ID)
				require.Equal(t, "old", app.mutation.ExpectedRevision)
				require.False(t, app.mutation.RecoverOnly)
				require.Contains(t, w.Body.String(), "application-admission")
			} else {
				require.NotContains(t, w.Body.String(), `"state":"succeeded"`)
			}
		})
	}
}

func TestApplicationNotFoundRemainsRecoveryFailureWithoutStorageDetails(t *testing.T) {
	app := &lookupApp{failure: fmt.Errorf("private: %w", ErrNotFound)}
	h := &Handler{driver: app, path: "/shrimp"}
	r := httptest.NewRequest(http.MethodGet, "/shrimp/operations/window/operation", nil)
	r.Header.Set("SHRIMP-Version", "0.2")
	w := httptest.NewRecorder()
	h.authenticated(w, r, &accessClaims{Scope: "shrimp.read"})
	require.Equal(t, 410, w.Code)
	var value map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &value))
	require.Equal(t, "operation_result_unavailable", value["code"])
	require.Equal(t, "unknown", value["commit"])
	require.NotContains(t, w.Body.String(), "private")
}

type lookupApp struct {
	Application
	failure error
}

func (a *lookupApp) Result(context.Context, string, string, string) (*Result, error) {
	return nil, a.failure
}
