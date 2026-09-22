package client

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/client/internal/schemas"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const schemaBase = "https://shrimp.example/schemas/0.1/"
const draft = "https://json-schema.org/draft/2020-12/schema"

func (c *Client) schemaName(name string) string {
	if c.profile.Version == "0.2" {
		return name + "-v0.2.schema.json"
	}
	return name + ".schema.json"
}

func (c *Client) receiptSchemas() []string {
	if c.profile.Version == "0.2" {
		return []string{"receipt-v0.2.schema.json"}
	}
	return []string{"operation-receipt.schema.json", "group-receipt.schema.json"}
}

type noLoader struct{}

func (noLoader) Load(string) (any, error) {
	return nil, errors.New("schema reference leaves supplied catalog")
}

func compileSchemas(documents map[string]map[string]any) (map[string]*jsonschema.Schema, error) {
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(noLoader{})
	ids := make(map[string]string, len(documents))
	seen := make(map[string]bool, len(documents))
	for name, doc := range documents {
		id, ok := doc["$id"].(string)
		if !ok || id == "" || seen[id] {
			return nil, errors.New("missing or duplicate schema identifier")
		}
		ids[name], seen[id] = id, true
		// Embedded schemas span versions. Register their canonical IDs so relative
		// references resolve within the declared namespace without network loading.
		if err := compiler.AddResource(id, doc); err != nil {
			return nil, errors.New("invalid schema resource")
		}
	}
	compiled := make(map[string]*jsonschema.Schema, len(documents))
	for name, id := range ids {
		schema, err := compiler.Compile(id)
		if err != nil {
			return nil, errors.New("invalid schema or unresolved catalog reference")
		}
		compiled[name] = schema
		for _, fragment := range wireFragments(name) {
			if definitions, ok := documents[name]["$defs"].(map[string]any); !ok || definitions[fragment] == nil {
				continue
			}
			key := name + "#/$defs/" + fragment
			compiled[key], err = compiler.Compile(id + "#/$defs/" + fragment)
			if err != nil {
				return nil, errors.New("invalid wire schema definition")
			}
		}
	}
	return compiled, nil
}

var localSchemas = sync.OnceValues(func() (map[string]*jsonschema.Schema, error) {
	files, err := schemas.Files.ReadDir(".")
	if err != nil {
		return nil, err
	}
	docs := map[string]map[string]any{}
	for _, file := range files {
		raw, err := schemas.Files.ReadFile(file.Name())
		if err != nil {
			return nil, err
		}
		value, err := decodeJSON(raw)
		if err != nil {
			return nil, err
		}
		docs[file.Name()] = value.(map[string]any)
	}
	return compileSchemas(docs)
})

type contract struct {
	ID             string   `json:"id"`
	SchemaVersion  string   `json:"schema_version"`
	Authentication string   `json:"authentication_profile"`
	Operations     []string `json:"operations"`
	Profiles       []string `json:"profiles"`
	Enumeration    struct {
		ResourceTypes []string `json:"resource_types"`
	} `json:"enumeration"`
	Schemas []struct{ ID, Path, SHA256 string } `json:"schemas"`
	Limits  struct {
		MaxRequestBytes           int   `json:"max_request_bytes"`
		MinRetention              int64 `json:"min_result_retention_after_window_seconds"`
		MinRetention02            int64 `json:"min_result_retention_seconds"`
		MaxAhead                  int64 `json:"max_execute_ahead_seconds"`
		MaxDepth                  int   `json:"max_json_depth"`
		MaxDependencies           int   `json:"max_required_dependencies"`
		MaxCommands               int   `json:"max_commands"`
		MaxResponseBytes          int   `json:"max_response_bytes"`
		MaxEnumerationPageRecords int   `json:"max_enumeration_page_records"`
	} `json:"limits"`
}

var schemaPath = regexp.MustCompile(`^/schemas/[A-Za-z0-9][A-Za-z0-9._-]*\.json$`)

