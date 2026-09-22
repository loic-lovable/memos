package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/require"
)

func TestScalarUpdatesRequireApplicationSupportAndPreserveIntent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
		clear   []any
		set     map[string]any
		status  int
	}{
		{name: "application opted in", enabled: true, set: map[string]any{"department": "Research"}, clear: []any{"email"}, status: 200},
		{name: "old application", set: map[string]any{"department": "Research"}, clear: []any{}, status: 400},
		{name: "overlap", enabled: true, set: map[string]any{"email": "exact@Example.COM"}, clear: []any{"email"}, status: 400},
		{name: "unknown", enabled: true, set: map[string]any{"password": "secret"}, clear: []any{}, status: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := &contractApp{t: t}
			h := &Handler{driver: app, path: "/shrimp", config: Config{Authority: "hr", AllowWrite: true, ScalarAttributes: tc.enabled}, resolved: map[string]*jsonschema.Resolved{"mutation": objectSchema(t), "receipt": objectSchema(t)}}
			var body map[string]any
			require.NoError(t, json.Unmarshal([]byte(contractMutation), &body))
			body["commands"] = []any{map[string]any{"command_id": "command", "action": "update_subject", "resource": map[string]any{"type": "subject", "id": "subject"}, "expected_revision": "old", "set": tc.set, "clear": tc.clear}}
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			r := httptest.NewRequest(http.MethodPost, "/shrimp/mutations", strings.NewReader(string(raw)))
			r.Header.Set("SHRIMP-Version", "0.2")
			w := httptest.NewRecorder()
			claims := &accessClaims{Scope: "shrimp.read shrimp.write"}
			claims.Subject = "client"
			h.auditedMutation(w, r, claims)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			if tc.status == 200 {
				require.Equal(t, map[string]string{"department": "Research"}, app.mutation.Set)
				require.Equal(t, []string{"email"}, app.mutation.Clear)
				require.Equal(t, "hr", app.mutation.Authority)
			} else {
				require.NotContains(t, app.events, "apply")
			}
		})
	}
}

func TestScalarRecordsKeepNullAndFieldRevision(t *testing.T) {
	h := &Handler{config: Config{Authority: "hr"}}
	empty := ""
	subject := Subject{ID: "subject", Revision: "subject-r3", Lifecycle: "disabled", Attributes: map[string]ScalarFact{"email": {Authority: "hr", Revision: "field-r2"}, "department": {Value: &empty, Authority: "hr", Revision: "field-r1"}}}
	record := h.directRecord(Record{Type: "subject", ID: subject.ID, Subject: subject})
	attrs := record["value"].(map[string]any)["attributes"].(map[string]any)
	require.Nil(t, attrs["email"].(map[string]any)["value"])
	require.Equal(t, "field-r2", attrs["email"].(map[string]any)["revision"])
	require.Equal(t, "", attrs["department"].(map[string]any)["value"])
	require.NotContains(t, attrs, "displayName")
}
