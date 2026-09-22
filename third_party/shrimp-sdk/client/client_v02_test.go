package client

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/client/internal/schemas"
)

func contractVector(t *testing.T, id string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors []struct {
		ID    string
		Value json.RawMessage
	}
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, vector := range vectors {
		if vector.ID == id {
			value, err := decodeJSON(vector.Value)
			if err != nil {
				t.Fatal(err)
			}
			return value.(map[string]any)
		}
	}
	t.Fatalf("missing contract vector %s", id)
	return nil
}

func discoveryFixture02(t *testing.T) map[string]any {
	t.Helper()
	doc := contractVector(t, "discovery-baseline-without-optional-apis")
	version := doc["versions"].([]any)[0].(map[string]any)
	files, err := schemas.Files.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	catalog := []any{}
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), "-v0.2.schema.json") && file.Name() != "human-attributes-v1.schema.json" && file.Name() != "enterprise-attributes-v1.schema.json" {
			continue
		}
		raw, err := schemas.Files.ReadFile(file.Name())
		if err != nil {
			t.Fatal(err)
		}
		catalog = append(catalog, map[string]any{"id": "https://shrimp.example/schemas/0.2/" + file.Name(),
			"path": "/schemas/" + file.Name(), "sha256": fmt.Sprintf("%x", sha256.Sum256(raw))})
	}
	version["schemas"] = catalog
	return doc
}

func TestDiscovery02Profiles(t *testing.T) {
	for _, test := range []struct {
		name     string
		required []string
		declared []any
		wantErr  bool
	}{
		{"default baseline", nil, []any{"baseline", "human"}, false},
		{"default rejects partial", nil, []any{}, true},
		{"explicit partial", []string{}, []any{}, false},
		{"missing required profile", []string{"roles"}, []any{"baseline", "human"}, true},
		{"unknown required profile", []string{"vendor-profile"}, []any{"baseline", "human"}, true},
		{"dependency incomplete", []string{}, []any{"baseline", "human", "groups"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newVersionPeer(t, "0.2", test.required, func(path string, r *testReply) {
				if strings.HasSuffix(path, "/capabilities") {
					r.value.(map[string]any)["versions"].([]any)[0].(map[string]any)["profiles"] = test.declared
				}
			})
			if err := p.client.Authenticate(t.Context()); err != nil {
				t.Fatal(err)
			}
			diagnostics, err := p.client.Discover(t.Context())
			if (err != nil) != test.wantErr {
				t.Fatalf("discovery error = %v, want error %v", err, test.wantErr)
			}
			if err == nil && (diagnostics.Version != "0.2" || diagnostics.Profiles == nil ||
				!slices.Equal(diagnostics.RequiredProfiles, p.client.profile.DiscoveryProfiles())) {
				t.Fatalf("incorrect negotiated profile diagnostics: %+v", diagnostics)
			}
			// Recovery does not depend on the current declaration, including a
			// failed baseline requirement or unknown requested optional profile.
			if _, err := p.client.Inspect(t.Context(), "window-1", "op-1"); err != nil {
				t.Fatal(err)
			}
			if err := p.client.LifecycleReady(); err == nil {
				t.Fatal("advertised 0.2 support enabled legacy lifecycle mutations")
			}
		})
	}
}

