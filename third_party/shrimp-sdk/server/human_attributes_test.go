package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/lovablelabs/shrimp-protocol/sdk/go/profiles/humanattributes"
	"github.com/stretchr/testify/require"
)

func TestHumanAttributeDispatchPreservesIntentBeforeCurrentValidation(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		app := &contractApp{t: t}
		h := &Handler{driver: app, path: "/shrimp", config: Config{Authority: "hr", AllowWrite: true, ExperimentalHumanAttributes: enabled}, resolved: map[string]*jsonschema.Resolved{"mutation": objectSchema(t), "receipt": objectSchema(t)}}
		var body map[string]any
		require.NoError(t, json.Unmarshal([]byte(contractMutation), &body))
		body["commands"] = []any{map[string]any{"command_id": "command", "action": "update_subject", "resource": map[string]any{"type": "subject", "id": "subject"}, "expected_revision": "old", "attribute_profile": humanattributes.ID, "set": map[string]any{"timezone": "Previously/Supported"}, "clear": []any{}}}
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		request := httptest.NewRequest(http.MethodPost, "/shrimp/mutations", strings.NewReader(string(raw)))
		request.Header.Set("SHRIMP-Version", "0.2")
		claims := &accessClaims{Scope: "shrimp.read shrimp.write"}
		claims.Subject = "client"
		response := httptest.NewRecorder()
		h.auditedMutation(response, request, claims)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.Contains(t, app.events, "apply")
		require.Equal(t, !enabled, app.mutation.UnsupportedProfiles)
		require.JSONEq(t, `{"set":{"timezone":"Previously/Supported"},"clear":[]}`, string(app.mutation.HumanAttributes))
		require.Nil(t, app.mutation.Set, "typed intent must not become scalar data")
		// The application can recover historical success before testing its current
		// catalog or support. The handler must not decode using current profile rules.
	}
}

func TestHumanMigrationApprovalBindsUnsignedIntentAndRetainsFullFingerprint(t *testing.T) {
	app := &contractApp{t: t}
	h := &Handler{driver: app, path: "/shrimp", config: Config{Authority: "hr", AllowWrite: true, ExperimentalHumanAttributes: true}, resolved: map[string]*jsonschema.Resolved{"mutation": objectSchema(t), "receipt": objectSchema(t)}}
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(contractMutation), &body))
	command := map[string]any{"command_id": "command", "action": "migrate_human_attributes", "resource": map[string]any{"type": "subject", "id": "subject"}, "expected_revision": "old", "email_entry_id": nil}
	body["commands"] = []any{command}
	unsigned, err := json.Marshal(body)
	require.NoError(t, err)
	approvalHash := sha256.Sum256(unsigned)
	command["authorization"] = "admin-handle"
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	fullHash := sha256.Sum256(raw)
	request := httptest.NewRequest(http.MethodPost, "/shrimp/mutations", strings.NewReader(string(raw)))
	request.Header.Set("SHRIMP-Version", "0.2")
	claims := &accessClaims{Scope: "shrimp.read shrimp.write"}
	claims.Subject = "client"
	response := httptest.NewRecorder()
	h.auditedMutation(response, request, claims)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Equal(t, hex.EncodeToString(fullHash[:]), app.mutation.Fingerprint)
	require.Equal(t, &HumanAttributeMigration{Authorization: "admin-handle", ApprovalFingerprint: hex.EncodeToString(approvalHash[:])}, app.mutation.Migration)
}

func TestHumanDirectRecordUsesJSONFactsWithoutPrivateHistory(t *testing.T) {
	h := &Handler{config: Config{Authority: "hr"}}
	facts := &humanattributes.Facts{Name: &humanattributes.Fact[humanattributes.Name]{Value: &humanattributes.Name{GivenName: new("Maya")}, Authority: "hr", Revision: "name-r1"}, Emails: &humanattributes.Fact[[]humanattributes.Email]{Authority: "hr", Revision: "email-r2"}}
	s := Subject{ID: "subject", Revision: "r3", Lifecycle: "disabled", AttributeProfile: humanattributes.ID, HumanAttributes: facts}
	record := h.directRecord(Record{Type: "subject", ID: s.ID, Subject: s})
	value := record["value"].(map[string]any)
	require.Equal(t, humanattributes.ID, value["attribute_profile"])
	attrs, ok := value["attributes"].(map[string]any)
	require.True(t, ok, "schema validation needs plain JSON objects")
	require.Nil(t, attrs["emails"].(map[string]any)["value"])
	require.NotContains(t, attrs, "entry_ids")
	require.NotContains(t, attrs, "displayName")
}
