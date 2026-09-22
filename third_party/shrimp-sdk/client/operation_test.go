package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func operationClient(t *testing.T) *Client {
	t.Helper()
	p := testProfile(t)
	p.Version = "0.2"
	p.RequiredProfiles = []string{}
	c, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	c.token, c.writeScope = "fixture-token", true
	c.catalog = c.local
	c.selected = &contract{Operations: []string{"resource.read", "mutation.submit", "operation.read", "replay_window.read"}}
	c.selected.Limits.MaxDepth = 16
	c.selected.Limits.MaxCommands = 1
	c.selected.Limits.MaxDependencies = 16
	c.selected.Limits.MaxAhead = 120
	c.selected.Limits.MaxRequestBytes = 65536
	c.selected.Limits.MinRetention02 = 86400
	return c
}
func operationScope(c *Client) map[string]any {
	return map[string]any{"resource": c.profile.Resource, "tenant": c.profile.Tenant, "domain": c.profile.Domain, "schema_version": "0.2", "history_epoch": "epoch-1", "authorization_context": "hr-authority"}
}
func operationWindow(c *Client) map[string]any {
	return map[string]any{"schema_version": "0.2", "scope": operationScope(c), "replay_window": "w1", "server_time": "2026-09-18T00:00:00Z", "closes_at": "2026-09-18T00:10:00Z", "results_retained_until": "2026-09-20T00:10:00Z", "min_result_retention_seconds": 172800}
}
func operationResponse(value any, location string) *http.Response {
	raw, _ := json.Marshal(value)
	h := http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}, "Shrimp-Version": {"0.2"}}
	if location != "" {
		h.Set("Location", location)
	}
	return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(bytes.NewReader(raw))}
}
func TestPreparedOperationsSurviveLostReplyAndClientRestart(t *testing.T) {
	for _, action := range []string{"create_subject", "activate", "disable", "update_subject"} {
		t.Run(action, func(t *testing.T) {
			c := operationClient(t)
			var saved, first []byte
			var receipt map[string]any
			posts := 0
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/replay-window") {
					return operationResponse(operationWindow(c), ""), nil
				}
				if strings.Contains(r.URL.Path, "/operations/") {
					return operationResponse(receipt, ""), nil
				}
				if !strings.HasSuffix(r.URL.Path, "/mutations") {
					t.Fatalf("unexpected route %s", r.URL.Path)
				}
				posts++
				raw, _ := io.ReadAll(r.Body)
				if len(saved) == 0 {
					t.Fatal("submitted without saved intent")
				}
				if posts == 1 {
					first = raw
				} else if !bytes.Equal(first, raw) {
					t.Fatal("retry changed request bytes")
				}
				value, err := decodeJSON(raw)
				if err != nil {
					t.Fatal(err)
				}
				request := value.(map[string]any)
				command := request["commands"].([]any)[0].(map[string]any)
				if command["action"] != action || request["expected_authority"] != "hr-authority" {
					t.Fatal("changed decision")
				}
				if action == "create_subject" && (command["source_reference"] != "employee-42" || command["profile"] != "human") {
					t.Fatal("lost source binding")
				}
				receipt = contractVector(t, "receipt-valid")
				receipt["scope"] = operationScope(c)
				receipt["operation"] = request["operation"]
				receipt["accepted_at"], receipt["terminal_at"] = "2026-09-18T00:00:00Z", "2026-09-18T00:00:01Z"
				receipt["results_retained_until"], receipt["min_result_retention_seconds"] = "2026-09-20T00:10:00Z", 172800
				receipt["commit"] = map[string]any{"state": "committed", "causal_token": "token", "resources": []any{map[string]any{"command_id": "c1", "resource": map[string]any{"type": "subject", "id": "s1"}, "revision": "r2"}}}
				if action == "create_subject" {
					commit := receipt["commit"].(map[string]any)
					commit["resources"] = append(commit["resources"].([]any), map[string]any{"command_id": "c1", "resource": map[string]any{"type": "source_reference", "id": "source-1"}, "revision": "source-r1"})
				}
				receipt["effects"] = []any{}
				if action == "disable" {
					receipt["effects"] = []any{map[string]any{"id": "e1", "kind": "admission_block", "resource": map[string]any{"type": "subject", "id": "s1"}, "consumer": "memos", "state": "complete", "deadline": "2026-09-18T00:00:01Z", "observed_frontier": "token", "error": nil}}
				}
				if posts == 1 {
					return nil, errors.New("reply lost after commit")
				}
				return operationResponse(receipt, strings.TrimSuffix(r.URL.Path, "/mutations")+"/operations/w1/"+request["operation"].(map[string]any)["id"].(string)), nil
			})
			c.http.Transport = transport
			var intent Intent
			var err error
			s := SubjectVersion{ID: "s1", Revision: "r1", Authority: "hr-authority"}
			switch action {
			case "create_subject":
				intent, err = c.PrepareCreate(t.Context(), Human{Authority: "hr-authority", SourceReference: "employee-42", DisplayName: "Alice"})
			case "activate":
				intent, err = c.PrepareActivate(t.Context(), s)
			case "update_subject":
				intent, err = c.PrepareUpdate(t.Context(), s, ScalarChanges{Set: map[string]string{"department": "Research"}, Clear: []string{"email"}})
			case "disable":
				intent, err = c.PrepareDisable(t.Context(), s)
			}
			if err != nil {
				t.Fatal(err)
			}
			saver := func(_ context.Context, id string, raw []byte) error {
				if id != intent.ID() {
					t.Fatal("changed identity")
				}
				if len(saved) > 0 && !bytes.Equal(saved, raw) {
					t.Fatal("changed saved intent")
				}
				saved = bytes.Clone(raw)
				return nil
			}
			if _, err := c.Submit(t.Context(), intent, nil); err == nil || posts != 0 {
				t.Fatal("nil saver sent mutation")
			}
			if _, err := c.Submit(t.Context(), intent, func(context.Context, string, []byte) error { return errors.New("disk full") }); err == nil || posts != 0 {
				t.Fatal("failed saver sent mutation")
			}
			if _, err := c.Submit(t.Context(), intent, saver); err == nil || posts != 1 {
				t.Fatal("expected uncertain first attempt")
			}
			// A restarted client uses persisted intent, even after new-work discovery is withdrawn.
			restarted := operationClient(t)
			restarted.profile = c.profile
			restarted.selected = nil
			restarted.catalog = nil
			restarted.http.Transport = transport
			restored, err := restarted.RestoreIntent(saved)
			if err != nil {
				t.Fatal(err)
			}
			leaked := restored.Bytes()
			leaked[0] = '!'
			if _, err := restarted.RestoreIntent(restored.Bytes()); err != nil {
				t.Fatal("intent is mutable")
			}
			got, err := restarted.Recover(t.Context(), restored)
			if err != nil || got["state"] != "succeeded" || posts != 1 {
				t.Fatalf("recovery: %v %v", got, err)
			}
			if _, err := restarted.Submit(t.Context(), restored, saver); err != nil || posts != 2 {
				t.Fatalf("exact retry: %v", err)
			}
			for _, field := range []string{"resource", "issuer", "tenant", "domain", "client_id", "version"} {
				var entry map[string]any
				json.Unmarshal(saved, &entry)
				entry[field] = "changed"
				bad, _ := json.Marshal(entry)
				if _, err := restarted.RestoreIntent(bad); err == nil {
					t.Fatalf("accepted changed %s", field)
				}
			}
			receipt["results_retained_until"] = "2026-09-20T00:05:00Z"
			if _, err := restarted.Recover(t.Context(), restored); err == nil {
				t.Fatal("accepted shortened original retention")
			}
		})
	}
}

