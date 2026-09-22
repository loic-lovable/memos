package client

import (
	"fmt"
	"testing"
)

func createOutcomeIntent(t *testing.T, withSource bool) (*Client, Intent) {
	t.Helper()
	c := operationClient(t)
	humanWindowTransport(t, c)
	intent, err := c.PrepareCreate(t.Context(), Human{Authority: "hr-authority", SourceReference: "employee-42", DisplayName: "Maya"})
	if err != nil {
		t.Fatal(err)
	}
	if !withSource {
		// The convenience API requires a source reference, but recovery also
		// supports trusted raw intents whose wire command explicitly uses null.
		saved := humanJSON(t, intent.Bytes())
		request := saved["request"].(map[string]any)
		request["commands"].([]any)[0].(map[string]any)["source_reference"] = nil
		intent, err = c.RestoreIntent(humanMarshal(t, saved))
		if err != nil {
			t.Fatal(err)
		}
	}
	return c, intent
}

func TestCreateCommittedOutcomeRequiresRequestedSourceReference(t *testing.T) {
	for _, transport := range []string{"submit", "recover"} {
		t.Run(transport, func(t *testing.T) {
			for _, vector := range []string{"receipt-valid", "committed-pending-effect", "postcommit-effect-failure"} {
				t.Run(vector, func(t *testing.T) {
					for _, tc := range []struct {
						name       string
						withSource bool
						count      int
						valid      bool
					}{
						{"requested source returned", true, 1, true},
						{"requested source omitted", true, 0, false},
						{"multiple sources returned", true, 2, false},
						{"null source omitted", false, 0, true},
						{"null source unexpectedly returned", false, 1, false},
					} {
						t.Run(tc.name, func(t *testing.T) {
							c, intent := createOutcomeIntent(t, tc.withSource)
							receipt := outcomeReceipt(t, c, intent, vector)
							// Creation starts disabled and does not have a disable effect.
							// Preserve any pending/failed projection from the vector.
							effects := receipt["effects"].([]any)
							receipt["effects"] = effects[:len(effects)-1]
							commit := receipt["commit"].(map[string]any)
							for i := 0; i < tc.count; i++ {
								commit["resources"] = append(commit["resources"].([]any), map[string]any{
									"command_id": "c1", "resource": map[string]any{"type": "source_reference", "id": fmt.Sprintf("source-%d", i)}, "revision": "source-r1",
								})
							}
							if err := c.local["receipt-v0.2.schema.json"].Validate(receipt); err != nil {
								t.Fatalf("invalid receipt fixture: %v", err)
							}
							if err := c.validateReceipt02(receipt); err != nil {
								t.Fatalf("invalid receipt scope or timing: %v", err)
							}
							outcome, err := committedOutcomeResponse(t, c, intent, receipt, transport == "recover")
							if !tc.valid {
								requireUnknownOutcome(t, outcome, err)
								return
							}
							if err != nil || outcome.Receipt == nil || string(outcome.State()) != receipt["state"] || outcome.Receipt.Commit.State != CommitCommitted || len(outcome.Receipt.Commit.Resources) != 1+tc.count {
								t.Fatalf("valid create outcome changed: %+v, %v", outcome, err)
							}
						})
					}
				})
			}
		})
	}
}
