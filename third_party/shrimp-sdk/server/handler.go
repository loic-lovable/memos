package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/pkg/errors"
)

//go:embed discovery.json
var discoveryTemplate []byte

// Config describes trusted application enrollment; requests cannot choose it.
// Scope and enforcement declarations must match the application's durable state.
type Config struct {
	Resource        string `json:"resource"`
	Issuer          string `json:"issuer"`
	IssuerKeyID     string `json:"issuer_key_id"`
	IssuerKeyFile   string `json:"issuer_key_file"`
	ClientID        string `json:"client_id"`
	Authority       string `json:"authority"`
	SchemaDirectory string `json:"schema_directory"`
	// ScalarAttributes requires Apply to enforce and persist Set/Clear and owned facts.
	ScalarAttributes                                bool `json:"scalar_attributes"`
	AllowWrite                                      bool `json:"allow_write"`
	Tenant, Domain, HistoryEpoch, DiscoveryRevision string
	AdmissionConsumer, HealthyConditions            string
}

// Handler exposes only the selected pilot operations, with explicit 0.2 selection.
type Handler struct {
	config       Config
	driver       Application
	key          *ecdsa.PublicKey
	origin, path string
	schemas      map[string]json.RawMessage
	resolved     map[string]*jsonschema.Resolved
	discovery    map[string]any
}

// New validates trusted configuration and compiles the pinned local schema catalog.
// Enrollment and persistence setup must already have been completed by the app.
func New(ctx context.Context, app Application, config Config) (*Handler, error) {
	u, err := url.Parse(config.Resource)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || u.Path == "" || strings.HasSuffix(u.Path, "/") || u.RawPath != "" {
		return nil, errors.New("resource must be an exact HTTPS URL with an unescaped nonempty path and no trailing slash")
	}
	if config.ClientID == "" || config.Authority == "" || config.Issuer == "" {
		return nil, errors.New("missing enrollment")
	}
	pem, err := os.ReadFile(config.IssuerKeyFile)
	if err != nil {
		return nil, err
	}
	key, err := jwt.ParseECPublicKeyFromPEM(pem)
	if err != nil {
		return nil, err
	}
	if app == nil || config.Tenant == "" || config.Domain == "" || config.HistoryEpoch == "" || config.DiscoveryRevision == "" || config.AdmissionConsumer == "" || config.HealthyConditions == "" {
		return nil, errors.New("application and scope, history, admission, and healthy-condition declarations are required")
	}
	h := &Handler{config: config, driver: app, key: key, origin: u.Scheme + "://" + u.Host, path: u.EscapedPath(), schemas: map[string]json.RawMessage{}, resolved: map[string]*jsonschema.Resolved{}}
	paths, err := filepath.Glob(filepath.Join(config.SchemaDirectory, "*-v0.2.schema.json"))
	if err != nil {
		return nil, err
	}
	paths = append(paths, filepath.Join(config.SchemaDirectory, "human-attributes-v1.schema.json"))
	catalog := map[string]*jsonschema.Schema{}
	entries := []any{}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var schema jsonschema.Schema
		if err := json.Unmarshal(b, &schema); err != nil {
			return nil, err
		}
		catalog[schema.ID] = &schema
		h.schemas[filepath.Base(path)] = b
		hash := sha256.Sum256(b)
		entries = append(entries, map[string]any{"id": schema.ID, "path": "/schemas/" + filepath.Base(path), "sha256": hex.EncodeToString(hash[:])})
	}
	for name, ref := range map[string]string{"mutation": "mutation-v0.2.schema.json#/$defs/request", "read": "read-sync-v0.2.schema.json#/$defs/read_request", "read_response": "read-sync-v0.2.schema.json#/$defs/read_response", "enumeration": "read-sync-v0.2.schema.json#/$defs/enumeration_request", "enumeration_page": "read-sync-v0.2.schema.json#/$defs/enumeration_page", "receipt": "receipt-v0.2.schema.json", "discovery": "discovery-v0.2.schema.json"} {
		schema := &jsonschema.Schema{Ref: "https://shrimp.example/schemas/0.2/" + ref}
		resolved, err := schema.Resolve(&jsonschema.ResolveOptions{Loader: func(uri *url.URL) (*jsonschema.Schema, error) {
			s, ok := catalog[uri.String()]
			if !ok {
				return nil, errors.New("unknown catalog reference")
			}
			return s, nil
		}})
		if err != nil {
			return nil, err
		}
		h.resolved[name] = resolved
	}
	if err := json.Unmarshal(discoveryTemplate, &h.discovery); err != nil {
		return nil, err
	}
	h.discovery["resource"] = config.Resource
	h.discovery["scope"] = map[string]any{"tenant": config.Tenant, "domain": config.Domain}
	h.discovery["discovery_revision"] = config.DiscoveryRevision
	contract := h.discovery["versions"].([]any)[0].(map[string]any)
	contract["healthy_conditions"] = config.HealthyConditions
	limits := contract["limits"].(map[string]any)
	limits["max_enumeration_page_records"] = EnumerationMaxPage
	limits["max_open_enumerations_per_principal"] = EnumerationMaxOpen
	limits["min_enumeration_cursor_lifetime_seconds"] = EnumerationLifetime
	contract["schemas"] = entries
	contract["dependency_issuers"] = []any{map[string]any{"resource": config.Resource, "domains": []string{config.Domain}}}
	if err := h.resolved["discovery"].Validate(h.discovery); err != nil {
		return nil, err
	}
	return h, nil
}

