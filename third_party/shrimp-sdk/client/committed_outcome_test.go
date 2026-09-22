package client

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func committedOutcomeIntent(t *testing.T, command string) (*Client, Intent) {
	t.Helper()
	c, p := humanAttributeClient(t)
	humanWindowTransport(t, c)
	var intent Intent
	var err error
	switch command {
	case "disable":
		intent, err = c.PrepareDisable(t.Context(), humanSubjectVersion())
	case "scalar-disabled":
		intent, err = prepareCompoundUpdate(t, c, p, false, "disabled")
	case "human-retired":
		intent, err = prepareCompoundUpdate(t, c, p, true, "retired")
	default:
		t.Fatalf("unknown test command %q", command)
	}
	if err != nil {
		t.Fatal(err)
	}
	return c, intent
}

func committedOutcomeResponse(t *testing.T, c *Client, intent Intent, receipt map[string]any, recover bool) (Outcome, error) {
	t.Helper()
	if recover {
		// Current discovery and write rights cannot replace validation against
		// the original intent during historical read-only recovery.
		c.selected, c.catalog, c.writeScope = nil, nil, false
	}
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if recover {
			if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/operations/"+intent.ReplayWindow()+"/"+intent.ID()) {
				t.Fatalf("recovery made unexpected request %s %s", r.Method, r.URL.Path)
			}
			return operationResponse(receipt, ""), nil
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/mutations") {
			t.Fatalf("submission made unexpected request %s %s", r.Method, r.URL.Path)
		}
		return outcomeResponse(receipt, r.URL.Path, intent), nil
	})
	if recover {
		return c.RecoverTyped(t.Context(), intent)
	}
	return c.SubmitTyped(t.Context(), intent, func(context.Context, string, []byte) error { return nil })
}

func TestTypedCommittedOutcomesBindOriginalCommandAndAdmission(t *testing.T) {
	for _, transport := range []string{"submit", "recover"} {
		t.Run(transport, func(t *testing.T) {
			for _, vector := range []string{"committed-pending-effect", "postcommit-effect-failure"} {
				t.Run(vector, func(t *testing.T) {
					for _, tc := range []struct {
						name, command string
						damage        func(map[string]any, map[string]any, map[string]any)
					}{
						{"valid disable", "disable", nil},
						{"valid compound disable", "scalar-disabled", nil},
						{"valid compound retire", "human-retired", nil},
						{"wrong subject", "disable", func(_ map[string]any, resource, _ map[string]any) {
							resource["resource"].(map[string]any)["id"] = "another-subject"
						}},
						{"wrong command", "scalar-disabled", func(_ map[string]any, resource, _ map[string]any) {
							resource["command_id"] = "other-command"
						}},
						{"unchanged revision", "human-retired", func(_ map[string]any, resource, _ map[string]any) {
							resource["revision"] = "r1"
						}},
						{"missing admission", "scalar-disabled", func(receipt, _, _ map[string]any) {
							receipt["effects"] = receipt["effects"].([]any)[:1]
						}},
						{"unfinished admission", "human-retired", func(receipt, _, admission map[string]any) {
							admission["observed_frontier"] = nil
							admission["state"] = receipt["state"]
							if receipt["state"] == "failed" {
								admission["error"] = receipt["error"]
							}
						}},
						{"admission for wrong subject", "disable", func(_, _ map[string]any, admission map[string]any) {
							admission["resource"].(map[string]any)["id"] = "another-subject"
						}},
					} {
						t.Run(tc.name, func(t *testing.T) {
							c, intent := committedOutcomeIntent(t, tc.command)
							receipt := outcomeReceipt(t, c, intent, vector)
							if tc.damage != nil {
								resource := receipt["commit"].(map[string]any)["resources"].([]any)[0].(map[string]any)
								admission := receipt["effects"].([]any)[1].(map[string]any)
								tc.damage(receipt, resource, admission)
							}
							// All cases pass independent receipt validation. Rejection must
							// come from their relationship to the original command.
							if err := c.local["receipt-v0.2.schema.json"].Validate(receipt); err != nil {
								t.Fatalf("invalid test fixture: %v", err)
							}
							if err := c.validateReceipt02(receipt); err != nil {
								t.Fatalf("invalid test fixture timing or scope: %v", err)
							}
							outcome, err := committedOutcomeResponse(t, c, intent, receipt, transport == "recover")
							if tc.damage != nil {
								requireUnknownOutcome(t, outcome, err)
								return
							}
							if err != nil || outcome.Receipt == nil || string(outcome.State()) != receipt["state"] || outcome.Receipt.Commit.State != CommitCommitted {
								t.Fatalf("valid committed projection outcome changed: %+v, %v", outcome, err)
							}
							if len(outcome.Receipt.Effects) != 2 || outcome.Receipt.Effects[0].Kind != "assignment_projection" || outcome.Receipt.Effects[1].State != EffectComplete {
								t.Fatal("projection or completed admission evidence was lost")
							}
						})
					}
				})
			}
		})
	}
}
