package shrimp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/store"
)

func TestVersionNegotiationErrorsDoNotSelectAContract(t *testing.T) {
	for _, route := range []struct {
		name, path, header, missing, unsupported string
	}{
		{"schema", "/schemas/receipt", "SHRIMP-Version", "version_required", "unsupported_version"},
		{"discovery", "/capabilities", "SHRIMP-Discovery-Version", "unsupported_discovery_version", "unsupported_discovery_version"},
	} {
		t.Run(route.name, func(t *testing.T) {
			for _, tc := range []struct {
				name   string
				values []string
				code   string
			}{
				{"supported", []string{"0.2"}, ""},
				{"missing", nil, route.missing},
				{"unknown", []string{"future-version"}, route.unsupported},
				{"legacy", []string{"0.1"}, route.unsupported},
				{"duplicate", []string{"0.2", "0.2"}, "invalid_request"},
				{"list", []string{"0.2, 0.2"}, "invalid_request"},
				{"empty", []string{""}, "invalid_request"},
				{"oversized", []string{strings.Repeat("x", 16385)}, "invalid_request"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					h := &Handler{path: "/shrimp", schemas: map[string]json.RawMessage{"receipt": json.RawMessage(`{}`)}}
					r := httptest.NewRequest(http.MethodGet, "/shrimp"+route.path, nil)
					for _, value := range tc.values {
						r.Header.Add(route.header, value)
					}
					w := httptest.NewRecorder()
					h.authenticated(w, r, &accessClaims{Scope: "shrimp.read"})
					if tc.code == "" {
						require.Equal(t, http.StatusOK, w.Code)
						require.Equal(t, "0.2", w.Header().Get(route.header))
						return
					}
					require.Equal(t, http.StatusBadRequest, w.Code)
					require.Empty(t, w.Header().Get("SHRIMP-Version"))
					require.Empty(t, w.Header().Get("SHRIMP-Discovery-Version"))
					var problem map[string]any
					require.NoError(t, json.Unmarshal(w.Body.Bytes(), &problem))
					require.Equal(t, tc.code, problem["code"])
					require.Equal(t, "negotiation", problem["stage"])
					require.Equal(t, "unknown", problem["commit"])
					require.Nil(t, problem["operation"])
					require.Nil(t, problem["command_id"])
				})
			}
		})
	}
}

func objectSchema(t *testing.T) *jsonschema.Resolved {
	t.Helper()
	schema, err := (&jsonschema.Schema{Type: "object"}).Resolve(nil)
	require.NoError(t, err)
	return schema
}

func TestBodyEnforcesActualByteBoundary(t *testing.T) {
	h := &Handler{resolved: map[string]*jsonschema.Resolved{"test": objectSchema(t)}}
	for _, tc := range []struct {
		name string
		size int
		want error
	}{
		{"below", 65535, nil},
		{"exact", 65536, nil},
		{"above", 65537, errRequestTooLarge},
		{"much_larger", 1048576, errRequestTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := &countedBody{Reader: strings.NewReader("{}" + strings.Repeat(" ", tc.size-2))}
			r := httptest.NewRequest(http.MethodPost, "/mutations", reader)
			r.ContentLength = -1 // Count received bytes, not a client-supplied length.
			body, err := h.body(r, "test")
			if tc.want == nil {
				require.NoError(t, err)
				require.NotNil(t, body)
			} else {
				require.ErrorIs(t, err, tc.want)
				require.Nil(t, body)
			}
			require.LessOrEqual(t, reader.bytes, 65537, "stop reading at the bounded rejection sentinel")
		})
	}
}

type countedBody struct {
	io.Reader
	bytes int
}

func (r *countedBody) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.bytes += n
	return n, err
}

func TestBodyProblemsKeepFailureClassAndUnknownOutcome(t *testing.T) {
	h := &Handler{}
	for _, tc := range []struct {
		name, code string
		err        error
		status     int
	}{
		{"bytes", "limit_exceeded", errors.Wrap(errRequestTooLarge, "private detail"), 413},
		{"depth", "limit_exceeded", errors.Wrap(errJSONDepth, "private detail"), 400},
		{"shape_limit", "limit_exceeded", errors.Wrap(errRequestLimit, "private detail"), 400},
		{"malformed", "invalid_request", errors.New("private parser detail"), 400},
		{"read_failed", "invalid_request", errors.Wrap(io.ErrUnexpectedEOF, "private detail"), 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.bodyProblem(w, tc.err, "acceptance")
			require.Equal(t, tc.status, w.Code)
			var body map[string]any
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
			require.Equal(t, tc.code, body["code"])
			require.Equal(t, float64(tc.status), body["status"])
			require.Equal(t, "unknown", body["commit"])
			require.Nil(t, body["operation"])
			require.Nil(t, body["command_id"])
			require.Equal(t, "correct_new_work_or_recover", body["recovery"].(map[string]any)["action"])
			require.Nil(t, body["recovery"].(map[string]any)["retry_after_seconds"])
			require.Empty(t, w.Header().Get("Retry-After"))
			require.NotContains(t, w.Body.String(), "private")
		})
	}
}

