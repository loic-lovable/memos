package client

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/profiles/humanattributes"
)

func TestReadHumanPreservesScalarFactsAndObservedVersion(t *testing.T) {
	t.Parallel()
	record := contractVector(t, "resource-human-valid")
	record["value"].(map[string]any)["attributes"] = map[string]any{
		"displayName": humanReadFact("", "directory", "name-1"),
		"department":  humanReadFact(nil, "hr", "department-1"),
	}
	c, calls := humanReadClient(t, record, nil)
	got, err := c.ReadHuman(t.Context(), "subject-1")
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || got.HumanAttributes != nil || got.AttributeProfile != "" || got.Lifecycle != "disabled" || got.Enterprise != nil {
		t.Fatalf("unexpected representation or request count: %+v, calls=%d", got, *calls)
	}
	if got.Version() != (SubjectVersion{ID: "subject-1", Revision: "r1", Authority: "source-1"}) {
		t.Fatalf("changed observed version: %+v", got.Version())
	}
	name, exists := got.ScalarAttributes["displayName"]
	if !exists || name.Value == nil || *name.Value != "" || name.Authority != "directory" || name.Revision != "name-1" {
		t.Fatalf("empty scalar fact changed: %+v", name)
	}
	department, exists := got.ScalarAttributes["department"]
	if !exists || department.Value != nil || department.Authority != "hr" || department.Revision != "department-1" {
		t.Fatalf("cleared scalar fact changed: %+v", department)
	}
	if _, exists := got.ScalarAttributes["email"]; exists {
		t.Fatal("absent email became a fact")
	}
}

func TestReadHumanPreservesHistoricalSelectedFacts(t *testing.T) {
	t.Parallel()
	record := contractVector(t, "resource-human-v1")
	value := record["value"].(map[string]any)
	value["lifecycle"] = "retired"
	value["attributes"] = map[string]any{
		"displayName": humanReadFact("  Maya 李  ", "directory", "name-1"),
		"name":        humanReadFact(map[string]any{"givenName": "李", "familyName": ""}, "directory", "structured-1"),
		"department":  humanReadFact(nil, "hr", "department-1"),
		"locale":      humanReadFact("EN-us", "preferences", "locale-1"),
		"timezone":    humanReadFact("US/Eastern", "preferences", "timezone-1"),
		"emails": humanReadFact([]any{
			map[string]any{"entry_id": "work", "value": "Maya+HR@Example.COM", "type": "work", "primary": true, "generation": "generation-old"},
			map[string]any{"entry_id": "home", "value": "maya@example.test", "primary": false, "generation": "generation-home"},
		}, "contacts", "email-1"),
	}
	c, _ := humanReadClient(t, record, nil)
	// Current new-write metadata is deliberately narrower than the stored facts.
	if err := json.Unmarshal([]byte(`{"human_attributes":{"max_emails":1,"tzdb_version":"2026z"}}`), c.selected); err != nil {
		t.Fatal(err)
	}
	got, err := c.ReadHuman(t.Context(), "subject-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.AttributeProfile != humanattributes.ID || got.ScalarAttributes != nil || got.HumanAttributes == nil || got.Lifecycle != "retired" {
		t.Fatalf("selected representation lost: %+v", got)
	}
	encoded, err := json.Marshal(got.HumanAttributes)
	if err != nil {
		t.Fatal(err)
	}
	decoded := humanJSON(t, encoded)
	if !reflect.DeepEqual(decoded, value["attributes"]) {
		t.Fatalf("typed facts changed values, generations or metadata: %s", encoded)
	}
	if got.HumanAttributes.Name.Value.MiddleName != nil || got.HumanAttributes.Name.Value.FamilyName == nil || *got.HumanAttributes.Name.Value.FamilyName != "" {
		t.Fatal("omitted and empty name components were conflated")
	}
	if got.HumanAttributes.Department == nil || got.HumanAttributes.Department.Value != nil {
		t.Fatal("owned clear was lost")
	}
	if (*got.HumanAttributes.Emails.Value)[1].Type != nil {
		t.Fatal("an email type was inferred")
	}
}

func TestReadHumanPreservesAbsentClearedAndEmptyEmails(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"absent", "cleared", "empty"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			record := contractVector(t, "resource-human-v1")
			attributes := map[string]any{}
			if state == "cleared" {
				attributes["emails"] = humanReadFact(nil, "contacts", "email-1")
			} else if state == "empty" {
				attributes["emails"] = humanReadFact([]any{}, "contacts", "email-1")
			}
			record["value"].(map[string]any)["attributes"] = attributes
			c, _ := humanReadClient(t, record, nil)
			got, err := c.ReadHuman(t.Context(), "subject-1")
			if err != nil {
				t.Fatal(err)
			}
			if got.HumanAttributes == nil || got.ScalarAttributes != nil {
				t.Fatal("empty selected representation changed")
			}
			fact := got.HumanAttributes.Emails
			switch state {
			case "absent":
				if fact != nil {
					t.Fatal("absent email became a fact")
				}
			case "cleared":
				if fact == nil || fact.Value != nil || fact.Authority != "contacts" || fact.Revision != "email-1" {
					t.Fatalf("owned-null email changed: %+v", fact)
				}
			case "empty":
				if fact == nil || fact.Value == nil || *fact.Value == nil || len(*fact.Value) != 0 {
					t.Fatalf("explicitly empty email collection changed: %+v", fact)
				}
			}
		})
	}
}

