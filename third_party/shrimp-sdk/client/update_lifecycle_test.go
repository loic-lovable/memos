package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/client/internal/schemas"
	"github.com/lovablelabs/shrimp-protocol/sdk/go/profiles/humanattributes"
)

func prepareCompoundUpdate(t *testing.T, c *Client, p *humanattributes.Profile, typed bool, lifecycle string) (Intent, error) {
	t.Helper()
	if typed {
		given := "Maya"
		return c.PrepareHumanUpdateLifecycle(t.Context(), p, humanSubjectVersion(), humanattributes.Changes{
			Set: humanattributes.Values{Name: &humanattributes.Name{GivenName: &given}}, Clear: []humanattributes.Field{humanattributes.Department},
		}, lifecycle)
	}
	return c.PrepareUpdateLifecycle(t.Context(), humanSubjectVersion(), ScalarChanges{
		Set: map[string]string{"department": "Engineering"}, Clear: []string{"email"},
	}, lifecycle)
}

func TestUpdateLifecyclePreparationUsesOneOriginalRevision(t *testing.T) {
	for _, typed := range []bool{false, true} {
		name := "scalar"
		if typed {
			name = "human-profile"
		}
		t.Run(name, func(t *testing.T) {
			c, p := humanAttributeClient(t)
			calls := humanWindowTransport(t, c)
			for _, lifecycle := range []string{"active", "disabled", "retired"} {
				t.Run(lifecycle, func(t *testing.T) {
					intent, err := prepareCompoundUpdate(t, c, p, typed, lifecycle)
					if err != nil {
						t.Fatal(err)
					}
					request := humanJSON(t, []byte(intent.body))
					commands := request["commands"].([]any)
					if len(commands) != 1 {
						t.Fatal("compound update split into multiple commands")
					}
					command := commands[0].(map[string]any)
					if command["action"] != "update_subject" || command["lifecycle"] != lifecycle || command["expected_revision"] != "r1" || !reflect.DeepEqual(command["resource"], map[string]any{"type": "subject", "id": "s1"}) {
						t.Fatalf("compound update changed original target, revision or lifecycle: %v", command)
					}
					if typed {
						if command["attribute_profile"] != humanattributes.ID || !reflect.DeepEqual(command["set"], map[string]any{"name": map[string]any{"givenName": "Maya"}}) || !reflect.DeepEqual(command["clear"], []any{"department"}) || !reflect.DeepEqual(request["required_profiles"], []any{"baseline", "human", humanattributes.ID}) {
							t.Fatal("compound update lost typed values or profile selection")
						}
					} else if _, selected := command["attribute_profile"]; selected || !reflect.DeepEqual(command["set"], map[string]any{"department": "Engineering"}) || !reflect.DeepEqual(command["clear"], []any{"email"}) || !reflect.DeepEqual(request["required_profiles"], []any{}) {
						t.Fatal("scalar compound update changed attributes or selected a richer profile")
					}
				})
			}
			var plain Intent
			var err error
			if typed {
				plain, err = c.PrepareHumanUpdate(t.Context(), p, humanSubjectVersion(), humanattributes.Changes{Clear: []humanattributes.Field{humanattributes.Department}})
			} else {
				plain, err = c.PrepareUpdate(t.Context(), humanSubjectVersion(), ScalarChanges{Clear: []string{"department"}})
			}
			if err != nil {
				t.Fatal(err)
			}
			request := humanJSON(t, []byte(plain.body))
			if _, exists := request["commands"].([]any)[0].(map[string]any)["lifecycle"]; exists {
				t.Fatal("existing update helper acquired an implicit lifecycle action")
			}
			if *calls != 4 || c.profile.RequiredProfiles == nil || len(c.profile.RequiredProfiles) != 0 {
				t.Fatal("helpers fetched extra state or changed connection profile requirements")
			}
		})
	}
}

