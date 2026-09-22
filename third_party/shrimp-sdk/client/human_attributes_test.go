package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/profiles/humanattributes"
)

func humanAttributeClient(t *testing.T) (*Client, *humanattributes.Profile) {
	t.Helper()
	c := operationClient(t)
	if err := json.Unmarshal([]byte(`{"profiles":["baseline","human","human-attributes-v1"],"human_attributes":{"max_emails":2,"tzdb_version":"2026a"}}`), c.selected); err != nil {
		t.Fatal(err)
	}
	return c, humanValidator(t, 2, "2026a")
}

func humanValidator(t *testing.T, maximum int, version string) *humanattributes.Profile {
	t.Helper()
	// This is a small deterministic catalog fixture, not a complete tzdb release.
	p, err := humanattributes.New(humanattributes.Config{MaxEmails: maximum, TZDBVersion: version, Timezones: []string{"Etc/UTC", "US/Eastern"}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func humanJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	value, err := decodeJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatal("expected object")
	}
	return object
}

func humanMarshal(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func humanWindowTransport(t *testing.T, c *Client) *int {
	t.Helper()
	calls := new(int)
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		*calls++
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/replay-window") {
			t.Fatalf("preparation called unexpected route %s %s", r.Method, r.URL.Path)
		}
		return operationResponse(operationWindow(c), ""), nil
	})
	return calls
}

func humanSubjectVersion() SubjectVersion {
	return SubjectVersion{ID: "s1", Revision: "r1", Authority: "hr-authority"}
}

func TestHumanAttributeLimitsRejectMissingContractBeforeWindow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Client)
	}{
		{"no discovery", func(c *Client) { c.selected = nil }},
		{"wrong wire version", func(c *Client) { c.profile.Version = "0.1" }},
		{"missing baseline", func(c *Client) { c.selected.Profiles = []string{"human", humanattributes.ID} }},
		{"missing human", func(c *Client) { c.selected.Profiles = []string{"baseline", humanattributes.ID} }},
		{"missing attribute profile", func(c *Client) { c.selected.Profiles = []string{"baseline", "human"} }},
		{"missing metadata", func(c *Client) { c.selected.HumanAttributes = nil }},
		{"zero email bound", func(c *Client) { c.selected.HumanAttributes.MaxEmails = 0 }},
		{"excess email bound", func(c *Client) { c.selected.HumanAttributes.MaxEmails = 17 }},
		{"missing pinned version", func(c *Client) { c.selected.HumanAttributes.TZDBVersion = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, p := humanAttributeClient(t)
			c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("unsupported profile contacted the peer")
				return nil, nil
			})
			tc.change(c)
			if _, _, err := c.HumanAttributeLimits(); err == nil {
				t.Fatal("accepted incomplete profile contract")
			}
			if _, err := c.PrepareHumanCreate(t.Context(), p, HumanWithAttributes{Authority: "hr-authority", SourceReference: "employee-42"}); err == nil {
				t.Fatal("creation accepted incomplete contract")
			}
			if _, err := c.PrepareHumanUpdate(t.Context(), p, humanSubjectVersion(), humanattributes.Changes{Clear: []humanattributes.Field{humanattributes.NameField}}); err == nil {
				t.Fatal("update accepted incomplete contract")
			}
			if _, err := c.PrepareHumanMigration(t.Context(), humanSubjectVersion(), nil); err == nil {
				t.Fatal("migration accepted incomplete contract")
			}
		})
	}
}