func stamp(seconds int64) string         { return time.Unix(seconds, 0).UTC().Format("2006-01-02T15:04:05Z") }
func ref(kind, id string) map[string]any { return map[string]any{"type": kind, "id": id} }
func (h *Handler) scope() map[string]any {
	return map[string]any{"resource": h.config.Resource, "tenant": h.config.Tenant, "domain": h.config.Domain, "schema_version": "0.2", "history_epoch": h.config.HistoryEpoch, "authorization_context": h.config.Authority}
}

func send(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (h *Handler) problem(w http.ResponseWriter, status int, code, stage string) {
	if code == "limit_exceeded" {
		h.problemRecovery(w, status, code, stage, "correct_new_work_or_recover", 0)
		return
	}
	h.problemRecovery(w, status, code, stage, "inspect_or_repair", 0)
}

func (h *Handler) problemRecovery(w http.ResponseWriter, status int, code, stage, recovery string, retryAfter int) {
	var retry any
	if retryAfter > 0 {
		retry = retryAfter
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	if audit, ok := w.(*auditResponse); ok {
		audit.code, audit.stage = code, stage
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "about:blank", "title": code, "status": status, "code": code, "stage": stage, "operation": nil, "command_id": nil, "commit": "unknown", "recovery": map[string]any{"action": recovery, "retry_after_seconds": retry}})
}

// ServeHTTP authenticates before negotiation or revealing protected route state.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.TLS == nil {
		h.problem(w, 400, "https_required", "authentication")
		return
	}
	if r.URL.Path == "/.well-known/oauth-protected-resource"+h.path && r.Method == "GET" {
		send(w, 200, map[string]any{"resource": h.config.Resource, "authorization_servers": []string{h.config.Issuer}, "scopes_supported": []string{"shrimp.read", "shrimp.write"}, "bearer_methods_supported": []string{}, "dpop_signing_alg_values_supported": []string{"ES256"}, "dpop_bound_access_tokens_required": true})
		return
	}
	claims, err := h.authenticate(r)
	if err != nil {
		h.authenticationProblem(w, err)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == h.path+"/mutations" {
		h.auditedMutation(w, r, claims)
		return
	}
	h.authenticated(w, r, claims)
}