func TestDiscovery02RejectsDowngradeAndCatalogChanges(t *testing.T) {
	for _, fault := range []string{"missing echo", "duplicate echo", "wrong echo", "legacy body", "wrong scope", "duplicate version",
		"mtls", "missing operation", "duplicate schema", "legacy catalog", "missing receipt", "external reference", "weakened receipt"} {
		t.Run(fault, func(t *testing.T) {
			broken := false
			p := newVersionPeer(t, "0.2", nil, func(path string, r *testReply) {
				if !broken {
					return
				}
				if strings.HasSuffix(path, "/capabilities") {
					doc := r.value.(map[string]any)
					version := doc["versions"].([]any)[0].(map[string]any)
					catalog := version["schemas"].([]any)
					switch fault {
					case "missing echo":
						r.header.Del("SHRIMP-Discovery-Version")
					case "duplicate echo":
						r.header.Add("SHRIMP-Discovery-Version", "0.2")
					case "wrong echo":
						r.header.Set("SHRIMP-Discovery-Version", "0.1")
					case "legacy body":
						doc["discovery_version"] = "0.1"
					case "wrong scope":
						doc["scope"].(map[string]any)["domain"] = "B"
					case "duplicate version":
						doc["versions"] = append(doc["versions"].([]any), version)
					case "mtls":
						version["authentication_profile"] = "oauth-client-credentials-mtls-0.1"
					case "missing operation":
						version["profiles"] = []any{}
						version["operations"] = []any{"resource.read"}
					case "duplicate schema":
						version["schemas"] = append(catalog, catalog[0])
					case "legacy catalog":
						catalog[0].(map[string]any)["id"] = schemaBase + "audit-v0.2.schema.json"
					case "missing receipt":
						version["schemas"] = slices.DeleteFunc(catalog, func(item any) bool {
							return item.(map[string]any)["path"] == "/schemas/receipt-v0.2.schema.json"
						})
					case "external reference", "weakened receipt":
						for _, entry := range catalog {
							if entry.(map[string]any)["path"] == "/schemas/receipt-v0.2.schema.json" {
								entry.(map[string]any)["sha256"] = fmt.Sprintf("%x", sha256.Sum256(changedReceiptSchema02(fault)))
							}
						}
					}
				} else if strings.HasSuffix(path, "/schemas/receipt-v0.2.schema.json") && (fault == "external reference" || fault == "weakened receipt") {
					r.raw = changedReceiptSchema02(fault)
				} else if strings.Contains(path, "/operations/") && fault == "weakened receipt" {
					r.value = map[string]any{"state": "succeeded"}
				}
			})
			c := p.client
			if err := c.Authenticate(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := c.Discover(t.Context()); err != nil {
				t.Fatal(err)
			}
			broken = true
			_, err := c.Discover(t.Context())
			if fault == "weakened receipt" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := c.Inspect(t.Context(), "window-1", "op-1"); err == nil {
					t.Fatal("current catalog weakened the retained receipt decoder")
				}
				return
			}
			if err == nil {
				t.Fatal("accepted invalid 0.2 discovery")
			}
			if c.selected != nil || c.catalog != nil || c.versions != nil || c.schemaDigests != nil {
				t.Fatal("failed rediscovery retained a stale selection")
			}
			if _, err := c.Check(t.Context(), ReplayedProof); err == nil {
				t.Fatal("probe used a stale discovery selection")
			}
			if _, err := c.Inspect(t.Context(), "window-1", "op-1"); err != nil {
				t.Fatal("failed rediscovery prevented recovery", err)
			}
		})
	}
}

func changedReceiptSchema02(fault string) []byte {
	doc := map[string]any{"$id": "https://shrimp.example/schemas/0.2/receipt-v0.2.schema.json", "$schema": draft}
	if fault == "external reference" {
		doc["$ref"] = "https://other.example/schema.json"
	}
	raw, _ := json.Marshal(doc)
	return raw
}