func TestPrepareHumanAttributesPreservesTypedShapeAndExplicitRequirements(t *testing.T) {
	c, p := humanAttributeClient(t)
	calls := humanWindowTransport(t, c)
	maximum, version, err := c.HumanAttributeLimits()
	if err != nil || maximum != 2 || version != "2026a" {
		t.Fatalf("advertised configuration: %d %q %v", maximum, version, err)
	}
	display, given, locale, zone := "", "Maya", "EN-us", "US/Eastern"
	emails := []humanattributes.EmailInput{{EntryID: "work-1", Value: "Maya+work@example.test", Primary: false}}
	intent, err := c.PrepareHumanCreate(t.Context(), p, HumanWithAttributes{
		Authority: "hr-authority", SourceReference: "employee-42",
		Attributes: humanattributes.Values{DisplayName: &display, Name: &humanattributes.Name{GivenName: &given}, Emails: &emails, Locale: &locale, Timezone: &zone},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := humanJSON(t, []byte(intent.body))
	command := request["commands"].([]any)[0].(map[string]any)
	if command["action"] != "create_subject" || command["profile"] != "human" || command["attribute_profile"] != humanattributes.ID || command["if_absent"] != true || command["source_reference"] != "employee-42" || command["expires_at"] != nil {
		t.Fatalf("incorrect selected-profile creation: %v", command)
	}
	attributes := command["attributes"].(map[string]any)
	want := humanJSON(t, []byte(`{"displayName":"","name":{"givenName":"Maya"},"emails":[{"entry_id":"work-1","value":"Maya+work@example.test","primary":false,"expected_generation":null}],"locale":"EN-us","timezone":"US/Eastern"}`))
	if !reflect.DeepEqual(attributes, want) {
		t.Fatalf("typed values changed: got %v want %v", attributes, want)
	}
	if !reflect.DeepEqual(request["required_profiles"], []any{"baseline", "human", humanattributes.ID}) || c.profile.RequiredProfiles == nil || len(c.profile.RequiredProfiles) != 0 {
		t.Fatal("implied profile requirements changed the explicit empty connection preference")
	}
	// Changing caller-owned input after preparation cannot rewrite the saved intent.
	original := intent.Bytes()
	emails[0].Value, given = "changed@example.test", "Changed"
	if !bytes.Equal(original, intent.Bytes()) {
		t.Fatal("typed inputs alias the prepared operation")
	}

	c.profile.RequiredProfiles = []string{"human", "directory-sync"}
	c.selected.Profiles = append(c.selected.Profiles, "directory-sync")
	configured := append([]string(nil), c.profile.RequiredProfiles...)
	emptyEmails := []humanattributes.EmailInput{}
	update, err := c.PrepareHumanUpdate(t.Context(), p, humanSubjectVersion(), humanattributes.Changes{
		Set: humanattributes.Values{Emails: &emptyEmails}, Clear: []humanattributes.Field{humanattributes.NameField},
	})
	if err != nil {
		t.Fatal(err)
	}
	request = humanJSON(t, []byte(update.body))
	command = request["commands"].([]any)[0].(map[string]any)
	wantCommand := humanJSON(t, []byte(`{"command_id":"c1","action":"update_subject","resource":{"type":"subject","id":"s1"},"expected_revision":"r1","attribute_profile":"human-attributes-v1","set":{"emails":[]},"clear":["name"]}`))
	if !reflect.DeepEqual(command, wantCommand) || !reflect.DeepEqual(request["required_profiles"], []any{"human", "directory-sync", "baseline", humanattributes.ID}) {
		t.Fatalf("update lost selection, clear or explicit requirements: %v", request)
	}
	if !reflect.DeepEqual(c.profile.RequiredProfiles, configured) || *calls != 2 {
		t.Fatal("preparation mutated connection requirements or made extra requests")
	}
	generation, kind := "current-generation", "work"
	existing := []humanattributes.EmailInput{{EntryID: "work-1", Value: "Maya+work@example.test", Type: &kind, Primary: true, ExpectedGeneration: &generation}}
	changes := humanattributes.Changes{Set: humanattributes.Values{Emails: &existing}}
	update, err = c.PrepareHumanUpdate(t.Context(), p, humanSubjectVersion(), changes)
	if err != nil {
		t.Fatal(err)
	}
	request = humanJSON(t, []byte(update.body))
	command = request["commands"].([]any)[0].(map[string]any)
	storedEmail := command["set"].(map[string]any)["emails"].([]any)[0].(map[string]any)
	if storedEmail["expected_generation"] != generation || storedEmail["type"] != kind || storedEmail["primary"] != true || storedEmail["value"] != existing[0].Value || !reflect.DeepEqual(command["clear"], []any{}) {
		t.Fatal("existing-entry update lost its exact generation, value or optional metadata")
	}
	if changes.Clear != nil || *calls != 3 {
		t.Fatal("normalizing the wire clear list modified caller input or requested extra windows")
	}
}

func TestPrepareHumanAttributesRejectsInvalidIntentBeforeWindow(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T, *Client, *humanattributes.Profile) error
	}{
		{"nil validator", func(t *testing.T, c *Client, _ *humanattributes.Profile) error {
			_, err := c.PrepareHumanCreate(t.Context(), nil, HumanWithAttributes{Authority: "hr-authority", SourceReference: "employee-42"})
			return err
		}},
		{"different email limit", func(t *testing.T, c *Client, _ *humanattributes.Profile) error {
			_, err := c.PrepareHumanCreate(t.Context(), humanValidator(t, 1, "2026a"), HumanWithAttributes{Authority: "hr-authority", SourceReference: "employee-42"})
			return err
		}},
		{"different pinned catalog", func(t *testing.T, c *Client, _ *humanattributes.Profile) error {
			_, err := c.PrepareHumanUpdate(t.Context(), humanValidator(t, 2, "2025b"), humanSubjectVersion(), humanattributes.Changes{Clear: []humanattributes.Field{humanattributes.NameField}})
			return err
		}},
		{"create with an old generation", func(t *testing.T, c *Client, p *humanattributes.Profile) error {
			generation := "already-used"
			emails := []humanattributes.EmailInput{{EntryID: "work", Value: "maya@example.test", ExpectedGeneration: &generation}}
			_, err := c.PrepareHumanCreate(t.Context(), p, HumanWithAttributes{Authority: "hr-authority", SourceReference: "employee-42", Attributes: humanattributes.Values{Emails: &emails}})
			return err
		}},
		{"invalid unicode before marshaling", func(t *testing.T, c *Client, p *humanattributes.Profile) error {
			value := string([]byte{0xff})
			_, err := c.PrepareHumanUpdate(t.Context(), p, humanSubjectVersion(), humanattributes.Changes{Set: humanattributes.Values{DisplayName: &value}})
			return err
		}},
		{"set clear overlap", func(t *testing.T, c *Client, p *humanattributes.Profile) error {
			value := "Maya"
			_, err := c.PrepareHumanUpdate(t.Context(), p, humanSubjectVersion(), humanattributes.Changes{Set: humanattributes.Values{DisplayName: &value}, Clear: []humanattributes.Field{humanattributes.DisplayName}})
			return err
		}},
		{"unknown timezone", func(t *testing.T, c *Client, p *humanattributes.Profile) error {
			zone := "Europe/Paris"
			_, err := c.PrepareHumanUpdate(t.Context(), p, humanSubjectVersion(), humanattributes.Changes{Set: humanattributes.Values{Timezone: &zone}})
			return err
		}},
		{"empty update", func(t *testing.T, c *Client, p *humanattributes.Profile) error {
			_, err := c.PrepareHumanUpdate(t.Context(), p, humanSubjectVersion(), humanattributes.Changes{})
			return err
		}},
		{"migration entry token", func(t *testing.T, c *Client, _ *humanattributes.Profile) error {
			entry := "not an entry token"
			_, err := c.PrepareHumanMigration(t.Context(), humanSubjectVersion(), &entry)
			return err
		}},
		{"migration without revision", func(t *testing.T, c *Client, _ *humanattributes.Profile) error {
			_, err := c.PrepareHumanMigration(t.Context(), SubjectVersion{ID: "s1", Authority: "hr-authority"}, nil)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, p := humanAttributeClient(t)
			c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("invalid intent requested a window")
				return nil, nil
			})
			if err := tc.run(t, c, p); err == nil {
				t.Fatal("accepted invalid new work")
			}
		})
	}
}