func TestReadHumanPreservesEnterpriseOnEitherRepresentation(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"resource-human-valid", "resource-human-v1"} {
		t.Run(shape, func(t *testing.T) {
			t.Parallel()
			record := contractVector(t, shape)
			enterprise := map[string]any{
				"profile": "enterprise-attributes-v1",
				"context": map[string]any{"employer": "employer-李", "issuer": "issuer-1", "generation": "selection-12"},
				"attributes": map[string]any{
					"employeeNumber": humanReadFact("00421", "hr", "employee-1"),
					"title":          humanReadFact(" Ingénieure 李 ", "directory", "title-1"),
					"costCenter":     humanReadFact(nil, "finance", "cost-1"),
					"organization":   humanReadFact("", "hr", "organization-1"),
					"division":       humanReadFact("Nordics", "hr", "division-1"),
				},
			}
			record["value"].(map[string]any)["enterprise"] = enterprise
			c, _ := humanReadClient(t, record, nil)
			got, err := c.ReadHuman(t.Context(), "subject-1")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(enterprise, humanJSON(t, got.Enterprise)) {
				t.Fatalf("enterprise data changed: %s", got.Enterprise)
			}
			if shape == "resource-human-valid" && (got.ScalarAttributes == nil || got.HumanAttributes != nil) {
				t.Fatal("enterprise selection migrated an empty legacy representation")
			}
		})
	}
}

func TestReadHumanRetainsRawReadValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		damage func(map[string]any)
	}{
		{name: "wrong scope", damage: func(v map[string]any) { v["scope"].(map[string]any)["tenant"] = "other" }},
		{name: "wrong id", damage: func(v map[string]any) { v["record"].(map[string]any)["id"] = "another-subject" }},
		{name: "missing selector", damage: func(v map[string]any) {
			delete(v["record"].(map[string]any)["value"].(map[string]any), "attribute_profile")
		}},
		{name: "unknown selector", damage: func(v map[string]any) {
			v["record"].(map[string]any)["value"].(map[string]any)["attribute_profile"] = "future"
		}},
		{name: "null attributes", damage: func(v map[string]any) { v["record"].(map[string]any)["value"].(map[string]any)["attributes"] = nil }},
		{name: "missing field owner", damage: func(v map[string]any) {
			attrs := v["record"].(map[string]any)["value"].(map[string]any)["attributes"].(map[string]any)
			delete(attrs["displayName"].(map[string]any), "authority")
		}},
		{name: "scalar email in rich facts", damage: func(v map[string]any) {
			attrs := v["record"].(map[string]any)["value"].(map[string]any)["attributes"].(map[string]any)
			attrs["email"] = humanReadFact("a@b", "contacts", "r1")
		}},
		{name: "missing email generation", damage: func(v map[string]any) {
			attrs := v["record"].(map[string]any)["value"].(map[string]any)["attributes"].(map[string]any)
			emails := attrs["emails"].(map[string]any)["value"].([]any)
			delete(emails[0].(map[string]any), "generation")
		}},
		{name: "malformed enterprise", damage: func(v map[string]any) { v["record"].(map[string]any)["value"].(map[string]any)["enterprise"] = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, calls := humanReadClient(t, contractVector(t, "resource-human-v1"), tc.damage)
			got, err := c.ReadHuman(t.Context(), "subject-1")
			if err == nil || !reflect.DeepEqual(got, HumanRecord{}) || *calls != 1 {
				t.Fatalf("malformed response returned partial success: %+v, %v, calls=%d", got, err, *calls)
			}
		})
	}
}

func TestReadHumanRequiresExistingAuthenticationAndDiscovery(t *testing.T) {
	t.Parallel()
	for _, prerequisite := range []string{"discovery", "authentication", "wire version"} {
		t.Run(prerequisite, func(t *testing.T) {
			t.Parallel()
			c, calls := humanReadClient(t, contractVector(t, "resource-human-valid"), nil)
			switch prerequisite {
			case "discovery":
				c.selected = nil
			case "authentication":
				c.token = ""
			case "wire version":
				c.profile.Version = "0.1"
			}
			got, err := c.ReadHuman(t.Context(), "subject-1")
			if err == nil || !reflect.DeepEqual(got, HumanRecord{}) || *calls != 0 {
				t.Fatalf("missing prerequisite contacted peer or returned data: %+v, %v, calls=%d", got, err, *calls)
			}
		})
	}
}

func humanReadFact(value any, authority, revision string) map[string]any {
	return map[string]any{"value": value, "authority": authority, "revision": revision}
}

func humanReadClient(t *testing.T, record map[string]any, damage func(map[string]any)) (*Client, *int) {
	t.Helper()
	c := operationClient(t)
	c.writeScope = false
	calls := new(int)
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		*calls++
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/reads") || r.Header.Get("SHRIMP-Version") != "0.2" || r.Header.Get("Authorization") != "DPoP fixture-token" || r.Header.Get("DPoP") == "" {
			t.Fatalf("typed read changed transport or authority: %s %s", r.Method, r.URL.Path)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		request := humanJSON(t, raw)
		if request["target"].(map[string]any)["resource"].(map[string]any)["id"] != "subject-1" {
			t.Fatal("typed read changed subject identity")
		}
		response := map[string]any{
			"scope": operationScope(c), "record": record,
			"observed_frontier": "frontier-1", "state_validator": "validator-1",
			"validator_expires_at": "2026-09-18T00:01:00Z",
		}
		if damage != nil {
			damage(response)
		}
		return operationResponse(response, ""), nil
	})
	return c, calls
}
