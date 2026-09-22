package client

import "testing"

func TestCompileSchemasVersionedReferences(t *testing.T) {
	t.Parallel()
	const base02 = "https://shrimp.example/schemas/0.2/"
	for _, test := range []struct {
		name string
		ref  string
		fail bool
	}{
		{"local reference", "common-v0.2.schema.json#/$defs/value", false},
		{"missing reference", "missing.schema.json", true},
		{"wrong version", schemaBase + "common-v0.2.schema.json#/$defs/value", true},
		{"external reference", "https://other.example/schema.json", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			documents := map[string]map[string]any{
				"legacy.schema.json": {"$id": schemaBase + "legacy.schema.json", "$schema": draft, "type": "integer"},
				"common-v0.2.schema.json": {
					"$id": base02 + "common-v0.2.schema.json", "$schema": draft,
					"$defs": map[string]any{"value": map[string]any{"type": "string"}},
				},
				"request-v0.2.schema.json": {
					"$id": base02 + "request-v0.2.schema.json", "$schema": draft, "$ref": test.ref,
				},
			}
			compiled, err := compileSchemas(documents)
			if test.fail {
				if err == nil {
					t.Fatal("accepted a reference outside the supplied catalog")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := compiled["request-v0.2.schema.json"].Validate("value"); err != nil {
				t.Fatalf("valid 0.2 value rejected: %v", err)
			}
			if compiled["request-v0.2.schema.json"].Validate(1) == nil {
				t.Fatal("0.2 reference constraint was not enforced")
			}
			if err := compiled["legacy.schema.json"].Validate(1); err != nil {
				t.Fatalf("valid 0.1 value rejected: %v", err)
			}
		})
	}
}

func TestCompileSchemasRejectsInvalidIDs(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		id   any
	}{
		{"missing", nil},
		{"empty", ""},
		{"non-string", 1},
		{"duplicate", schemaBase + "one.schema.json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := compileSchemas(map[string]map[string]any{
				"one.schema.json": {"$id": schemaBase + "one.schema.json", "$schema": draft},
				"two.schema.json": {"$id": test.id, "$schema": draft},
			})
			if err == nil {
				t.Fatal("accepted a missing or duplicate schema identity")
			}
		})
	}
}

func TestLocalSchemas(t *testing.T) {
	t.Parallel()
	compiled, err := localSchemas()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"discovery.schema.json", "discovery-v0.2.schema.json", "read-sync-v0.2.schema.json"} {
		if compiled[name] == nil {
			t.Errorf("missing embedded schema %s", name)
		}
	}
}