func TestUpdateLifecycleInvalidIntentFailsBeforeNetwork(t *testing.T) {
	for _, typed := range []bool{false, true} {
		name := "scalar"
		if typed {
			name = "human-profile"
		}
		t.Run(name, func(t *testing.T) {
			c, p := humanAttributeClient(t)
			c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("invalid compound intent contacted the peer")
				return nil, nil
			})
			for _, lifecycle := range []string{"", "activate", "ACTIVE", "disabled "} {
				if _, err := prepareCompoundUpdate(t, c, p, typed, lifecycle); err == nil {
					t.Fatalf("accepted invalid lifecycle %q", lifecycle)
				}
			}
			var empty, overlap error
			if typed {
				_, empty = c.PrepareHumanUpdateLifecycle(t.Context(), p, humanSubjectVersion(), humanattributes.Changes{}, "disabled")
				value := "Maya"
				_, overlap = c.PrepareHumanUpdateLifecycle(t.Context(), p, humanSubjectVersion(), humanattributes.Changes{
					Set: humanattributes.Values{DisplayName: &value}, Clear: []humanattributes.Field{humanattributes.DisplayName},
				}, "retired")
			} else {
				_, empty = c.PrepareUpdateLifecycle(t.Context(), humanSubjectVersion(), ScalarChanges{}, "disabled")
				_, overlap = c.PrepareUpdateLifecycle(t.Context(), humanSubjectVersion(), ScalarChanges{
					Set: map[string]string{"displayName": "Maya"}, Clear: []string{"displayName"},
				}, "retired")
			}
			if empty == nil || overlap == nil {
				t.Fatal("lifecycle allowed an empty or invalid attribute change")
			}
		})
	}
}

func TestUpdateLifecycleRejectsOlderCatalogWithoutFallback(t *testing.T) {
	// Reconstruct the previous closed update_subject shape. The peer still
	// supports ordinary scalar and selected-profile updates, but not lifecycle.
	documents := map[string]map[string]any{}
	files, err := schemas.Files.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		raw, err := schemas.Files.ReadFile(file.Name())
		if err != nil {
			t.Fatal(err)
		}
		documents[file.Name()] = humanJSON(t, raw)
	}
	update := documents["mutation-v0.2.schema.json"]["$defs"].(map[string]any)["update_subject"].(map[string]any)
	delete(update["properties"].(map[string]any), "lifecycle")
	previous, err := compileSchemas(documents)
	if err != nil {
		t.Fatal(err)
	}
	for _, typed := range []bool{false, true} {
		name := "scalar"
		if typed {
			name = "human-profile"
		}
		t.Run(name, func(t *testing.T) {
			c, p := humanAttributeClient(t)
			c.catalog = previous
			calls := humanWindowTransport(t, c)
			intent, err := prepareCompoundUpdate(t, c, p, typed, "disabled")
			if err == nil || len(intent.Bytes()) != 0 || *calls != 1 {
				t.Fatal("unsupported compound update was accepted, rewritten or split")
			}
			if typed {
				_, err = c.PrepareHumanUpdate(t.Context(), p, humanSubjectVersion(), humanattributes.Changes{Clear: []humanattributes.Field{humanattributes.Department}})
			} else {
				_, err = c.PrepareUpdate(t.Context(), humanSubjectVersion(), ScalarChanges{Clear: []string{"department"}})
			}
			if err != nil || *calls != 2 {
				t.Fatalf("ordinary update positive control failed: %v", err)
			}
		})
	}
}

func TestUpdateLifecycleSuccessRequiresMatchingAdmissionEvidence(t *testing.T) {
	for _, lifecycle := range []string{"active", "disabled", "retired"} {
		t.Run(lifecycle, func(t *testing.T) {
			c, p := humanAttributeClient(t)
			humanWindowTransport(t, c)
			intent, err := prepareCompoundUpdate(t, c, p, lifecycle == "retired", lifecycle)
			if err != nil {
				t.Fatal(err)
			}
			cases := []string{"correct", "wrong subject", "wrong effect", "pending effect", "missing effect"}
			if lifecycle == "active" {
				cases = []string{"correct", "unexpected effect"}
			}
			for _, fault := range cases {
				t.Run(fault, func(t *testing.T) {
					receipt := outcomeReceipt(t, c, intent, "receipt-valid")
					switch fault {
					case "correct":
						if lifecycle == "active" {
							receipt["effects"] = []any{}
						}
					case "wrong subject":
						receipt["effects"].([]any)[0].(map[string]any)["resource"].(map[string]any)["id"] = "another-subject"
					case "wrong effect":
						receipt["effects"].([]any)[0].(map[string]any)["kind"] = "binding_fence"
					case "pending effect":
						receipt["effects"].([]any)[0].(map[string]any)["state"] = "pending"
					case "missing effect":
						receipt["effects"] = []any{}
					}
					posts := 0
					c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
						posts++
						raw, err := io.ReadAll(r.Body)
						if err != nil || r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/mutations") || !bytes.Equal(raw, []byte(intent.body)) {
							t.Fatal("submission changed the compound intent")
						}
						return outcomeResponse(receipt, r.URL.Path, intent), nil
					})
					outcome, err := c.SubmitTyped(t.Context(), intent, func(context.Context, string, []byte) error { return nil })
					if fault == "correct" {
						if err != nil || outcome.State() != OutcomeSucceeded {
							t.Fatalf("complete compound receipt rejected: %v", err)
						}
					} else {
						requireUnknownOutcome(t, outcome, err)
					}
					if posts != 1 {
						t.Fatal("compound submission caused a second operation")
					}
				})
			}
		})
	}
}

