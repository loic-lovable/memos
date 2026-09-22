package humanattributes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/require"
)

func TestDecodeValuesPreservesExactValuesAndRequiredMembers(t *testing.T) {
	p := profile(t)
	raw := []byte(`{"displayName":"  Maya Å  ","name":{"formatted":"","givenName":"Maya","familyName":"Å","middleName":""},"department":"","locale":"EN-us","timezone":"US/Eastern","emails":[{"entry_id":"work","value":"Maya+HR@Example.COM","primary":false,"expected_generation":null},{"entry_id":"home","value":"a@b","primary":true,"type":"home","expected_generation":"opaque"}]}`)
	before := bytes.Clone(raw)
	values, err := p.DecodeValues(raw)
	require.NoError(t, err)
	require.Equal(t, Values{DisplayName: pointer("  Maya Å  "), Name: &Name{Formatted: pointer(""), GivenName: pointer("Maya"), FamilyName: pointer("Å"), MiddleName: pointer("")}, Department: pointer(""), Locale: pointer("EN-us"), Timezone: pointer("US/Eastern"),
		Emails: pointer([]EmailInput{{EntryID: "work", Value: "Maya+HR@Example.COM"}, {EntryID: "home", Value: "a@b", Type: pointer("home"), Primary: true, ExpectedGeneration: pointer("opaque")}})}, values)
	require.Equal(t, before, raw)
	for i := range raw {
		raw[i] = 'x'
	}
	require.Equal(t, "  Maya Å  ", *values.DisplayName, "decoded values must not alias caller bytes")
	empty, err := p.DecodeValues([]byte(`{}`))
	require.NoError(t, err)
	require.Equal(t, Values{}, empty)
	empty, err = p.DecodeValues([]byte(`{"emails":[]}`))
	require.NoError(t, err)
	require.NotNil(t, empty.Emails)
	require.NotNil(t, *empty.Emails)
	require.Empty(t, *empty.Emails)
}

