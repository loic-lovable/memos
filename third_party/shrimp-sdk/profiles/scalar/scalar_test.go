package scalar_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/profiles/scalar"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		change scalar.Changes
		valid  bool
	}{
		{name: "empty", valid: true},
		{name: "invalid UTF-8", change: scalar.Changes{Set: map[string]string{"email": "\xff"}}},
		{name: "too long", change: scalar.Changes{Set: map[string]string{"displayName": strings.Repeat("界", 1025)}}},
		{name: "maximum Unicode scalars", change: scalar.Changes{Set: map[string]string{"displayName": strings.Repeat("界", 1024)}}, valid: true},
		{name: "unknown set", change: scalar.Changes{Set: map[string]string{"locale": "en"}}},
		{name: "unknown clear", change: scalar.Changes{Clear: []string{"locale"}}},
		{name: "duplicate clear", change: scalar.Changes{Clear: []string{"email", "email"}}},
		{name: "overlap", change: scalar.Changes{Set: map[string]string{"email": ""}, Clear: []string{"email"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := scalar.Validate(tt.change); (err == nil) != tt.valid {
				t.Fatalf("Validate() = %v, valid = %v", err, tt.valid)
			}
		})
	}
}

func TestDecode(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		set   map[string]any
		clear []any
		valid bool
	}{
		{name: "empty", valid: true},
		{name: "set and clear", set: map[string]any{"email": "Case+Tag@Example.test"}, clear: []any{"department"}, valid: true},
		{name: "nonstring value", set: map[string]any{"email": nil}},
		{name: "nonstring name", clear: []any{42}},
		{name: "unknown", set: map[string]any{"locale": "en"}},
		{name: "overlap", set: map[string]any{"email": ""}, clear: []any{"email"}},
		{name: "duplicate", clear: []any{"email", "email"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := scalar.Decode(tt.set, tt.clear)
			if (err == nil) != tt.valid {
				t.Fatalf("Decode() = %v, valid = %v", err, tt.valid)
			}
			if !tt.valid {
				return
			}
			if got.Set == nil || got.Clear == nil || len(got.Set) != len(tt.set) || len(got.Clear) != len(tt.clear) {
				t.Fatalf("unexpected decoded shape: %#v", got)
			}
			for name, value := range tt.set {
				if got.Set[name] != value {
					t.Fatal("decoded value changed")
				}
			}
			for i, name := range tt.clear {
				if got.Clear[i] != name {
					t.Fatal("decoded clear changed")
				}
			}
		})
	}
}

func TestApplyPreservesExactFactsAndCopies(t *testing.T) {
	t.Parallel()
	name := " José\u0301 "
	current := map[string]scalar.Fact{"displayName": {Value: &name, Authority: "hr", Revision: "r1"}}
	result, err := scalar.Apply(
		current,
		scalar.Changes{Set: map[string]string{"email": "Case+Tag@Example.test", "department": ""}},
		"hr",
		"r2",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(current["displayName"], result["displayName"]) {
		t.Fatal("omitted fact changed")
	}
	if *result["email"].Value != "Case+Tag@Example.test" || *result["department"].Value != "" {
		t.Fatal("set values changed")
	}
	cleared, err := scalar.Apply(result, scalar.Changes{Clear: []string{"email"}}, "hr", "r3")
	if err != nil {
		t.Fatal(err)
	}
	if cleared["email"] != (scalar.Fact{Authority: "hr", Revision: "r3"}) {
		t.Fatal("clear must remain an owned, revisioned nil value")
	}
	if cleared["department"].Value == nil || cleared["department"].Revision != "r2" {
		t.Fatal("empty value or unchanged revision was lost")
	}
	*result["displayName"].Value = "result changed"
	if name != " José\u0301 " || *cleared["displayName"].Value != name {
		t.Fatal("output aliases input or a later output")
	}
	name = "input changed"
	if *result["displayName"].Value != "result changed" {
		t.Fatal("input aliases output")
	}
	delete(result, "displayName")
	if _, ok := current["displayName"]; !ok {
		t.Fatal("output map aliases input")
	}
	empty, err := scalar.Apply(nil, scalar.Changes{}, "hr", "r1")
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("absent facts were materialized: %#v, %v", empty, err)
	}
}

func TestApplyRejectsWithoutPartialChanges(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		change    scalar.Changes
		owner     string
		authority string
		revision  string
	}{
		{name: "foreign set", owner: "other", change: scalar.Changes{Set: map[string]string{"displayName": "new", "email": "new"}}},
		{name: "foreign clear", owner: "other", change: scalar.Changes{Set: map[string]string{"displayName": "new"}, Clear: []string{"email"}}},
		{name: "unowned set", change: scalar.Changes{Set: map[string]string{"email": "new"}}},
		{name: "unowned clear", change: scalar.Changes{Clear: []string{"email"}}},
		{name: "invalid UTF8", change: scalar.Changes{Set: map[string]string{"displayName": "\xff"}}},
		{name: "overlength", change: scalar.Changes{Set: map[string]string{"displayName": strings.Repeat("é", 1025)}}},
		{name: "unknown", change: scalar.Changes{Set: map[string]string{"locale": "en"}}},
		{name: "duplicate", change: scalar.Changes{Clear: []string{"displayName", "displayName"}}},
		{name: "overlap", change: scalar.Changes{Set: map[string]string{"displayName": "new"}, Clear: []string{"displayName"}}},
		{name: "missing authority", authority: "missing"},
		{name: "missing revision", revision: "missing"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			name, email := "before", "secret@example.test"
			current := map[string]scalar.Fact{
				"displayName": {Value: &name, Authority: "hr", Revision: "r1"},
				"email":       {Value: &email, Authority: tt.owner, Revision: "r0"},
			}
			beforeName, beforeEmail := name, email
			before := map[string]scalar.Fact{
				"displayName": {Value: &beforeName, Authority: "hr", Revision: "r1"},
				"email":       {Value: &beforeEmail, Authority: tt.owner, Revision: "r0"},
			}
			authority, revision := "hr", "r2"
			if tt.authority == "missing" {
				authority = ""
			}
			if tt.revision == "missing" {
				revision = ""
			}
			got, err := scalar.Apply(current, tt.change, authority, revision)
			if err == nil || got != nil {
				t.Fatalf("Apply() = %#v, %v; want nil result and error", got, err)
			}
			if len(err.Error()) > 100 || strings.Contains(err.Error(), email) {
				t.Fatal("error must be bounded and omit values")
			}
			if !reflect.DeepEqual(current, before) {
				t.Fatal("failed change mutated current facts")
			}
		})
	}
	value := strings.Repeat("é", 1024)
	got, err := scalar.Apply(nil, scalar.Changes{Set: map[string]string{"displayName": value}}, "hr", "r1")
	if err != nil || *got["displayName"].Value != value {
		t.Fatalf("1024 Unicode scalars must be accepted: %v", err)
	}
}
