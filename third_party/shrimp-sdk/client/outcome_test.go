package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func outcomeIntent(t *testing.T, c *Client) Intent {
	t.Helper()
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/replay-window") {
			t.Fatalf("unexpected preparation request %s %s", r.Method, r.URL.Path)
		}
		return operationResponse(operationWindow(c), ""), nil
	})
	intent, err := c.PrepareDisable(t.Context(), SubjectVersion{ID: "s1", Revision: "r1", Authority: "hr-authority"})
	if err != nil {
		t.Fatal(err)
	}
	return intent
}

func outcomeReceipt(t *testing.T, c *Client, intent Intent, vector string) map[string]any {
	t.Helper()
	receipt := contractVector(t, vector)
	receipt["scope"] = operationScope(c)
	receipt["operation"] = map[string]any{"id": intent.ID(), "replay_window": intent.ReplayWindow()}
	receipt["accepted_at"] = "2026-09-18T00:00:00Z"
	if receipt["state"] != "pending" {
		receipt["terminal_at"] = "2026-09-18T00:00:01Z"
	}
	receipt["results_retained_until"], receipt["min_result_retention_seconds"] = "2026-09-20T00:10:00Z", json.Number("172800")
	commit := receipt["commit"].(map[string]any)
	if commit["state"] == "committed" {
		commit["resources"] = []any{map[string]any{"command_id": "c1", "resource": map[string]any{"type": "subject", "id": "s1"}, "revision": "r2"}}
	}
	if receipt["state"] == "succeeded" {
		receipt["effects"] = []any{}
	}
	for _, item := range receipt["effects"].([]any) {
		effect := item.(map[string]any)
		effect["deadline"] = "2026-09-18T00:00:01Z"
	}
	if commit["state"] == "committed" {
		// Admission is already fenced at commit. A separate projection may still
		// be pending or failed without undoing that fence or the committed state.
		receipt["effects"] = append(receipt["effects"].([]any), map[string]any{
			"id": "admission", "kind": "admission_block", "resource": map[string]any{"type": "subject", "id": "s1"},
			"consumer": "application", "state": "complete", "deadline": "2026-09-18T00:00:01Z", "observed_frontier": "fence-1", "error": nil,
		})
	}
	return receipt
}

func outcomeResponse(receipt map[string]any, path string, intent Intent) *http.Response {
	r := operationResponse(receipt, strings.TrimSuffix(path, "/mutations")+"/operations/"+intent.ReplayWindow()+"/"+intent.ID())
	if receipt["state"] == "pending" {
		r.StatusCode = http.StatusAccepted
		r.Header.Set("Retry-After", fmt.Sprint(receipt["poll_after_seconds"]))
	}
	return r
}

func outcomeJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func requireUnknownOutcome(t *testing.T, outcome Outcome, err error) {
	t.Helper()
	if err == nil || outcome.Receipt != nil || outcome.State() != OutcomeUnknown {
		t.Fatalf("untrusted response exposed an outcome: state=%q receipt=%v error=%v", outcome.State(), outcome.Receipt != nil, err)
	}
}