func TestHumanMigrationApprovalBindsPersistedIntentAcrossRestart(t *testing.T) {
	for _, entry := range []*string{nil, new("migration-mail")} {
		name := "absent-or-cleared-email"
		if entry != nil {
			name = "present-email"
		}
		t.Run(name, func(t *testing.T) {
			c, _ := humanAttributeClient(t)
			calls := humanWindowTransport(t, c)
			pending, err := c.PrepareHumanMigration(t.Context(), humanSubjectVersion(), entry)
			if err != nil {
				t.Fatal(err)
			}
			persisted, approvedRequest := pending.Bytes(), pending.Request()
			draft := humanJSON(t, persisted)
			request := draft["migration_request"].(map[string]any)
			command := request["commands"].([]any)[0].(map[string]any)
			if _, exists := command["authorization"]; exists {
				t.Fatal("pending migration carries a submit-ready authorization")
			}
			if command["expected_revision"] != "r1" || command["action"] != "migrate_human_attributes" || !reflect.DeepEqual(command["resource"], map[string]any{"type": "subject", "id": "s1"}) {
				t.Fatal("pending migration changed the requested subject")
			}
			if (entry == nil && command["email_entry_id"] != nil) || (entry != nil && command["email_entry_id"] != *entry) {
				t.Fatal("migration changed null/value entry selection")
			}
			canonical := humanMarshal(t, request)
			sum := sha256.Sum256(canonical)
			if !bytes.Equal(canonical, approvedRequest) || pending.Fingerprint() != hex.EncodeToString(sum[:]) {
				t.Fatal("approval fingerprint is not the exact request without authorization")
			}
			leaked := pending.Bytes()
			leaked[0] = '!'
			leaked = pending.Request()
			leaked[0] = '!'
			if !bytes.Equal(pending.Bytes(), persisted) || !bytes.Equal(pending.Request(), approvedRequest) {
				t.Fatal("pending proposal exposes mutable byte storage")
			}
			if _, err := c.RestoreIntent(persisted); err == nil {
				t.Fatal("pending proposal restored as submit-ready intent")
			}
			if _, err := c.Submit(t.Context(), Intent{raw: string(persisted)}, func(context.Context, string, []byte) error { return nil }); err == nil || *calls != 1 {
				t.Fatal("pending proposal was submitted")
			}

			restarted := operationClient(t)
			restarted.profile, restarted.selected, restarted.catalog = c.profile, nil, nil
			restarted.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Fatal("restoring or authorizing migration contacted peer")
				return nil, nil
			})
			restored, err := restarted.RestoreHumanMigration(persisted)
			if err != nil || !bytes.Equal(restored.Bytes(), persisted) || restored.Fingerprint() != pending.Fingerprint() {
				t.Fatalf("restore changed approved proposal: %v", err)
			}
			final, err := restarted.AuthorizeHumanMigration(restored, "admin-grant-for-exact-intent")
			if err != nil {
				t.Fatal(err)
			}
			finalEnvelope := humanJSON(t, final.Bytes())
			finalRequest := finalEnvelope["request"].(map[string]any)
			finalCommand := finalRequest["commands"].([]any)[0].(map[string]any)
			if finalCommand["authorization"] != "admin-grant-for-exact-intent" {
				t.Fatal("authorization handle missing")
			}
			fullSum := sha256.Sum256(humanMarshal(t, finalRequest))
			if hex.EncodeToString(fullSum[:]) == pending.Fingerprint() {
				t.Fatal("final operation fingerprint omitted authorization")
			}
			delete(finalCommand, "authorization")
			if !bytes.Equal(humanMarshal(t, finalRequest), approvedRequest) {
				t.Fatal("authorizing changed approved operation/window/deadline/revision/entry ID")
			}
			delete(finalEnvelope, "request")
			delete(draft, "migration_request")
			if !reflect.DeepEqual(finalEnvelope, draft) || !bytes.Equal(pending.Bytes(), persisted) {
				t.Fatal("authorizing changed enrollment/window evidence or original proposal")
			}
		})
	}
}