type boundsDriver struct {
	store.ShrimpDriver
	failure error
	calls   int
}

func (d *boundsDriver) ShrimpWindow(context.Context, string) (string, int64, error) {
	d.calls++
	return "retained-window", time.Now().Unix() + 300, d.failure
}

func (d *boundsDriver) EnumerateShrimp(context.Context, store.ShrimpEnumeration, func(*store.ShrimpEnumerationPage) (bool, error)) (*store.ShrimpEnumerationPage, error) {
	d.calls++
	return nil, d.failure
}

func TestWindowQuotaDiffersFromStorageFailure(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		err        error
		status     int
	}{
		{"healthy", "", nil, 200},
		{"quota", "throttled", errors.Wrap(store.ErrShrimpWindowQuota, "private detail"), 429},
		{"storage", "storage_unavailable", errors.New("private storage detail"), 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver := &boundsDriver{failure: tc.err}
			h := &Handler{driver: driver, path: "/shrimp", config: Config{AllowWrite: true}}
			r := httptest.NewRequest(http.MethodGet, "/shrimp/replay-window", nil)
			r.Header.Set("SHRIMP-Version", "0.2")
			w := httptest.NewRecorder()
			h.authenticated(w, r, &accessClaims{Scope: "shrimp.read shrimp.write"})
			require.Equal(t, 1, driver.calls)
			require.Equal(t, tc.status, w.Code)
			if tc.err == nil {
				require.Contains(t, w.Body.String(), "retained-window")
				require.Empty(t, w.Header().Get("Retry-After"))
				return
			}
			assertBoundedRetry(t, w, tc.status, tc.code, "acceptance")
		})
	}
}

func TestEnumerationThrottleMatchesRetryBody(t *testing.T) {
	driver := &boundsDriver{failure: errors.Wrap(store.ShrimpEnumerationError("throttled"), "private detail")}
	h := &Handler{driver: driver, resolved: map[string]*jsonschema.Resolved{"enumeration": objectSchema(t)}}
	r := httptest.NewRequest(http.MethodPost, "/enumerations", strings.NewReader(`{"view":{"resource_types":["subject"],"authority_filter":null,"representation":"full-direct-records"},"cursor":null,"wait_ms":0,"page_size":1,"required_dependencies":[],"required_profiles":[]}`))
	w := httptest.NewRecorder()
	h.enumerate(w, r, &accessClaims{Scope: "shrimp.read"})
	require.Equal(t, 1, driver.calls)
	assertBoundedRetry(t, w, 429, "throttled", "read")
}

func assertBoundedRetry(t *testing.T, w *httptest.ResponseRecorder, status int, code, stage string) {
	t.Helper()
	require.Equal(t, status, w.Code)
	require.Equal(t, "1", w.Header().Get("Retry-After"))
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, code, body["code"])
	require.Equal(t, float64(status), body["status"])
	require.Equal(t, stage, body["stage"])
	require.Equal(t, "unknown", body["commit"])
	require.Nil(t, body["operation"])
	require.Nil(t, body["command_id"])
	require.Equal(t, map[string]any{"action": "retry_with_fresh_proof", "retry_after_seconds": float64(1)}, body["recovery"])
	require.NotContains(t, w.Body.String(), "private")
}

func TestAdvertisedLimitsPrecedeSchemaCeilings(t *testing.T) {
	var schema jsonschema.Schema
	require.NoError(t, json.Unmarshal([]byte(`{"type":"object","properties":{"commands":{"type":"array","maxItems":64},"required_dependencies":{"type":"array","maxItems":64},"page_size":{"type":"integer","maximum":1000}}}`), &schema))
	resolved, err := schema.Resolve(nil)
	require.NoError(t, err)
	h := &Handler{resolved: map[string]*jsonschema.Resolved{"mutation": resolved, "read": resolved, "enumeration": resolved}}
	for _, tc := range []struct {
		name, schema, field string
		value               any
	}{
		{"commands", "mutation", "commands", make([]any, 65)},
		{"mutation_dependencies", "mutation", "required_dependencies", make([]any, 65)},
		{"read_dependencies", "read", "required_dependencies", make([]any, 65)},
		{"enumeration_dependencies", "enumeration", "required_dependencies", make([]any, 65)},
		{"enumeration_page", "enumeration", "page_size", 1001},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(map[string]any{tc.field: tc.value})
			require.NoError(t, err)
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(encoded)))
			_, err = h.body(r, tc.schema)
			require.ErrorIs(t, err, errRequestLimit)
		})
	}
}