func TestDecodeValuesRejectsClosedShapeViolations(t *testing.T) {
	p := profile(t)
	for _, tc := range []struct{ name, raw string }{
		{"scalar alias", `{"displayname":"Maya"}`},
		{"name alias", `{"name":{"GivenName":"Maya"}}`},
		{"unknown attribute", `{"private-field":"private-value"}`},
		{"unknown name member", `{"name":{"givenName":"Maya","private-field":"private-value"}}`},
		{"empty name", `{"name":{}}`},
		{"name string", `{"name":"Maya"}`},
		{"name null", `{"name":null}`},
		{"name component null", `{"name":{"givenName":null}}`},
		{"scalar null", `{"displayName":null}`},
		{"scalar bool", `{"department":true}`},
		{"scalar number", `{"displayName":1e9999}`},
		{"emails null", `{"emails":null}`},
		{"emails object", `{"emails":{}}`},
		{"email null", `{"emails":[null]}`},
		{"mailbox shorthand", `{"emails":["a@b"]}`},
		{"invalid locale", `{"locale":"en-a-test-A-again"}`},
		{"unknown timezone", `{"timezone":"utc"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values, err := p.DecodeValues([]byte(tc.raw))
			require.ErrorIs(t, err, ErrInvalidValue)
			require.Equal(t, Values{}, values)
			require.NotContains(t, err.Error(), "private-field")
			require.NotContains(t, err.Error(), "private-value")
		})
	}
	for _, member := range []string{"entry_id", "value", "primary", "expected_generation"} {
		t.Run("missing email "+member, func(t *testing.T) {
			entry := map[string]any{"entry_id": "work", "value": "a@b", "primary": false, "expected_generation": nil}
			delete(entry, member)
			raw, err := json.Marshal(map[string]any{"emails": []any{entry}})
			require.NoError(t, err)
			values, err := p.DecodeValues(raw)
			require.ErrorIs(t, err, ErrInvalidValue)
			require.Equal(t, Values{}, values)
		})
	}
	for _, tc := range []struct {
		name, member string
		value        any
	}{
		{"primary null", "primary", nil}, {"primary number", "primary", 0}, {"primary string", "primary", "false"},
		{"generation bool", "expected_generation", false}, {"generation empty", "expected_generation", ""},
		{"type null", "type", nil}, {"type unknown", "type", "personal"},
		{"read generation", "generation", "private-value"}, {"unknown member", "private-field", "private-value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := map[string]any{"entry_id": "work", "value": "a@b", "primary": false, "expected_generation": nil}
			entry[tc.member] = tc.value
			raw, err := json.Marshal(map[string]any{"emails": []any{entry}})
			require.NoError(t, err)
			values, err := p.DecodeValues(raw)
			require.ErrorIs(t, err, ErrInvalidValue)
			require.Equal(t, Values{}, values)
		})
	}
	var zero *Profile
	_, err := zero.DecodeValues([]byte(`{}`))
	require.ErrorIs(t, err, ErrInvalidConfiguration)
}

func TestDecodeChangesPreservesClearAndStatePreconditions(t *testing.T) {
	p := profile(t)
	changes, err := p.DecodeChanges([]byte(`{"set":{"department":"","emails":[]},"clear":["timezone"]}`))
	require.NoError(t, err)
	require.Equal(t, Changes{Set: Values{Department: pointer(""), Emails: pointer([]EmailInput{})}, Clear: []Field{Timezone}}, changes)
	for _, tc := range []struct{ name, raw string }{
		{"missing set", `{"clear":["department"]}`}, {"missing clear", `{"set":{"department":"new"}}`},
		{"null set", `{"set":null,"clear":["department"]}`}, {"null clear", `{"set":{"department":"new"},"clear":null}`},
		{"null clear entry", `{"set":{},"clear":[null]}`}, {"string clear", `{"set":{},"clear":"department"}`},
		{"extra member", `{"set":{},"clear":["department"],"extra":false}`},
		{"empty", `{"set":{},"clear":[]}`}, {"unknown clear", `{"set":{},"clear":["Department"]}`},
		{"duplicate clear", `{"set":{},"clear":["department","department"]}`},
		{"overlap", `{"set":{"department":"new"},"clear":["department"]}`},
		{"bad set", `{"set":{"department":null},"clear":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changes, err := p.DecodeChanges([]byte(tc.raw))
			require.ErrorIs(t, err, ErrInvalidValue)
			require.Equal(t, Changes{}, changes)
		})
	}
	initial, err := p.DecodeValues([]byte(`{"emails":[{"entry_id":"work","value":"a@b","primary":false,"expected_generation":null}]}`))
	require.NoError(t, err)
	created, err := p.Create(initial, "hr", "r1", counter())
	require.NoError(t, err)
	changes, err = p.DecodeChanges([]byte(`{"set":{"emails":[{"entry_id":"work","value":"b@b","primary":false,"expected_generation":"stale"}]},"clear":[]}`))
	require.NoError(t, err, "decoding does not claim to validate current-state preconditions")
	result, err := p.Update(created.State, changes, "hr", "r2", counter())
	require.ErrorIs(t, err, ErrRevisionConflict)
	require.Equal(t, Result{}, result)
	require.Equal(t, "a@b", (*created.State.Facts.Emails.Value)[0].Value)
}

