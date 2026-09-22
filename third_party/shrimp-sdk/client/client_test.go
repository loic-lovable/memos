package client

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/client/internal/schemas"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type testReply struct {
	status int
	value  any
	raw    []byte
	header http.Header
}

type peer struct {
	client    *Client
	tokens    atomic.Int32
	protected atomic.Int32
}

func newPeer(t *testing.T, change func(string, *testReply)) *peer {
	t.Helper()
	return newVersionPeer(t, "0.1", nil, change)
}

func newVersionPeer(t *testing.T, version string, required []string, change func(string, *testReply)) *peer {
	t.Helper()
	p := testProfile(t)
	p.Version, p.RequiredProfiles = version, required
	p.Issuer = "https://unused.example/realm" // Exercise path-bearing RFC 8414 discovery.
	fixture := &peer{}
	discovery := discoveryFixture(t)
	if version == "0.2" {
		discovery = discoveryFixture02(t)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := testReply{status: 200, header: make(http.Header)}
		reply.header.Set("Content-Type", "application/json")
		reply.header.Set("Cache-Control", "no-store")
		reply.header.Set("Pragma", "no-cache")
		path := r.URL.Path
		switch {
		case path == "/.well-known/oauth-protected-resource/shrimp/v1/tenants/acme/domains/A":
			reply.value = map[string]any{"resource": p.Resource, "authorization_servers": []any{p.Issuer},
				"scopes_supported": []any{"shrimp.read"}, "bearer_methods_supported": []any{}}
		case path == "/.well-known/oauth-authorization-server/realm":
			reply.value = map[string]any{"issuer": p.Issuer, "token_endpoint": strings.TrimSuffix(p.Issuer, "/realm") + "/token",
				"grant_types_supported": []any{"client_credentials"}, "token_endpoint_auth_methods_supported": []any{"private_key_jwt"},
				"token_endpoint_auth_signing_alg_values_supported": []any{"ES256"}, "dpop_signing_alg_values_supported": []any{"ES256"}}
		case path == "/token":
			fixture.tokens.Add(1)
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("scope") != "shrimp.read" || r.Form.Get("resource") != p.Resource || r.Header.Get("DPoP") == "" {
				t.Error("incorrect read-only grant")
			}
			reply.value = map[string]any{"access_token": "opaque-secret-access-token", "token_type": "DPoP", "expires_in": 300, "scope": "shrimp.read"}
		default:
			fixture.protected.Add(1)
			if r.Method != "GET" || r.Header.Get("Authorization") != "DPoP opaque-secret-access-token" || r.Header.Get("DPoP") == "" {
				t.Error("protected request missing proof or attempted a mutation")
			}
			reply.header.Set("SHRIMP-Version", version)
			switch {
			case strings.HasSuffix(path, "/capabilities"):
				if version == "0.2" {
					if r.Header.Get("SHRIMP-Discovery-Version") != "0.2" || r.Header.Get("SHRIMP-Version") != "" {
						t.Error("0.2 discovery did not select its explicit bootstrap")
					}
					reply.header.Set("SHRIMP-Discovery-Version", "0.2")
				}
				reply.value = discovery
			case strings.Contains(path, "/schemas/"):
				reply.header.Set("Content-Type", "application/schema+json")
				reply.raw, _ = schemas.Files.ReadFile(filepath.Base(path))
			default:
				reply.value = map[string]any{"operation": map[string]any{"id": "op-1", "replay_window": "window-1", "href": path},
					"state": "succeeded", "commit": map[string]any{"state": "committed", "causal_token": "causal-1",
						"resources": []any{map[string]any{"type": "subject", "id": "subject-1", "revision": "r1"}}},
					"effects": []any{}, "error": nil}
				if version == "0.2" {
					receipt := contractVector(t, "receipt-valid")
					receipt["scope"].(map[string]any)["resource"] = p.Resource
					receipt["operation"] = map[string]any{"id": "op-1", "replay_window": "window-1"}
					reply.value = receipt
				}
			}
			if !strings.HasSuffix(path, "/capabilities") && r.Header.Get("SHRIMP-Version") != version {
				t.Error("protected read did not preserve the configured version")
			}
		}
		if change != nil {
			change(path, &reply)
		}
		for name, values := range reply.header {
			w.Header()[name] = values
		}
		w.WriteHeader(reply.status)
		if reply.raw != nil {
			_, _ = w.Write(reply.raw)
		} else {
			_ = json.NewEncoder(w).Encode(reply.value)
		}
	}))
	t.Cleanup(server.Close)
	p.Resource = server.URL + "/shrimp/v1/tenants/acme/domains/A"
	p.Issuer = server.URL + "/realm"
	discovery["resource"] = p.Resource
	p.CAFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(p.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	fixture.client = c
	return fixture
}