func TestHumanMigrationRestorationRejectsMalformedAndForeignDrafts(t *testing.T) {
	c, _ := humanAttributeClient(t)
	humanWindowTransport(t, c)
	pending, err := c.PrepareHumanMigration(t.Context(), humanSubjectVersion(), nil)
	if err != nil {
		t.Fatal(err)
	}
	c.selected, c.catalog = nil, nil
	c.http.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid draft contacted peer")
		return nil, nil
	})
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"wrong enrollment", func(d map[string]any) { d["tenant"] = "another-tenant" }},
		{"extra wrapper member", func(d map[string]any) { d["approved"] = true }},
		{"submit envelope", func(d map[string]any) { d["request"] = d["migration_request"] }},
		{"missing original window", func(d map[string]any) { delete(d, "replay_window_evidence") }},
		{"already authorized", func(d map[string]any) { migrationCommand(d)["authorization"] = "handle" }},
		{"wrong action", func(d map[string]any) { migrationCommand(d)["action"] = "activate" }},
		{"missing nullable entry ID", func(d map[string]any) { delete(migrationCommand(d), "email_entry_id") }},
		{"unapproved attribute mutation", func(d map[string]any) { migrationCommand(d)["set"] = map[string]any{"email": "another@example.test"} }},
		{"window substitution", func(d map[string]any) {
			d["migration_request"].(map[string]any)["operation"].(map[string]any)["replay_window"] = "other-window"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			draft := humanJSON(t, pending.Bytes())
			tc.change(draft)
			if _, err := c.RestoreHumanMigration(humanMarshal(t, draft)); err == nil {
				t.Fatal("accepted malformed or foreign draft")
			}
		})
	}
	for _, raw := range [][]byte{nil, []byte(`[]`), append(pending.Bytes(), []byte(` {}`)...)} {
		if _, err := c.RestoreHumanMigration(raw); err == nil {
			t.Fatal("accepted invalid proposal JSON")
		}
	}
	for _, handle := range []string{"", string([]byte{0xff}), strings.Repeat("x", 1025)} {
		if _, err := c.AuthorizeHumanMigration(pending, handle); err == nil {
			t.Fatal("accepted invalid authorization handle")
		}
	}
	if _, err := c.AuthorizeHumanMigration(PendingHumanMigration{}, "handle"); err == nil {
		t.Fatal("accepted zero pending migration")
	}
	// Saved drafts require integrity protection. A valid edit is new approval
	// content, not something structural restoration can authenticate or authorize.
	for _, field := range []string{"expected_revision", "email_entry_id"} {
		draft := humanJSON(t, pending.Bytes())
		migrationCommand(draft)[field] = "changed"
		changed, err := c.RestoreHumanMigration(humanMarshal(t, draft))
		if err != nil || changed.Fingerprint() == pending.Fingerprint() {
			t.Fatalf("changed %s did not change approval binding: %v", field, err)
		}
	}
}