func TestUpdateLifecycleRecoveryKeepsOriginalIntentAfterWithdrawal(t *testing.T) {
	c, p := humanAttributeClient(t)
	humanWindowTransport(t, c)
	name := "Maya"
	subject := humanSubjectVersion()
	changes := humanattributes.Changes{Set: humanattributes.Values{DisplayName: &name}}
	intent, err := c.PrepareHumanUpdateLifecycle(t.Context(), p, subject, changes, "disabled")
	if err != nil {
		t.Fatal(err)
	}
	receipt := outcomeReceipt(t, c, intent, "receipt-valid")
	var saved []byte
	posts, reads := 0, 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/operations/"+intent.ReplayWindow()+"/"+intent.ID()) {
			reads++
			return operationResponse(receipt, ""), nil
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/mutations") {
			t.Fatalf("recovery reread current state or requested a new window: %s %s", r.Method, r.URL.Path)
		}
		posts++
		raw, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(raw, []byte(intent.body)) || !bytes.Equal(saved, intent.Bytes()) {
			t.Fatal("compound retry changed intent or submitted before persistence")
		}
		if posts == 1 {
			return nil, errors.New("reply lost after commit")
		}
		return outcomeResponse(receipt, r.URL.Path, intent), nil
	})
	c.http.Transport = transport
	saver := func(_ context.Context, id string, raw []byte) error {
		if id != intent.ID() || (len(saved) != 0 && !bytes.Equal(saved, raw)) {
			return errors.New("changed persisted compound intent")
		}
		saved = bytes.Clone(raw)
		return nil
	}
	outcome, err := c.SubmitTyped(t.Context(), intent, saver)
	requireUnknownOutcome(t, outcome, err)
	if posts != 1 {
		t.Fatal("expected one uncertain submission")
	}
	// Caller input and current target state may move on. Neither is the original
	// operation's outcome; recovery must not reread or rebase onto these values.
	name, subject.Revision = "Later name", "r-later"
	restarted := operationClient(t)
	restarted.profile, restarted.selected, restarted.catalog, restarted.writeScope = c.profile, nil, nil, false
	restarted.http.Transport = transport
	restored, err := restarted.RestoreIntent(saved)
	if err != nil || !bytes.Equal(restored.Bytes(), intent.Bytes()) {
		t.Fatalf("restoring compound intent changed its representation: %v", err)
	}
	outcome, err = restarted.RecoverTyped(t.Context(), restored)
	if err != nil || outcome.State() != OutcomeSucceeded || outcome.Receipt.Commit.Resources[0].Revision != "r2" || posts != 1 || reads != 1 {
		t.Fatalf("read-only compound recovery: %q %v", outcome.State(), err)
	}
	restarted.writeScope = true
	if outcome, err = restarted.SubmitTyped(t.Context(), restored, saver); err != nil || outcome.State() != OutcomeSucceeded || posts != 2 {
		t.Fatalf("exact retry after support withdrawal: %q %v", outcome.State(), err)
	}
	for _, fault := range []string{"missing admission fence", "unchanged original revision", "shortened retention"} {
		t.Run(fault, func(t *testing.T) {
			receipt = outcomeReceipt(t, c, intent, "receipt-valid")
			switch fault {
			case "missing admission fence":
				receipt["effects"] = []any{}
			case "unchanged original revision":
				receipt["commit"].(map[string]any)["resources"].([]any)[0].(map[string]any)["revision"] = "r1"
			case "shortened retention":
				receipt["results_retained_until"] = "2026-09-20T00:05:00Z"
			}
			outcome, err := restarted.RecoverTyped(t.Context(), restored)
			requireUnknownOutcome(t, outcome, err)
			if posts != 2 {
				t.Fatal("recovery of invalid evidence repeated a business operation")
			}
		})
	}
}