func discoveryFixture(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/discovery-transcript.json")
	if err != nil {
		t.Fatal(err)
	}
	var transcript struct {
		Exchanges []struct{ Response struct{ Body map[string]any } }
	}
	if err := json.Unmarshal(raw, &transcript); err != nil {
		t.Fatal(err)
	}
	doc := transcript.Exchanges[0].Response.Body
	version := doc["versions"].([]any)[0].(map[string]any)
	files, _ := schemas.Files.ReadDir(".")
	catalog := []any{}
	for _, file := range files {
		raw, _ := schemas.Files.ReadFile(file.Name())
		value, err := decodeJSON(raw)
		if err != nil {
			t.Fatal(err)
		}
		if value.(map[string]any)["$id"] != schemaBase+file.Name() {
			continue // This peer advertises only the 0.1 catalog.
		}
		catalog = append(catalog, map[string]any{"id": schemaBase + file.Name(), "path": "/schemas/" + file.Name(), "sha256": fmt.Sprintf("%x", sha256.Sum256(raw))})
	}
	version["schemas"] = catalog
	return doc
}

func TestRemoteReadWorkflow(t *testing.T) {
	p := newPeer(t, nil)
	if err := p.client.Authenticate(t.Context()); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := p.client.Discover(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics.Scope["tenant"] != "acme" {
		t.Fatal("wrong scope")
	}
	if _, err := p.client.Inspect(t.Context(), "window-1", "op-1"); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataFailsBeforeCredentialDisclosure(t *testing.T) {
	for _, test := range []struct {
		name, route string
		change      func(*testReply)
	}{
		{"resource mismatch", "oauth-protected", func(r *testReply) { r.value.(map[string]any)["resource"] = "https://other.example" }},
		{"issuer not enrolled", "oauth-protected", func(r *testReply) { r.value.(map[string]any)["authorization_servers"] = []any{"https://other.example"} }},
		{"bearer fallback", "oauth-protected", func(r *testReply) { r.value.(map[string]any)["bearer_methods_supported"] = []any{"header"} }},
		{"missing read scope", "oauth-protected", func(r *testReply) { delete(r.value.(map[string]any), "scopes_supported") }},
		{"wrong issuer", "oauth-authorization", func(r *testReply) { r.value.(map[string]any)["issuer"] = "https://other.example" }},
		{"off-origin token endpoint", "oauth-authorization", func(r *testReply) { r.value.(map[string]any)["token_endpoint"] = "https://other.example/token" }},
		{"redirect", "oauth-authorization", func(r *testReply) { r.status = 307; r.header.Set("Location", "https://other.example/token") }},
		{"duplicate metadata", "oauth-authorization", func(r *testReply) { r.raw = []byte(`{"issuer":"one","issuer":"two"}`) }},
		{"oversized metadata", "oauth-authorization", func(r *testReply) { r.raw = []byte(`{"x":"` + strings.Repeat("x", 65536) + `"}`) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newPeer(t, func(path string, reply *testReply) {
				if strings.Contains(path, test.route) {
					test.change(reply)
				}
			})
			if err := p.client.Authenticate(t.Context()); err == nil {
				t.Fatal("accepted incompatible metadata")
			}
			if p.tokens.Load() != 0 || p.protected.Load() != 0 {
				t.Fatal("credentials crossed failed metadata boundary")
			}
		})
	}
}

func TestTokenResponseAndNonceBounds(t *testing.T) {
	for _, field := range []string{"token_type", "expires_in", "scope", "refresh_token", "access_token", "cache", "pragma", "nonce"} {
		t.Run(field, func(t *testing.T) {
			p := newPeer(t, func(path string, r *testReply) {
				if path != "/token" {
					return
				}
				value := r.value.(map[string]any)
				switch field {
				case "token_type":
					value[field] = "Bearer"
				case "expires_in":
					value[field] = 301
				case "scope":
					value[field] = "shrimp.read shrimp.write"
				case "refresh_token":
					value[field] = "must-not-be-accepted"
				case "access_token":
					value[field] = "token\r\ninjected-header"
				case "cache":
					r.header.Del("Cache-Control")
				case "pragma":
					r.header.Del("Pragma")
				case "nonce":
					r.status = 400
					r.value = map[string]any{"error": "use_dpop_nonce"}
					r.header.Set("DPoP-Nonce", "always-retry")
				}
			})
			if err := p.client.Authenticate(t.Context()); err == nil {
				t.Fatal("accepted invalid token response")
			}
			want := int32(1)
			if field == "nonce" {
				want = 2
			}
			if p.tokens.Load() != want || p.protected.Load() != 0 {
				t.Fatal("unbounded retry or leaked token")
			}
		})
	}
}

func TestDiscoveryTrustAndSchemaBoundaries(t *testing.T) {
	for _, kind := range []string{"scope", "resource", "duplicate version", "schema path", "hash", "external ref", "version", "cache"} {
		t.Run(kind, func(t *testing.T) {
			p := newPeer(t, func(path string, r *testReply) {
				if strings.HasSuffix(path, "/capabilities") {
					doc := r.value.(map[string]any)
					versions := doc["versions"].([]any)
					catalog := versions[0].(map[string]any)["schemas"].([]any)
					switch kind {
					case "scope":
						doc["scope"].(map[string]any)["domain"] = "different"
					case "resource":
						doc["resource"] = "https://other.example/resource"
					case "duplicate version":
						doc["versions"] = append(versions, versions[0])
					case "schema path":
						catalog[0].(map[string]any)["path"] = "/schemas/../token.json"
					case "hash":
						catalog[0].(map[string]any)["sha256"] = strings.Repeat("0", 64)
					case "external ref":
						for _, entry := range catalog {
							d := entry.(map[string]any)
							if strings.HasSuffix(d["path"].(string), "/problem.schema.json") {
								d["sha256"] = fmt.Sprintf("%x", sha256.Sum256(externalSchema()))
							}
						}
					}
				}
				if strings.Contains(path, "/schemas/") {
					switch kind {
					case "version":
						r.header.Set("SHRIMP-Version", "different")
					case "cache":
						r.header.Del("Cache-Control")
					case "external ref":
						if strings.HasSuffix(path, "/problem.schema.json") {
							r.raw = externalSchema()
						}
					}
				}
			})
			if err := p.client.Authenticate(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := p.client.Discover(t.Context()); err == nil {
				t.Fatal("accepted incompatible discovery/schema")
			}
		})
	}
}

func externalSchema() []byte {
	return []byte(`{"$id":"` + schemaBase + `problem.schema.json","$schema":"` + draft + `","$ref":"https://other.example/schema.json"}`)
}

func TestReceiptIdentityAndUncertainOutcomes(t *testing.T) {
	for _, kind := range []string{"id", "replay_window", "href", "state", "resources", "not found", "nonce"} {
		t.Run(kind, func(t *testing.T) {
			p := newPeer(t, func(path string, r *testReply) {
				if !strings.Contains(path, "/operations/") {
					return
				}
				v := r.value.(map[string]any)
				switch kind {
				case "id", "replay_window", "href":
					v["operation"].(map[string]any)[kind] = "different"
				case "state":
					v["state"] = "failed"
				case "resources":
					v["commit"].(map[string]any)["resources"] = []any{}
				case "not found", "nonce":
					r.status = 404
					r.header.Set("Content-Type", "application/problem+json")
					r.value = map[string]any{"type": "about:blank", "title": "Unavailable", "code": "not_found", "status": 404,
						"recovery": map[string]any{"action": "inspect"}, "commit": "unknown"}
					if kind == "nonce" {
						r.status = 401
						r.value.(map[string]any)["status"] = 401
						r.value.(map[string]any)["code"] = "use_dpop_nonce"
						r.header.Set("WWW-Authenticate", `DPoP error="use_dpop_nonce"`)
						r.header.Set("DPoP-Nonce", "always-retry")
					}
				}
			})
			if err := p.client.Authenticate(t.Context()); err != nil {
				t.Fatal(err)
			}
			_, err := p.client.Inspect(t.Context(), "window-1", "op-1")
			if err == nil {
				t.Fatal("accepted invalid receipt")
			}
			if kind == "not found" && !strings.Contains(err.Error(), "outcome remains unknown") {
				t.Fatal(err)
			}
			if kind == "nonce" && p.protected.Load() != 2 {
				t.Fatal("unbounded resource nonce retry")
			}
		})
	}
}

func TestRetainedReceiptSurvivesNewWorkCatalogWithdrawal(t *testing.T) {
	p := newPeer(t, nil)
	if err := p.client.Authenticate(t.Context()); err != nil {
		t.Fatal(err)
	}
	// This invocation saw a current catalog supporting only group receipts.
	// Its previously accepted human operation still has the original 0.1 decoder.
	p.client.catalog = map[string]*jsonschema.Schema{"group-receipt.schema.json": p.client.local["group-receipt.schema.json"]}
	if _, err := p.client.Inspect(t.Context(), "window-1", "op-1"); err != nil {
		t.Fatal(err)
	}
}

func TestTLSVerificationAndCancellation(t *testing.T) {
	p := newPeer(t, nil)
	profile := p.client.profile
	profile.CAFile = ""
	c, err := New(profile)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Authenticate(t.Context()); err == nil || !strings.Contains(err.Error(), "TLS certificate") {
		t.Fatal("untrusted TLS accepted", err)
	}
	if p.tokens.Load() != 0 {
		t.Fatal("sent credentials without TLS trust")
	}
	// Read failures do not include arbitrary response bodies or credentials.
	_, err = p.client.request(t.Context(), "GET", "https://outside.example", "", make(http.Header), 65536)
	if err == nil || strings.Contains(err.Error(), "opaque-secret") {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := p.client.Authenticate(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation was not preserved", err)
	}
}