func TestReceipt02RejectsScopeAndInvalidEvidence(t *testing.T) {
	for _, fault := range []string{"resource", "tenant", "domain", "scope version", "wire version", "id", "window", "legacy receipt",
		"invalid time", "terminal before acceptance", "short retention", "overflow retention", "unknown terminal commit", "success with pending effect"} {
		t.Run(fault, func(t *testing.T) {
			p := newVersionPeer(t, "0.2", nil, func(path string, r *testReply) {
				if !strings.Contains(path, "/operations/") {
					return
				}
				v := r.value.(map[string]any)
				switch fault {
				case "resource", "tenant", "domain":
					v["scope"].(map[string]any)[fault] = "https://different.example"
				case "scope version":
					v["scope"].(map[string]any)["schema_version"] = "0.1"
				case "wire version":
					r.header.Set("SHRIMP-Version", "0.1")
				case "id":
					v["operation"].(map[string]any)["id"] = "other"
				case "window":
					v["operation"].(map[string]any)["replay_window"] = "other"
				case "legacy receipt":
					delete(v, "schema_version")
				case "invalid time":
					v["accepted_at"] = "2026-02-30T12:00:00Z"
				case "terminal before acceptance":
					v["terminal_at"] = "2020-01-01T00:00:00Z"
				case "short retention":
					v["results_retained_until"] = v["terminal_at"]
				case "overflow retention":
					v["min_result_retention_seconds"] = json.Number("999999999999999999999999")
				case "unknown terminal commit":
					v["commit"].(map[string]any)["state"] = "unknown"
				case "success with pending effect":
					v["effects"] = []any{map[string]any{"state": "pending"}}
				}
			})
			if err := p.client.Authenticate(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := p.client.Inspect(t.Context(), "window-1", "op-1"); err == nil {
				t.Fatal("accepted invalid retained evidence")
			}
		})
	}
}

func TestReceipt02PreservesPendingFailureAndSupersession(t *testing.T) {
	for _, id := range []string{"pending-unknown-outcome", "committed-pending-effect", "postcommit-effect-failure",
		"known-precommit-failure", "explicit-supersession", "erasure-receipt-policy"} {
		t.Run(id, func(t *testing.T) {
			var expected map[string]any
			p := newVersionPeer(t, "0.2", nil, func(path string, r *testReply) {
				if !strings.Contains(path, "/operations/") {
					return
				}
				expected = contractVector(t, id)
				expected["scope"] = r.value.(map[string]any)["scope"]
				expected["operation"] = r.value.(map[string]any)["operation"]
				r.value = expected
			})
			if err := p.client.Authenticate(t.Context()); err != nil {
				t.Fatal(err)
			}
			receipt, err := p.client.Inspect(t.Context(), "window-1", "op-1")
			if err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(receipt)
			want, _ := json.Marshal(expected)
			if string(got) != string(want) {
				t.Fatal("reader rewrote the retained outcome")
			}
		})
	}
}

func TestReceipt02AcceptsIntegerRetentionInExponentNotation(t *testing.T) {
	p := newVersionPeer(t, "0.2", nil, func(path string, r *testReply) {
		if strings.Contains(path, "/operations/") {
			r.value.(map[string]any)["min_result_retention_seconds"] = json.Number("8.64e4")
		}
	})
	if err := p.client.Authenticate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := p.client.Inspect(t.Context(), "window-1", "op-1"); err != nil {
		t.Fatal(err)
	}
}

func TestReceipt02UnavailableStaysUnknown(t *testing.T) {
	for _, code := range []int{401, 403, 404, 410, 503} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			p := newVersionPeer(t, "0.2", nil, func(path string, r *testReply) {
				if !strings.Contains(path, "/operations/") {
					return
				}
				r.status = code
				r.header.Set("Content-Type", "application/problem+json")
				stage := "recovery"
				if code == 401 {
					stage = "authentication"
				} else if code == 403 {
					stage = "authorization"
				}
				r.value = map[string]any{"type": "about:blank", "title": "Unavailable", "status": code,
					"code": "unavailable", "stage": stage, "commit": "unknown", "operation": nil, "command_id": nil,
					"recovery": map[string]any{"action": "inspect", "retry_after_seconds": nil}}
			})
			if err := p.client.Authenticate(t.Context()); err != nil {
				t.Fatal(err)
			}
			_, err := p.client.Inspect(t.Context(), "window-1", "op-1")
			var unavailable *UnavailableError
			if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "outcome remains unknown") {
				t.Fatalf("missing result was not left unknown: %v", err)
			}
		})
	}
}

func TestProblem02LegacyShapeOnlyBeforeVersionSelection(t *testing.T) {
	c := newVersionPeer(t, "0.2", nil, nil).client
	for _, test := range []struct {
		name    string
		status  int
		code    string
		version string
		valid   bool
	}{
		{"missing version", 400, "version_required", "", true},
		{"unknown version", 400, "unsupported_version", "", true},
		{"version already selected", 400, "unsupported_version", "0.2", false},
		{"unrelated rejection", 400, "invalid_request", "", false},
		{"authentication cannot downgrade", 401, "invalid_dpop_proof", "", false},
		{"recovery cannot downgrade", 410, "operation_result_unavailable", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			headers := make(http.Header)
			headers.Set("Content-Type", "application/problem+json")
			if test.version != "" {
				headers.Set("SHRIMP-Version", test.version)
			}
			r := response{status: test.status, header: headers, value: map[string]any{
				"type": "about:blank", "title": "Rejected", "status": json.Number(fmt.Sprint(test.status)),
				"code": test.code, "commit": "unknown", "recovery": map[string]any{"action": "inspect"},
			}}
			if c.validProblem(r) != test.valid {
				t.Fatal("legacy error crossed the version-selection boundary")
			}
		})
	}
}
