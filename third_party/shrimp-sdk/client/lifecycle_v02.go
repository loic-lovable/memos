package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

const readRequest02 = "read-sync-v0.2.schema.json#/$defs/read_request"
const readResponse02 = "read-sync-v0.2.schema.json#/$defs/read_response"
const human02 = "resources-v0.2.schema.json#/$defs/human"
const mutation02 = "mutation-v0.2.schema.json#/$defs/request"

// Compile the definitions the remote client actually uses. Validating a
// definitions-only schema root would accept an arbitrary document.
func wireFragments(name string) []string {
	switch name {
	case "read-sync-v0.2.schema.json":
		return []string{"read_request", "read_response", "enumeration_request", "enumeration_page"}
	case "resources-v0.2.schema.json":
		return []string{"human"}
	case "mutation-v0.2.schema.json":
		return []string{"request"}
	default:
		return nil
	}
}

func (c *Client) lifecycleReady02() error {
	if !c.writeScope || c.selected == nil {
		return unavailable("lifecycle tests require write authentication and discovery")
	}
	for _, operation := range []string{"resource.read", "mutation.submit", "operation.read", "replay_window.read"} {
		if !slices.Contains(c.selected.Operations, operation) {
			return unavailable("required lifecycle operation is not advertised")
		}
	}
	if c.selected.Limits.MaxDepth < 5 || c.selected.Limits.MaxCommands < 1 || c.selected.Limits.MaxDependencies < 1 || c.selected.Limits.MaxAhead < 1 {
		return unavailable("advertised limits do not support the lifecycle test workflow")
	}
	for _, name := range []string{readRequest02, readResponse02, human02, mutation02, "receipt-v0.2.schema.json", "replay-window-v0.2.schema.json"} {
		if c.catalog[name] == nil {
			return unavailable("required lifecycle schema is not advertised")
		}
	}
	return nil
}

func (c *Client) scope02(value map[string]any) bool {
	scope, ok := value["scope"].(map[string]any)
	return ok && scope["resource"] == c.profile.Resource && scope["tenant"] == c.profile.Tenant &&
		scope["domain"] == c.profile.Domain && scope["schema_version"] == c.profile.Version
}

func (c *Client) subject02(ctx context.Context, id string, dependencies []string) (map[string]any, error) {
	if dependencies == nil {
		dependencies = []string{}
	}
	request := map[string]any{"schema_version": "0.2", "target": map[string]any{"resource": map[string]any{"type": "subject", "id": id}},
		"required_dependencies": dependencies, "wait_ms": 0, "required_profiles": c.profile.DiscoveryProfiles()}
	raw, err := c.lifecycleBody02(request, readRequest02)
	if err != nil {
		return nil, err
	}
	r, err := c.api(ctx, "POST", "/reads", "0.2", string(raw))
	if err != nil {
		return nil, err
	}
	if err := c.checked(r, readResponse02); err != nil {
		return nil, err
	}
	resource := r.value["record"].(map[string]any)
	if !c.scope02(r.value) || c.local[human02].Validate(resource) != nil || resource["id"] != id {
		return nil, errors.New("subject read identifies another scope or has no human representation")
	}
	return resource, nil
}

func (c *Client) lifecycleBody02(request map[string]any, name string) ([]byte, error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	parsed, err := decodeJSON(raw)
	if err != nil {
		return nil, err
	}
	if c.local[name].Validate(parsed) != nil || c.catalog[name] == nil || c.catalog[name].Validate(parsed) != nil || len(raw) > c.selected.Limits.MaxRequestBytes {
		return nil, unavailable("request exceeds the advertised lifecycle contract")
	}
	return raw, nil
}

func (c *Client) lifecycleReceipt02(receipt map[string]any, subject, action, previous string) (string, error) {
	if c.local["receipt-v0.2.schema.json"].Validate(receipt) != nil {
		return "", errors.New("invalid human lifecycle receipt")
	}
	if err := c.validateReceipt02(receipt); err != nil {
		return "", err
	}
	commit := receipt["commit"].(map[string]any)
	if receipt["state"] != "succeeded" || commit["state"] != "committed" {
		return "", unavailable("lifecycle operation did not succeed; inspect its saved identity")
	}
	if err := committedHumanReceipt02(receipt, subject, action, previous, "c1"); err != nil {
		return "", err
	}
	return commit["causal_token"].(string), nil
}

// committedHumanReceipt02 follows structural/scope validation and checks the
// original command whenever a receipt claims a commit, independently of whether
// its asynchronous effects have succeeded yet.
func committedHumanReceipt02(receipt map[string]any, subject, action, previous, command string) error {
	commit := receipt["commit"].(map[string]any)
	resources := commit["resources"].([]any)
	found, sourceFound := false, false
	for _, item := range resources {
		value := item.(map[string]any)
		ref := value["resource"].(map[string]any)
		if value["command_id"] != command {
			return errors.New("lifecycle receipt identifies a different command")
		}
		if ref["type"] == "subject" {
			if found || ref["id"] != subject || value["revision"] == previous {
				return errors.New("lifecycle receipt has a different subject or unchanged revision")
			}
			found = true
		} else if action != "create_subject" || ref["type"] != "source_reference" || sourceFound {
			return errors.New("lifecycle receipt includes an unexpected resource")
		} else {
			sourceFound = true
		}
	}
	if !found {
		return errors.New("lifecycle receipt omits the subject revision")
	}
	needsAdmission, admission := action == "disable" || action == "retire", false
	for _, item := range receipt["effects"].([]any) {
		effect := item.(map[string]any)
		switch effect["kind"] {
		case "admission_block":
			ref := effect["resource"].(map[string]any)
			if !needsAdmission || effect["state"] != "complete" || ref["type"] != "subject" || ref["id"] != subject {
				return errors.New("lifecycle receipt has an invalid admission block for this subject")
			}
			admission = true
		case "membership_projection", "assignment_projection", "profile":
			// These may remain pending or fail after the core commit. The receipt
			// schema still requires all effects complete before overall success.
		default:
			return errors.New("unexpected lifecycle effect")
		}
	}
	if needsAdmission && !admission {
		return errors.New("lifecycle receipt omits its admission block")
	}
	return nil
}

func (c *Client) lifecycle02(ctx context.Context, subject, action string, save SaveIntent) error {
	if action != "activate" && action != "disable" {
		return errors.New("unsupported lifecycle test action")
	}
	if err := c.lifecycleReady02(); err != nil {
		return err
	}
	resource, err := c.subject02(ctx, subject, nil)
	if err != nil {
		return err
	}
	intent, err := c.prepareTransition(ctx, SubjectVersion{ID: subject, Revision: resource["revision"].(string), Authority: resource["authority"].(string)}, action)
	if err != nil {
		return err
	}
	receipt, err := c.Submit(ctx, intent, save)
	if err != nil {
		return err
	}
	token, err := c.lifecycleReceipt02(receipt, subject, action, resource["revision"].(string))
	if err != nil {
		return err
	}
	observed, err := c.subject02(ctx, subject, []string{token})
	if err != nil {
		return err
	}
	want := "active"
	if action == "disable" {
		want = "disabled"
	}
	if observed["revision"] == resource["revision"] {
		return errors.New("causally dependent subject read reused the pre-mutation revision")
	}
	if observed["value"].(map[string]any)["lifecycle"] != want {
		return fmt.Errorf("causally dependent subject read did not show %s", want)
	}
	return nil
}