func TestPrepareRequiresExplicitDecision(t *testing.T) {
	c := operationClient(t)
	c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid decision contacted peer")
		return nil, nil
	})
	if _, err := c.PrepareCreate(t.Context(), Human{}); err == nil {
		t.Fatal("accepted missing authority/source")
	}
	if _, err := c.PrepareActivate(t.Context(), SubjectVersion{ID: "s1"}); err == nil {
		t.Fatal("accepted missing revision")
	}
	if _, err := c.Submit(t.Context(), Intent{}, nil); err == nil {
		t.Fatal("accepted zero intent")
	}
}

func TestSubmissionPreservesPendingAndFailure(t *testing.T) {
	c := operationClient(t)
	for _, id := range []string{"pending-unknown-outcome", "committed-pending-effect", "postcommit-effect-failure", "known-precommit-failure"} {
		t.Run(id, func(t *testing.T) {
			value := contractVector(t, id)
			r := response{status: 200, header: http.Header{"Content-Type": {"application/json"}}, value: value}
			if value["state"] == "pending" {
				r.status = 202
				r.header.Set("Retry-After", value["poll_after_seconds"].(json.Number).String())
			}
			if err := c.submissionReceipt(r); err != nil {
				t.Fatal(err)
			}
			if value["state"] == "pending" {
				r.header.Set("Retry-After", "999999")
				if err := c.submissionReceipt(r); err == nil {
					t.Fatal("accepted changed polling bound")
				}
				r.status = 200
				if err := c.submissionReceipt(r); err == nil {
					t.Fatal("accepted pending receipt as terminal HTTP status")
				}
			}
		})
	}
}
