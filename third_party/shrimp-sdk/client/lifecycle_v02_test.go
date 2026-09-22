package client

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestLifecycle02BindsIntentScopeAndOriginalRetention(t *testing.T) {
	for _, fault := range []string{"none", "no journal", "reply lost", "wrong operation", "wrong location", "wrong subject", "wrong command", "unchanged receipt revision", "missing block", "wrong effect", "stale read", "unchanged read revision", "wrong read scope", "malformed read", "wrong window scope", "short window", "reduced minimum", "late terminal", "missing definition"} {
		t.Run(fault, func(t *testing.T) {
			profile := testProfile(t)
			profile.Version, profile.RequiredProfiles = "0.2", []string{}
			c, err := New(profile)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.token, c.writeScope = "test-token", true
			c.catalog = map[string]*jsonschema.Schema{}
			for k, v := range c.local {
				c.catalog[k] = v
			}
			c.selected = &contract{Operations: []string{"resource.read", "mutation.submit", "operation.read", "replay_window.read"}}
			c.selected.Limits.MaxDepth, c.selected.Limits.MaxCommands, c.selected.Limits.MaxDependencies, c.selected.Limits.MaxAhead = 16, 1, 16, 60
			c.selected.Limits.MaxRequestBytes, c.selected.Limits.MinRetention02 = 65536, 86400
			if fault == "missing definition" {
				delete(c.catalog, mutation02)
			}
			journal := filepath.Join(t.TempDir(), "intents")
			if fault == "no journal" {
				if err := os.WriteFile(journal, []byte("blocked"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			posts := 0
			c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				headers := http.Header{"Content-Type": []string{"application/json"}, "Cache-Control": []string{"no-store"}, "Shrimp-Version": []string{"0.2"}}
				scope := map[string]any{"resource": profile.Resource, "tenant": profile.Tenant, "domain": profile.Domain, "schema_version": "0.2", "history_epoch": "epoch-1", "authorization_context": "hr-authority"}
				var value map[string]any
				switch {
				case strings.HasSuffix(r.URL.Path, "/reads"):
					raw, _ := io.ReadAll(r.Body)
					request, err := decodeJSON(raw)
					if err != nil || c.local[readRequest02].Validate(request) != nil {
						t.Fatal("invalid typed read", err)
					}
					state, revision := "active", "r1"
					if posts > 0 {
						deps := request.(map[string]any)["required_dependencies"].([]any)
						if len(deps) != 1 || deps[0] != "opaque-token" {
							t.Fatal("missing original receipt dependency")
						}
						state, revision = "disabled", "r2"
						if fault == "stale read" {
							state = "active"
						}
						if fault == "unchanged read revision" {
							revision = "r1"
						}
					}
					record := map[string]any{"type": "subject", "id": "s1", "revision": revision, "authority": "hr-authority", "deleted": false,
						"value": map[string]any{"profile": "human", "lifecycle": state, "attributes": map[string]any{}, "expires_at": nil}}
					value = map[string]any{"scope": scope, "record": record, "observed_frontier": "frontier", "state_validator": "validator", "validator_expires_at": "2026-09-18T00:01:00Z"}
					if fault == "wrong read scope" {
						scope["resource"] = "https://other.example"
					}
					if fault == "malformed read" {
						delete(value, "record")
					}
				case strings.HasSuffix(r.URL.Path, "/replay-window"):
					value = map[string]any{"schema_version": "0.2", "scope": scope, "replay_window": "w1", "server_time": "2026-09-18T00:00:00Z", "closes_at": "2026-09-18T00:10:00Z", "results_retained_until": "2026-09-20T00:10:00Z", "min_result_retention_seconds": 172800}
					if fault == "wrong window scope" {
						scope["domain"] = "B"
					}
					if fault == "short window" {
						value["results_retained_until"] = "2026-09-19T00:10:00Z"
					}
				case strings.HasSuffix(r.URL.Path, "/mutations"):
					posts++
					raw, _ := io.ReadAll(r.Body)
					request, err := decodeJSON(raw)
					if err != nil || c.local[mutation02].Validate(request) != nil {
						t.Fatal("invalid 0.2 mutation", err)
					}
					body := request.(map[string]any)
					op := body["operation"].(map[string]any)
					if body["expected_authority"] != "hr-authority" || len(body["required_profiles"].([]any)) != 0 {
						t.Fatal("lost authority or explicit partial selection")
					}
					saved, err := os.ReadFile(filepath.Join(journal, op["id"].(string)+".json"))
					if err != nil {
						t.Fatal("sent before durable intent", err)
					}
					var entry map[string]any
					if err := json.Unmarshal(saved, &entry); err != nil {
						t.Fatal(err)
					}
					original, _ := json.Marshal(entry["request"])
					sent, _ := json.Marshal(body)
					if string(original) != string(sent) || entry["version"] != "0.2" || entry["replay_window_evidence"].(map[string]any)["min_result_retention_seconds"] != float64(172800) {
						t.Fatal("saved intent lost original request or retention")
					}
					if fault == "reply lost" {
						return nil, errors.New("response lost")
					}
					headers.Set("Location", strings.TrimSuffix(r.URL.Path, "/mutations")+"/operations/w1/"+op["id"].(string))
					value = contractVector(t, "receipt-valid")
					value["scope"], value["operation"] = scope, op
					value["accepted_at"], value["terminal_at"] = "2026-09-18T00:00:00Z", "2026-09-18T00:00:01Z"
					value["results_retained_until"], value["min_result_retention_seconds"] = "2026-09-20T00:10:00Z", 172800
					resource := map[string]any{"command_id": "c1", "resource": map[string]any{"type": "subject", "id": "s1"}, "revision": "r2"}
					value["commit"] = map[string]any{"state": "committed", "causal_token": "opaque-token", "resources": []any{resource}}
					effect := map[string]any{"id": "e1", "kind": "admission_block", "resource": map[string]any{"type": "subject", "id": "s1"}, "consumer": "memos", "state": "complete", "deadline": "2026-09-18T00:00:01Z", "observed_frontier": "opaque-token", "error": nil}
					value["effects"] = []any{effect}
					switch fault {
					case "wrong operation":
						op["id"] = "other"
					case "wrong location":
						headers.Set("Location", "/other")
					case "wrong subject":
						resource["resource"].(map[string]any)["id"] = "s2"
					case "wrong command":
						resource["command_id"] = "another"
					case "unchanged receipt revision":
						resource["revision"] = "r1"
					case "missing block":
						value["effects"] = []any{}
					case "wrong effect":
						effect["kind"] = "binding_fence"
					case "reduced minimum":
						value["min_result_retention_seconds"] = 86400
					case "late terminal":
						value["min_result_retention_seconds"] = 86400
						value["terminal_at"] = "2026-09-18T00:11:00Z"
					}
				default:
					t.Fatal("unexpected route", r.URL.Path)
				}
				raw, _ := json.Marshal(value)
				return &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
			})
			err = c.Lifecycle(t.Context(), "s1", "disable", testSaver(journal))
			if fault == "none" {
				if err != nil || posts != 1 {
					t.Fatal(posts, err)
				}
				return
			}
			if err == nil || posts > 1 {
				t.Fatal("fault missed or original intent resubmitted", posts, err)
			}
			switch fault {
			case "no journal", "wrong read scope", "malformed read", "wrong window scope", "short window", "missing definition":
				if posts != 0 {
					t.Fatal("mutation sent without valid preconditions")
				}
			}
			var unavailable *UnavailableError
			wantUnavailable := fault == "reply lost" || fault == "no journal" || fault == "missing definition"
			if errors.As(err, &unavailable) != wantUnavailable {
				t.Fatal("incorrect evidence classification", err)
			}
		})
	}
}