func TestOutcomeStateRequiresKnownCommitAndCompletedEffects(t *testing.T) {
	if (Outcome{}).State() != OutcomeUnknown || (Outcome{Receipt: &Receipt{}}).State() != OutcomeUnknown {
		t.Fatal("zero values imply a known result")
	}
	c := operationClient(t)
	intent := outcomeIntent(t, c)
	valid, err := typedOutcome(outcomeReceipt(t, c, intent, "receipt-valid"))
	if err != nil || valid.State() != OutcomeSucceeded {
		t.Fatalf("complete success: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*Receipt)
		want   OutcomeState
	}{
		{"unknown commit", func(r *Receipt) { r.Commit.State = CommitUnknown }, OutcomeUnknown},
		{"no commit", func(r *Receipt) { r.Commit.State = CommitNotCommitted }, OutcomeUnknown},
		{"missing commit token", func(r *Receipt) { r.Commit.CausalToken = nil }, OutcomeUnknown},
		{"no committed resource", func(r *Receipt) { r.Commit.Resources = nil }, OutcomeUnknown},
		{"unfinished effect", func(r *Receipt) { r.Effects[0].State = EffectPending }, OutcomeUnknown},
		{"effect failed", func(r *Receipt) { r.Effects[0].State = EffectFailed }, OutcomeUnknown},
		{"no completion observation", func(r *Receipt) { r.Effects[0].ObservedFrontier = nil }, OutcomeUnknown},
		{"no terminal evidence", func(r *Receipt) { r.TerminalAt = nil }, OutcomeUnknown},
		{"committed pending", func(r *Receipt) { r.State = OutcomePending }, OutcomePending},
		{"committed failure", func(r *Receipt) { r.State = OutcomeFailed }, OutcomeFailed},
		{"superseded history", func(r *Receipt) { r.State = OutcomeSuperseded }, OutcomeSuperseded},
		{"unknown state", func(r *Receipt) { r.State = "future-state" }, OutcomeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var receipt Receipt
			if err := json.Unmarshal(outcomeJSON(t, valid.Receipt), &receipt); err != nil {
				t.Fatal(err)
			}
			tc.change(&receipt)
			if got := (Outcome{Receipt: &receipt}).State(); got != tc.want {
				t.Fatalf("state=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestSubmitTypedPreservesReceiptStatesAndCommitDistinctions(t *testing.T) {
	for _, tc := range []struct {
		vector string
		state  OutcomeState
		commit CommitState
	}{
		{"pending-unknown-outcome", OutcomePending, CommitUnknown},
		{"pending-unknown-outcome", OutcomePending, CommitNotCommitted},
		{"committed-pending-effect", OutcomePending, CommitCommitted},
		{"known-precommit-failure", OutcomeFailed, CommitNotCommitted},
		{"postcommit-effect-failure", OutcomeFailed, CommitCommitted},
		{"explicit-supersession", OutcomeSuperseded, CommitNotCommitted},
		{"receipt-valid", OutcomeSucceeded, CommitCommitted},
	} {
		t.Run(tc.vector+"/"+string(tc.commit), func(t *testing.T) {
			c := operationClient(t)
			intent := outcomeIntent(t, c)
			receipt := outcomeReceipt(t, c, intent, tc.vector)
			if tc.state == OutcomePending && tc.commit == CommitNotCommitted {
				receipt["commit"].(map[string]any)["state"] = "not_committed"
			}
			saved := false
			c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				raw, err := io.ReadAll(r.Body)
				if !saved || err != nil || r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/mutations") || !bytes.Equal(raw, []byte(intent.body)) {
					t.Fatal("typed submission bypassed original intent persistence or changed the request")
				}
				return outcomeResponse(receipt, r.URL.Path, intent), nil
			})
			outcome, err := c.SubmitTyped(t.Context(), intent, func(_ context.Context, id string, raw []byte) error {
				if id != intent.ID() || !bytes.Equal(raw, intent.Bytes()) {
					t.Fatal("typed submission changed persisted intent")
				}
				saved = true
				return nil
			})
			if err != nil || outcome.Receipt == nil || outcome.State() != tc.state || outcome.Receipt.Commit.State != tc.commit {
				t.Fatalf("typed result: state=%q receipt=%+v err=%v", outcome.State(), outcome.Receipt, err)
			}
			r := outcome.Receipt
			if r.Operation.ID != intent.ID() || r.Operation.ReplayWindow != intent.ReplayWindow() || r.SchemaVersion != "0.2" || r.Scope.Resource != c.profile.Resource || r.Scope.AuthorizationContext != "hr-authority" || r.Scope.HistoryEpoch != "epoch-1" || r.MinResultRetentionSeconds.String() != "172800" || r.AcceptedAt.Format("2006-01-02T15:04:05Z") != receipt["accepted_at"] {
				t.Fatal("typed conversion lost scope, identity, time or retention")
			}
			if tc.state == OutcomePending {
				if r.TerminalAt != nil || r.PollAfterSeconds == nil || *r.PollAfterSeconds != 1 || r.Error != nil {
					t.Fatal("pending receipt lost nullable fields or polling guidance")
				}
			} else if r.TerminalAt == nil || r.PollAfterSeconds != nil {
				t.Fatal("terminal receipt lost terminal/null polling fields")
			}
			if tc.commit == CommitCommitted {
				if r.Commit.CausalToken == nil || len(r.Commit.Resources) != 1 || r.Commit.Resources[0] != (CommittedResource{CommandID: "c1", Resource: ResourceRef{Type: "subject", ID: "s1"}, Revision: "r2"}) {
					t.Fatal("committed resources or causal evidence changed")
				}
			} else if r.Commit.CausalToken != nil || len(r.Commit.Resources) != 0 {
				t.Fatal("noncommitted evidence invented a commit")
			}
			if tc.state == OutcomeFailed && (r.Error == nil || r.Error.Code == "" || r.Error.Recovery != "reconcile") {
				t.Fatal("recorded operation failure became a transport error or lost recovery guidance")
			}
			if tc.vector == "postcommit-effect-failure" && (r.Effects[0].State != EffectFailed || r.Effects[0].Error == nil || r.Effects[0].Error.Stage != "effect") {
				t.Fatal("committed failure lost its separate effect failure")
			}
			if tc.state == OutcomeSuperseded && (r.SupersededBy == nil || r.SupersededBy.ID != "op-2" || r.Error == nil) {
				t.Fatal("supersession lost replacement identity or became success")
			}
			decoded, err := decodeJSON(outcomeJSON(t, r))
			if err != nil || !reflect.DeepEqual(decoded, receipt) {
				t.Fatalf("typed receipt did not preserve all JSON fields: %v", err)
			}
		})
	}
}

func TestSubmitTypedReturnsUnknownWithoutTrustedReceipt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		lost   bool
	}{
		{"transport loss", nil, true},
		{"missing field", func(r map[string]any) { delete(r, "scope") }, false},
		{"wrong operation", func(r map[string]any) { r["operation"].(map[string]any)["id"] = "other" }, false},
		{"wrong tenant", func(r map[string]any) { r["scope"].(map[string]any)["tenant"] = "other" }, false},
		{"wrong committed subject", func(r map[string]any) {
			r["commit"].(map[string]any)["resources"].([]any)[0].(map[string]any)["resource"].(map[string]any)["id"] = "other"
		}, false},
		{"shortened original retention", func(r map[string]any) { r["results_retained_until"] = "2026-09-20T00:05:00Z" }, false},
		{"success with pending effect", func(r map[string]any) { r["effects"].([]any)[0].(map[string]any)["state"] = "pending" }, false},
		{"disable without admission evidence", func(r map[string]any) { r["effects"] = []any{} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := operationClient(t)
			intent := outcomeIntent(t, c)
			receipt := outcomeReceipt(t, c, intent, "receipt-valid")
			if tc.change != nil {
				tc.change(receipt)
			}
			c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if tc.lost {
					return nil, errors.New("reply lost after possible commit")
				}
				return outcomeResponse(receipt, r.URL.Path, intent), nil
			})
			outcome, err := c.SubmitTyped(t.Context(), intent, func(context.Context, string, []byte) error { return nil })
			requireUnknownOutcome(t, outcome, err)
		})
	}
	c := operationClient(t)
	intent := outcomeIntent(t, c)
	c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("failed persistence still sent a mutation")
		return nil, nil
	})
	outcome, err := c.SubmitTyped(t.Context(), intent, func(context.Context, string, []byte) error { return errors.New("disk full") })
	requireUnknownOutcome(t, outcome, err)
}