func migrationCommand(draft map[string]any) map[string]any {
	return draft["migration_request"].(map[string]any)["commands"].([]any)[0].(map[string]any)
}

func TestHumanMigrationRecoversOriginalIntentAfterSupportWithdrawal(t *testing.T) {
	c, _ := humanAttributeClient(t)
	humanWindowTransport(t, c)
	entryID := "migrated-work"
	pending, err := c.PrepareHumanMigration(t.Context(), humanSubjectVersion(), &entryID)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := c.AuthorizeHumanMigration(pending, "exact-migration-grant")
	if err != nil {
		t.Fatal(err)
	}
	var saved []byte
	posts, reads := 0, 0
	request := humanJSON(t, []byte(intent.body))
	receipt := contractVector(t, "receipt-valid")
	receipt["scope"], receipt["operation"] = operationScope(c), request["operation"]
	receipt["accepted_at"], receipt["terminal_at"] = "2026-09-18T00:00:00Z", "2026-09-18T00:00:01Z"
	receipt["results_retained_until"], receipt["min_result_retention_seconds"] = "2026-09-20T00:10:00Z", 172800
	receipt["commit"] = map[string]any{"state": "committed", "causal_token": "migration-token", "resources": []any{map[string]any{"command_id": "c1", "resource": map[string]any{"type": "subject", "id": "s1"}, "revision": "r2"}}}
	receipt["effects"] = []any{}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/operations/") {
			reads++
			return operationResponse(receipt, ""), nil
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/mutations") {
			t.Fatalf("recovery tried new preparation: %s %s", r.Method, r.URL.Path)
		}
		posts++
		raw, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(raw, []byte(intent.body)) || !bytes.Equal(saved, intent.Bytes()) {
			t.Fatal("mutation was not the durably saved original request")
		}
		if posts == 1 {
			return nil, errors.New("reply lost after commit")
		}
		return operationResponse(receipt, strings.TrimSuffix(r.URL.Path, "/mutations")+"/operations/"+intent.ReplayWindow()+"/"+intent.ID()), nil
	})
	c.http.Transport = transport
	saver := func(_ context.Context, id string, raw []byte) error {
		if id != intent.ID() || (len(saved) != 0 && !bytes.Equal(saved, raw)) {
			return errors.New("saved operation identity already has different content")
		}
		saved = bytes.Clone(raw)
		return nil
	}
	if _, err := c.Submit(t.Context(), intent, saver); err == nil || posts != 1 {
		t.Fatal("expected uncertain first migration attempt")
	}
	restarted := operationClient(t)
	restarted.profile, restarted.selected, restarted.catalog = c.profile, nil, nil
	restarted.http.Transport = transport
	restored, err := restarted.RestoreIntent(saved)
	if err != nil || !bytes.Equal(restored.Bytes(), intent.Bytes()) {
		t.Fatalf("could not restore original migration: %v", err)
	}
	restarted.writeScope = false
	got, err := restarted.Recover(t.Context(), restored)
	if err != nil || got["state"] != "succeeded" || posts != 1 || reads != 1 {
		t.Fatalf("read-only migration recovery: %v %v", got, err)
	}
	if _, err := restarted.Submit(t.Context(), restored, saver); err == nil || posts != 1 {
		t.Fatal("read-only recovery became a new submission")
	}
	restarted.writeScope = true
	if _, err := restarted.Submit(t.Context(), restored, saver); err != nil || posts != 2 {
		t.Fatalf("exact migration retry after support withdrawal: %v", err)
	}
	// Changing the approval handle under the original operation must not replace
	// its persisted final intent. The target independently checks replay equality.
	changed := humanJSON(t, restored.Bytes())
	changed["request"].(map[string]any)["commands"].([]any)[0].(map[string]any)["authorization"] = "replacement-handle"
	replacement, err := restarted.RestoreIntent(humanMarshal(t, changed))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Submit(t.Context(), replacement, saver); err == nil || posts != 2 {
		t.Fatal("changed authorization replaced the saved operation")
	}
	// A receipt is historical evidence, but still must identify this exact result.
	goodReceipt := humanMarshal(t, receipt)
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"another operation", func(r map[string]any) { r["operation"].(map[string]any)["id"] = "other-op" }},
		{"another subject", func(r map[string]any) {
			r["commit"].(map[string]any)["resources"].([]any)[0].(map[string]any)["resource"].(map[string]any)["id"] = "other-subject"
		}},
		{"unchanged revision", func(r map[string]any) {
			r["commit"].(map[string]any)["resources"].([]any)[0].(map[string]any)["revision"] = "r1"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			receipt = humanJSON(t, goodReceipt)
			tc.change(receipt)
			if _, err := restarted.Recover(t.Context(), restored); err == nil {
				t.Fatal("accepted mismatched migration receipt")
			}
		})
	}
}