// Discover validates scope and supported version before following bounded schema
// paths. Advertised schemas cannot weaken the client's embedded receipt contract.
func (c *Client) Discover(ctx context.Context) (Diagnostics, error) {
	c.selected, c.catalog, c.versions, c.schemaDigests = nil, nil, nil, nil
	r, err := c.get(ctx, "/capabilities", "")
	if err != nil {
		return Diagnostics{}, err
	}
	if r.status != 200 || !media(r.header, "application/json") {
		if r.status == 401 || r.status == 403 {
			return Diagnostics{}, unavailable("capability discovery refused (HTTP %d); verify current read authority", r.status)
		}
		return Diagnostics{}, fmt.Errorf("capability discovery refused (HTTP %d)", r.status)
	}
	if c.local[c.schemaName("discovery")].Validate(r.value) != nil {
		return Diagnostics{}, errors.New("invalid discovery document")
	}
	scope := r.value["scope"].(map[string]any)
	if r.value["resource"] != c.profile.Resource || scope["tenant"] != c.profile.Tenant || scope["domain"] != c.profile.Domain {
		return Diagnostics{}, errors.New("discovery resource or tenant/domain differs from enrolled connection")
	}
	var discovery struct {
		Versions []contract `json:"versions"`
	}
	if err := json.Unmarshal(r.raw, &discovery); err != nil {
		return Diagnostics{}, errors.New("invalid discovery version data")
	}
	seen := map[string]bool{}
	var selected *contract
	for _, v := range discovery.Versions {
		if seen[v.ID] {
			return Diagnostics{}, errors.New("duplicate advertised version")
		}
		seen[v.ID] = true
		if v.ID == c.profile.Version {
			selected = &v
		}
	}
	if selected == nil || selected.SchemaVersion != c.profile.Version || selected.Authentication != "oauth-client-credentials-dpop-0.1" ||
		!slices.Contains(selected.Operations, "operation.read") {
		return Diagnostics{}, unavailable("configured version or read workflow unavailable")
	}
	for _, required := range c.profile.DiscoveryProfiles() {
		if !slices.Contains(selected.Profiles, required) {
			return Diagnostics{}, unavailable("required profiles unavailable; verify the connection's required_profiles")
		}
	}
	if len(selected.Schemas) == 0 || len(selected.Schemas) > 32 {
		return Diagnostics{}, errors.New("schema catalog must contain 1–32 entries")
	}
	docs := map[string]map[string]any{}
	digests := map[string]string{}
	for _, descriptor := range selected.Schemas {
		if !schemaPath.MatchString(descriptor.Path) || strings.Contains(descriptor.Path, "..") {
			return Diagnostics{}, errors.New("unsafe schema catalog path")
		}
		name := path.Base(descriptor.Path)
		// This reader supports the enrolled version's schema namespace. A vendor
		// namespace is not permission to change the interpretation of a receipt.
		base := "https://shrimp.example/schemas/" + c.profile.Version + "/"
		if descriptor.ID != base+name || docs[name] != nil {
			return Diagnostics{}, errors.New("unsupported or duplicate schema catalog identity")
		}
		response, err := c.get(ctx, descriptor.Path, c.profile.Version)
		if err != nil {
			return Diagnostics{}, err
		}
		if response.status != 200 || !media(response.header, "application/schema+json") || fmt.Sprintf("%x", sha256.Sum256(response.raw)) != descriptor.SHA256 {
			return Diagnostics{}, errors.New("schema retrieval or integrity check failed")
		}
		if response.value["$id"] != descriptor.ID || response.value["$schema"] != draft {
			return Diagnostics{}, errors.New("schema identifier or draft mismatch")
		}
		docs[name] = response.value
		digests[name] = descriptor.SHA256
	}
	for _, doc := range docs {
		if err := catalogRefs(doc, docs, true); err != nil {
			return Diagnostics{}, err
		}
	}
	catalog, err := compileSchemas(docs)
	if err != nil {
		return Diagnostics{}, err
	}
	hasReceipt := false
	for _, name := range c.receiptSchemas() {
		hasReceipt = hasReceipt || catalog[name] != nil
	}
	if !hasReceipt {
		return Diagnostics{}, errors.New("no supported receipt schema advertised")
	}
	c.catalog, c.versions = catalog, seen
	c.schemaDigests = digests
	c.selected = selected
	return c.diagnostics(selected.Operations, selected.Profiles), nil
}

func catalogRefs(value any, docs map[string]map[string]any, root bool) error {
	switch v := value.(type) {
	case map[string]any:
		if _, exists := v["$id"]; exists && !root {
			return errors.New("nested schema identifiers are unsupported")
		}
		for _, keyword := range []string{"$ref", "$dynamicRef"} {
			if ref, exists := v[keyword]; exists {
				text, ok := ref.(string)
				if !ok {
					return errors.New("invalid schema reference")
				}
				name := strings.SplitN(text, "#", 2)[0]
				if name != "" && docs[name] == nil {
					return errors.New("schema reference leaves catalog")
				}
			}
		}
		for _, child := range v {
			if err := catalogRefs(child, docs, false); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range v {
			if err := catalogRefs(child, docs, false); err != nil {
				return err
			}
		}
	}
	return nil
}