func TestRecoverTypedUsesOriginalIntentWithoutDiscoveryOrWriteAuthority(t *testing.T) {
	c := operationClient(t)
	intent := outcomeIntent(t, c)
	stored := intent.Bytes()
	receipt := outcomeReceipt(t, c, intent, "receipt-valid")
	restarted := operationClient(t)
	restarted.profile, restarted.selected, restarted.catalog, restarted.writeScope = c.profile, nil, nil, false
	var err error
	intent, err = restarted.RestoreIntent(stored)
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	restarted.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		reads++
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/operations/"+intent.ReplayWindow()+"/"+intent.ID()) {
			t.Fatalf("typed recovery changed operation or requested new-work state: %s %s", r.Method, r.URL.Path)
		}
		return operationResponse(receipt, ""), nil
	})
	outcome, err := restarted.RecoverTyped(t.Context(), intent)
	if err != nil || outcome.State() != OutcomeSucceeded || !bytes.Equal(intent.Bytes(), stored) || reads != 1 {
		t.Fatalf("read-only historical recovery: state=%q err=%v", outcome.State(), err)
	}
	// The original result is validated against the original window, even though
	// current discovery and write authority no longer exist.
	receipt["results_retained_until"] = "2026-09-20T00:05:00Z"
	outcome, err = restarted.RecoverTyped(t.Context(), intent)
	requireUnknownOutcome(t, outcome, err)
	restarted.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("result temporarily unavailable") })
	outcome, err = restarted.RecoverTyped(t.Context(), intent)
	requireUnknownOutcome(t, outcome, err)
}

func TestReceiptTypesPreserveOptionalErasureFieldsAndUnboundedNumbers(t *testing.T) {
	// This tests the typed representation, not acceptance of an impossible
	// retention promise. Raw receipt validation still enforces actual time bounds.
	value := contractVector(t, "postcommit-effect-failure")
	value["min_result_retention_seconds"] = json.Number("92233720368547758081234567890")
	effect := value["effects"].([]any)[0].(map[string]any)
	effect["kind"], effect["policy_id"] = "erasure", "privacy-policy-v3"
	effect["error"].(map[string]any)["command_id"] = "c1"
	var receipt Receipt
	if err := json.Unmarshal(outcomeJSON(t, value), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.MinResultRetentionSeconds.String() != "92233720368547758081234567890" || receipt.Effects[0].PolicyID == nil || *receipt.Effects[0].PolicyID != "privacy-policy-v3" || receipt.Effects[0].Error.CommandID == nil || *receipt.Effects[0].Error.CommandID != "c1" {
		t.Fatal("typed receipt narrowed an integer or lost optional effect/error data")
	}
	decoded, err := decodeJSON(outcomeJSON(t, receipt))
	if err != nil || !reflect.DeepEqual(decoded, value) {
		t.Fatalf("typed receipt changed complete wire data: %v", err)
	}
}