func (h *Handler) authenticated(w http.ResponseWriter, r *http.Request, claims *accessClaims) {
	if !hasScope(claims, "shrimp.read") {
		h.insufficientScope(w, "shrimp.read")
		return
	}
	if !strings.HasPrefix(r.URL.Path, h.path+"/") {
		h.problem(w, 404, "not_found", "read")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, h.path)
	versionName := "SHRIMP-Version"
	if path == "/capabilities" {
		versionName = "SHRIMP-Discovery-Version"
	}
	version, err := oneHeader(r, versionName)
	if err != nil || version != "0.2" {
		code := "unsupported_version"
		switch {
		case len(r.Header.Values(versionName)) == 0:
			code = "version_required"
		case err != nil || version == "" || strings.Contains(version, ","):
			code = "invalid_request"
		}
		if versionName == "SHRIMP-Discovery-Version" && code != "invalid_request" {
			code = "unsupported_discovery_version"
		}
		h.problem(w, 400, code, "negotiation")
		return
	}
	w.Header().Set(versionName, "0.2")
	switch {
	case r.Method == "GET" && path == "/capabilities":
		send(w, 200, h.discovery)
	case r.Method == "GET" && strings.HasPrefix(path, "/schemas/"):
		value, ok := h.schemas[strings.TrimPrefix(path, "/schemas/")]
		if !ok {
			h.problem(w, 404, "not_found", "read")
			return
		}
		w.Header().Set("Content-Type", "application/schema+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(value)
	case r.Method == "GET" && path == "/replay-window":
		if !hasScope(claims, "shrimp.write") {
			h.insufficientScope(w, "shrimp.write")
			return
		}
		if !h.config.AllowWrite {
			h.problem(w, 403, "forbidden", "authorization")
			return
		}
		id, closes, err := h.driver.Window(r.Context(), claims.Subject)
		if err != nil {
			if errors.Is(err, ErrWindowQuota) {
				h.throttled(w, "acceptance")
			} else {
				h.problemRecovery(w, 503, "storage_unavailable", "acceptance", "retry_with_fresh_proof", 1)
			}
			return
		}
		send(w, 200, map[string]any{"schema_version": "0.2", "scope": h.scope(), "replay_window": id, "server_time": stamp(time.Now().Unix()), "closes_at": stamp(closes), "results_retained_until": stamp(closes + 86400), "min_result_retention_seconds": 86400})
	case r.Method == "POST" && path == "/mutations":
		h.mutate(w, r, claims)
	case r.Method == "POST" && path == "/reads":
		h.read(w, r)
	case r.Method == "POST" && path == "/enumerations":
		h.enumerate(w, r, claims)
	case r.Method == "GET" && strings.HasPrefix(path, "/operations/"):
		parts := strings.Split(strings.TrimPrefix(path, "/operations/"), "/")
		if len(parts) != 2 {
			h.problem(w, 404, "not_found", "recovery")
			return
		}
		result, err := h.driver.Result(r.Context(), claims.Subject, parts[0], parts[1])
		if err != nil {
			if !errors.Is(err, ErrNotFound) {
				h.problem(w, 503, "storage_unavailable", "recovery")
				return
			}
			h.problem(w, 410, "operation_result_unavailable", "recovery")
			return
		}
		h.writeReceipt(w, parts[0], parts[1], result)
	default:
		h.problem(w, 400, "unsupported_operation", "acceptance")
	}
}

const maxRequestBytes = 65536

var errRequestTooLarge = errors.New("request byte limit exceeded")
var errRequestLimit = errors.New("request shape limit exceeded")

func (h *Handler) body(r *http.Request, schema string) (map[string]any, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if len(b) > maxRequestBytes {
		return nil, errRequestTooLarge
	}
	if err != nil {
		return nil, errors.Wrap(err, "read request body")
	}
	var body map[string]any
	if err := strictJSON(b, &body); err != nil {
		return nil, err
	}
	// Check advertised cardinality limits before the schema's broader safety
	// envelope, so larger requests retain the same machine-readable category.
	if commands, ok := body["commands"].([]any); schema == "mutation" && ok && len(commands) > 1 {
		return nil, errRequestLimit
	}
	if dependencies, ok := body["required_dependencies"].([]any); ok && len(dependencies) > 16 {
		return nil, errRequestLimit
	}
	if size, ok := body["page_size"].(float64); schema == "enumeration" && ok && size > EnumerationMaxPage {
		return nil, errRequestLimit
	}
	if err := h.resolved[schema].Validate(body); err != nil {
		return nil, err
	}
	return body, nil
}

func (h *Handler) mutate(w http.ResponseWriter, r *http.Request, claims *accessClaims) {
	body, err := h.body(r, "mutation")
	if err != nil {
		h.bodyProblem(w, err, "acceptance")
		return
	}
	operation := body["operation"].(map[string]any)
	if err := h.driver.IdentifyAudit(r.Context(), operation["replay_window"].(string), operation["id"].(string)); err != nil {
		h.problem(w, 503, "audit_unavailable", "acceptance")
		return
	}
	commands := body["commands"].([]any)
	if len(body["required_capabilities"].([]any)) != 0 || body["reconciliation"] != nil {
		h.problem(w, 400, "unsupported_operation", "acceptance")
		return
	}
	if body["expected_authority"] != h.config.Authority {
		h.problem(w, 403, "wrong_authority", "authorization")
		return
	}
	command := commands[0].(map[string]any)
	op := body["operation"].(map[string]any)
	deadline, err := time.Parse("2006-01-02T15:04:05Z", body["execute_before"].(string))
	if err != nil {
		h.problem(w, 400, "invalid_deadline", "acceptance")
		return
	}
	canonical, _ := json.Marshal(body)
	hash := sha256.Sum256(canonical)
	m := Mutation{Authority: h.config.Authority, Principal: claims.Subject, Window: op["replay_window"].(string), ID: op["id"].(string), Fingerprint: hex.EncodeToString(hash[:]), Action: command["action"].(string), Deadline: deadline.Unix(), CommandID: command["command_id"].(string), RecoverOnly: !h.config.AllowWrite || !hasScope(claims, "shrimp.write")}
	if profiles, ok := body["required_profiles"].([]any); ok && len(profiles) > 0 {
		m.UnsupportedProfiles = true
	}
	if _, selected := command["attribute_profile"]; selected {
		// Keep implied support rejection at the application's commit boundary,
		// after retained intent equality and authorized outcome recovery.
		m.UnsupportedProfiles = true
	}
	for _, token := range body["required_dependencies"].([]any) {
		m.Dependencies = append(m.Dependencies, token.(string))
	}
	switch m.Action {
	case "create_subject":
		attributes := command["attributes"].(map[string]any)
		source, ok := command["source_reference"].(string)
		name, nameOK := attributes["displayName"].(string)
		legacyOnly := !h.config.ScalarAttributes && (!nameOK || len(attributes) != 1)
		if command["profile"] != "human" || !ok || source == "" || legacyOnly {
			h.problem(w, 400, "unsupported_create_shape", "acceptance")
			return
		}
		m.SourceReference = source
		m.DisplayName = name
		if h.config.ScalarAttributes {
			var valid bool
			m.Set, m.Clear, valid = scalarChanges(attributes, nil)
			if !valid {
				h.problem(w, 400, "unsupported_attribute_update", "acceptance")
				return
			}
		}
	case "activate", "disable", "retire", "update_subject":
		resource := command["resource"].(map[string]any)
		m.SubjectID = resource["id"].(string)
		m.ExpectedRevision = command["expected_revision"].(string)
		if m.Action == "update_subject" {
			set := command["set"].(map[string]any)
			name, ok := set["displayName"].(string)
			if !h.config.ScalarAttributes && (!ok || len(set) != 1 || len(command["clear"].([]any)) != 0) {
				h.problem(w, 400, "unsupported_attribute_update", "acceptance")
				return
			}
			m.DisplayName = name
			if h.config.ScalarAttributes {
				var valid bool
				m.Set, m.Clear, valid = scalarChanges(set, command["clear"].([]any))
				if !valid {
					h.problem(w, 400, "unsupported_attribute_update", "acceptance")
					return
				}
			}
		}
	default:
		h.problem(w, 400, "unsupported_command", "acceptance")
		return
	}
	result, err := h.driver.Apply(r.Context(), m)
	if err != nil {
		if errors.Is(err, ErrCapacity) {
			h.throttled(w, "acceptance")
			return
		}
		code := "storage_unavailable"
		status := 503
		stage := "commit"
		switch err.Error() {
		case "replay_conflict":
			code = err.Error()
			status = 409
		case "operation_result_unavailable":
			code = err.Error()
			status = 410
			stage = "recovery"
		case "unsupported_profile":
			code = err.Error()
			status = 400
			stage = "acceptance"
		case "insufficient_scope":
			if !hasScope(claims, "shrimp.write") {
				h.insufficientScope(w, "shrimp.write")
			} else {
				h.problem(w, 403, "forbidden", "authorization")
			}
			return
		case "execution_deadline_expired":
			code = err.Error()
			status = 409
		}
		h.problem(w, status, code, stage)
		return
	}
	w.Header().Set("Location", h.path+"/operations/"+m.Window+"/"+m.ID)
	h.writeReceipt(w, m.Window, m.ID, result)
}

func (h *Handler) writeReceipt(w http.ResponseWriter, window, id string, result *Result) {
	s := result.Subject
	resources := []any{}
	effects := []any{}
	state := "succeeded"
	commit := "committed"
	var token any = result.Token
	var failure any
	if result.Error != "" {
		state = "failed"
		commit = "not_committed"
		token = nil
		failure = map[string]any{"code": result.Error, "stage": "commit", "command_id": result.CommandID, "recovery": "new_conditional_intent"}
	} else {
		resources = append(resources, map[string]any{"command_id": result.CommandID, "resource": ref("subject", s.ID), "revision": s.Revision})
		if result.Action == "create_subject" {
			resources = append(resources, map[string]any{"command_id": result.CommandID, "resource": ref("source_reference", s.SourceID), "revision": s.SourceRevision})
		}
		if result.Action == "disable" || result.Action == "retire" {
			effects = append(effects, map[string]any{"id": "admission", "kind": "admission_block", "resource": ref("subject", s.ID), "consumer": h.config.AdmissionConsumer, "state": "complete", "deadline": stamp(result.Time), "observed_frontier": result.Token, "error": nil})
		}
	}
	value := map[string]any{"schema_version": "0.2", "scope": h.scope(), "operation": map[string]any{"id": id, "replay_window": window}, "accepted_at": stamp(result.Time), "state": state, "terminal_at": stamp(result.Time), "commit": map[string]any{"state": commit, "causal_token": token, "resources": resources}, "effects": effects, "error": failure, "superseded_by": nil, "results_retained_until": stamp(result.RetainedUntil), "min_result_retention_seconds": 86400, "poll_after_seconds": nil}
	if err := h.resolved["receipt"].Validate(value); err != nil {
		h.problem(w, 500, "invalid_receipt", "recovery")
		return
	}
	send(w, 200, value)
}

func (h *Handler) read(w http.ResponseWriter, r *http.Request) {
	body, err := h.body(r, "read")
	if err != nil {
		h.bodyProblem(w, err, "read")
		return
	}
	if profiles, ok := body["required_profiles"].([]any); ok && len(profiles) > 0 {
		h.problem(w, 400, "unsupported_profile", "read")
		return
	}
	target := body["target"].(map[string]any)
	resource, ok := target["resource"].(map[string]any)
	if !ok || body["wait_ms"] != float64(0) {
		h.problem(w, 400, "unsupported_read", "read")
		return
	}
	kind := resource["type"].(string)
	if kind != "subject" && kind != "source_reference" {
		h.problem(w, 400, "unsupported_resource", "read")
		return
	}
	deps := []string{}
	for _, v := range body["required_dependencies"].([]any) {
		deps = append(deps, v.(string))
	}
	id := resource["id"].(string)
	s, frontier, err := h.driver.Read(r.Context(), id, deps)
	if err != nil {
		status := 503
		code := "storage_unavailable"
		if err.Error() == "invalid_dependency" {
			status = 409
			code = "invalid_dependency"
		}
		if errors.Is(err, ErrNotFound) {
			status = 404
			code = "not_found"
		}
		h.problem(w, status, code, "read")
		return
	}
	if (kind == "subject" && id != s.ID) || (kind == "source_reference" && id != s.SourceID) {
		h.problem(w, 404, "not_found", "read")
		return
	}
	record := h.directRecord(Record{Type: kind, ID: id, Subject: *s})
	response := map[string]any{"scope": h.scope(), "observed_frontier": frontier, "state_validator": record["revision"], "validator_expires_at": stamp(time.Now().Unix() + 60), "record": record}
	if err := h.resolved["read_response"].Validate(response); err != nil {
		h.problem(w, 500, "invalid_read_response", "read")
		return
	}
	send(w, 200, response)
}

// bodyProblem keeps untrusted or undecodable operation identities out of errors.
// Refusing this attempt says nothing about a previous attempt's durable outcome.
func (h *Handler) bodyProblem(w http.ResponseWriter, err error, stage string) {
	switch {
	case errors.Is(err, errRequestTooLarge):
		h.problem(w, http.StatusRequestEntityTooLarge, "limit_exceeded", stage)
	case errors.Is(err, errJSONDepth), errors.Is(err, errRequestLimit):
		h.problem(w, http.StatusBadRequest, "limit_exceeded", stage)
	default:
		h.problemRecovery(w, http.StatusBadRequest, "invalid_request", stage, "correct_new_work_or_recover", 0)
	}
}

func (h *Handler) throttled(w http.ResponseWriter, stage string) {
	// Scope is the principal-within-resource boundary advertised in discovery.
	// Retrying preserves the original body, deadline, window and cursor.
	h.problemRecovery(w, http.StatusTooManyRequests, "throttled", stage, "retry_with_fresh_proof", 1)
}
