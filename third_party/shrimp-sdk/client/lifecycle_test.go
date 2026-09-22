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
)

func TestLifecycleDurableIntentAndReceiptControls(t *testing.T) {
	for _, fault := range []string{"none", "no journal", "reply lost", "wrong identity", "wrong subject", "unchanged revision", "missing block", "stale read"} {
		t.Run(fault, func(t *testing.T) {
			c, err := New(testProfile(t))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.token, c.writeScope, c.catalog = "test-token", true, c.local
			c.selected = &contract{Operations: []string{"subject.read", "subject.activate", "subject.disable", "operation.read", "replay_window.read"}}
			c.selected.Limits.MaxRequestBytes = 65536
			c.selected.Limits.MaxDepth, c.selected.Limits.MaxCommands, c.selected.Limits.MaxDependencies, c.selected.Limits.MaxAhead = 16, 1, 1, 60
			journal := filepath.Join(t.TempDir(), "intents")
			if fault == "no journal" {
				if err := os.WriteFile(journal, []byte("blocked"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			posts := 0
			c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				headers := http.Header{"Content-Type": []string{"application/json"}, "Cache-Control": []string{"no-store"}, "Shrimp-Version": []string{"0.1"}}
				var value any
				switch {
				case strings.HasSuffix(r.URL.Path, "/reads"):
					body, _ := io.ReadAll(r.Body)
					var request map[string]any
					_ = json.Unmarshal(body, &request)
					lifecycle, revision := "active", "r1"
					if posts > 0 {
						deps := request["required_dependencies"].([]any)
						if len(deps) != 1 || deps[0] != "opaque-token" {
							t.Fatal("causal receipt dependency not sent")
						}
						lifecycle, revision = "disabled", "r2"
						if fault == "stale read" {
							lifecycle = "active"
						}
					}
					value = map[string]any{"resource": map[string]any{"type": "subject", "id": "s1", "revision": revision, "value": map[string]any{
						"profile": "human", "lifecycle": lifecycle, "attributes": map[string]any{}, "source_references": []any{}}}, "observed_frontier": "frontier", "state_validator": "validator"}
				case strings.HasSuffix(r.URL.Path, "/replay-window"):
					value = map[string]any{"replay_window": "w1", "server_time": "2026-09-18T00:00:00Z", "closes_at": "2026-09-18T00:10:00Z", "results_retained_until": "2026-09-18T00:11:00Z"}
				case strings.HasSuffix(r.URL.Path, "/mutations"):
					posts++
					body, _ := io.ReadAll(r.Body)
					var request map[string]any
					_ = json.Unmarshal(body, &request)
					op := request["operation"].(map[string]any)
					path := filepath.Join(journal, op["id"].(string)+".json")
					saved, err := os.ReadFile(path)
					if err != nil {
						t.Fatal("mutation sent before intent persisted", err)
					}
					var entry map[string]any
					_ = json.Unmarshal(saved, &entry)
					exact, _ := json.Marshal(entry["request"])
					sent, _ := json.Marshal(request)
					if string(exact) != string(sent) || entry["resource"] != c.profile.Resource {
						t.Fatal("saved intent differs from sent mutation")
					}
					if fault == "reply lost" {
						return nil, errors.New("lost response")
					}
					href := strings.TrimSuffix(r.URL.Path, "/mutations") + "/operations/w1/" + op["id"].(string)
					headers.Set("Location", href)
					op["href"] = href
					if fault == "wrong identity" {
						op["id"] = "other"
					}
					id, revision := "s1", "r2"
					if fault == "wrong subject" {
						id = "s2"
					}
					if fault == "unchanged revision" {
						revision = "r1"
					}
					effects := []any{map[string]any{"kind": "admission_block", "resource": map[string]any{"type": "subject", "id": "s1"}, "state": "complete"}}
					if fault == "missing block" {
						effects = []any{}
					}
					value = map[string]any{"operation": op, "state": "succeeded", "commit": map[string]any{"state": "committed", "causal_token": "opaque-token",
						"resources": []any{map[string]any{"type": "subject", "id": id, "revision": revision}}}, "effects": effects, "error": nil}
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
			if err == nil {
				t.Fatal("fault not detected", fault)
			}
			if fault == "no journal" && posts != 0 {
				t.Fatal("sent mutation without durable intent")
			}
			if posts > 1 {
				t.Fatal("uncertain mutation was automatically resubmitted")
			}
			var unavailable *UnavailableError
			if (fault == "reply lost" || fault == "no journal") != errors.As(err, &unavailable) {
				t.Fatal("incorrect evidence classification", err)
			}
		})
	}
}

func TestRequestedScopeComparison(t *testing.T) {
	for _, tc := range []struct {
		name, got string
		want      bool
	}{
		{"ordered", "shrimp.read shrimp.write", true},
		{"reordered", "shrimp.write shrimp.read", true},
		{"extra grant", "shrimp.read shrimp.write shrimp.audit", false},
		{"missing grant", "shrimp.read", false},
		{"duplicate grant", "shrimp.read shrimp.write shrimp.write", false},
		{"invalid separator", "shrimp.read\tshrimp.write", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameScopes(tc.got, "shrimp.read shrimp.write"); got != tc.want {
				t.Fatal(got)
			}
		})
	}
}