func TestDecodeValuesAppliesConfiguredAndSemanticLimits(t *testing.T) {
	p, err := New(Config{MaxEmails: 1, TZDBVersion: "2025b", Timezones: []string{"UTC"}})
	require.NoError(t, err)
	values, err := p.DecodeValues([]byte(`{"emails":[{"entry_id":"a","value":"a@b","primary":false,"expected_generation":null},{"entry_id":"b","value":"b@b","primary":false,"expected_generation":null}]}`))
	require.ErrorIs(t, err, ErrLimitExceeded)
	require.Equal(t, Values{}, values)
	for _, tc := range []struct{ name, entries string }{
		{"duplicate key", `{"entry_id":"a","value":"a@b","primary":false,"expected_generation":null},{"entry_id":"a","value":"b@b","primary":false,"expected_generation":null}`},
		{"duplicate address", `{"entry_id":"a","value":"a@b","primary":false,"expected_generation":null},{"entry_id":"b","value":"a@b","primary":false,"expected_generation":null}`},
		{"multiple primary", `{"entry_id":"a","value":"a@b","primary":true,"expected_generation":null},{"entry_id":"b","value":"b@b","primary":true,"expected_generation":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values, err := profile(t).DecodeValues([]byte(`{"emails":[` + tc.entries + `]}`))
			require.ErrorIs(t, err, ErrInvalidValue)
			require.Equal(t, Values{}, values)
		})
	}
}

// Read the canonical SDK schema without adding a runtime dependency on the
// transport package. This also works in Memos's unchanged SDK source snapshot.
func attributeSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile("../../client/internal/schemas/human-attributes-v1.schema.json")
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, json.Unmarshal(raw, &document))
	compiler := jsonschema.NewCompiler()
	id := document["$id"].(string)
	require.NoError(t, compiler.AddResource(id, document))
	schema, err := compiler.Compile(id + "#/$defs/attributes")
	require.NoError(t, err)
	return schema
}

func TestDecodeValuesMatchesSchemaShape(t *testing.T) {
	p, schema := profile(t), attributeSchema(t)
	for _, tc := range []struct{ name, raw string }{
		{"empty", `{}`}, {"empty strings", `{"displayName":"","name":{"formatted":""},"department":""}`},
		{"email", `{"emails":[{"entry_id":"work","value":"a@b","primary":false,"expected_generation":null}]}`},
		{"preferences", `{"locale":"EN-us","timezone":"US/Eastern"}`},
		{"null", `{"name":null}`}, {"unknown", `{"givenName":"a"}`}, {"case", `{"Name":{"givenName":"a"}}`},
		{"missing email member", `{"emails":[{"entry_id":"work","value":"a@b","expected_generation":null}]}`},
		{"null type", `{"emails":[{"entry_id":"work","value":"a@b","type":null,"primary":false,"expected_generation":null}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var document any
			require.NoError(t, json.Unmarshal([]byte(tc.raw), &document))
			values, err := p.DecodeValues([]byte(tc.raw))
			require.Equal(t, schema.Validate(document) == nil, err == nil)
			if err == nil {
				encoded, err := json.Marshal(values)
				require.NoError(t, err)
				require.JSONEq(t, tc.raw, string(encoded))
			}
		})
	}
}

func ExampleProfile_DecodeChanges() {
	// Deliberately small test catalog. Applications must provide a complete,
	// verified upstream release catalog before enabling this profile.
	p, err := New(Config{MaxEmails: 1, TZDBVersion: "2025b", Timezones: []string{"UTC"}})
	if err != nil {
		panic(err)
	}
	changes, err := p.DecodeChanges([]byte(`{"set":{"department":""},"clear":["timezone"]}`))
	if err != nil {
		panic(err)
	}
	fmt.Printf("department=%q; clear=%s\n", *changes.Set.Department, changes.Clear[0])
	// Output: department=""; clear=timezone
}

func FuzzDecode(f *testing.F) {
	for _, raw := range []string{`{}`, `{"emails":[]}`, `{"name":{"givenName":"Maya"}}`, `{"displayName":"\ud83d\ude00"}`,
		`{"displayName":"\ud800"}`, `{"set":{"department":""},"clear":["timezone"]}`, `{"displayName":"x","displayName":"y"}`,
		`{"emails":[{"entry_id":"work","value":"a@b","primary":false,"expected_generation":null}]}`, string([]byte{0xff}), strings.Repeat("[", 100)} {
		f.Add([]byte(raw))
	}
	p, err := New(Config{MaxEmails: 1, TZDBVersion: "2025b", Timezones: []string{"UTC"}})
	require.NoError(f, err)
	f.Fuzz(func(t *testing.T, raw []byte) {
		before := bytes.Clone(raw)
		values, valueErr := p.DecodeValues(raw)
		changes, changeErr := p.DecodeChanges(raw)
		require.Equal(t, before, raw)
		if valueErr != nil {
			require.Equal(t, Values{}, values)
		} else {
			require.NoError(t, p.Validate(values))
			encoded, err := json.Marshal(values)
			require.NoError(t, err)
			if len(encoded) <= MaxJSONBytes {
				roundtrip, err := p.DecodeValues(encoded)
				require.NoError(t, err)
				require.Equal(t, values, roundtrip)
			}
		}
		if changeErr != nil {
			require.Equal(t, Changes{}, changes)
		} else {
			encoded, err := json.Marshal(changes)
			require.NoError(t, err)
			if len(encoded) <= MaxJSONBytes {
				roundtrip, err := p.DecodeChanges(encoded)
				require.NoError(t, err)
				require.Equal(t, changes, roundtrip)
			}
		}
	})
}
