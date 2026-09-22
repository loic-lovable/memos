package client

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"slices"
	"time"
)

// LifecycleReady checks the bounded synchronous human workflow before setup.
func (c *Client) LifecycleReady() error {
	if c.profile.Version == "0.2" {
		return c.lifecycleReady02()
	}
	if !c.writeScope || c.selected == nil {
		return unavailable("lifecycle tests require write authentication and discovery")
	}
	for _, operation := range []string{"subject.read", "subject.activate", "subject.disable", "operation.read", "replay_window.read"} {
		if !slices.Contains(c.selected.Operations, operation) {
			return unavailable("required lifecycle operation is not advertised")
		}
	}
	if c.selected.Limits.MaxDepth < 4 || c.selected.Limits.MaxCommands < 1 || c.selected.Limits.MaxDependencies < 1 || c.selected.Limits.MaxAhead < 1 {
		return unavailable("advertised limits do not support the lifecycle test workflow")
	}
	for _, name := range []string{"subject-read.schema.json", "subject-mutation.schema.json", "operation-receipt.schema.json", "replay-window.schema.json"} {
		if c.catalog[name] == nil {
			return unavailable("required lifecycle schema is not advertised")
		}
	}
	return nil
}

func (c *Client) checked(r response, name string) error {
	if r.status != 200 {
		return unavailable("lifecycle observation unavailable (HTTP %d)", r.status)
	}
	if !media(r.header, "application/json") || c.local[name].Validate(r.value) != nil {
		return errors.New("lifecycle response violates the embedded wire schema")
	}
	return nil
}

func (c *Client) subject(ctx context.Context, id string, dependencies []string) (map[string]any, error) {
	if c.profile.Version == "0.2" {
		return c.subject02(ctx, id, dependencies)
	}
	if dependencies == nil {
		dependencies = []string{}
	}
	body, _ := json.Marshal(map[string]any{"schema_version": c.profile.Version, "resource": map[string]any{"type": "subject", "id": id},
		"representation": "subject", "required_dependencies": dependencies, "wait_ms": 0})
	r, err := c.api(ctx, "POST", "/reads", c.profile.Version, string(body))
	if err != nil {
		return nil, err
	}
	if err := c.checked(r, "subject-read.schema.json"); err != nil {
		return nil, err
	}
	resource, ok := r.value["resource"].(map[string]any)
	if !ok || c.local["subject.schema.json"].Validate(resource) != nil || resource["id"] != id {
		return nil, errors.New("subject read identifies another resource or has no representation")
	}
	return resource, nil
}

// VerifyCreated checks the setup adapter's receipt and a causally dependent read.
// Isolation and creation provenance remain obligations of the trusted adapter.
func (c *Client) VerifyCreated(ctx context.Context, subject, window, id string) error {
	receipt, err := c.Inspect(ctx, window, id)
	if err != nil {
		return err
	}
	token, err := c.lifecycleReceipt(receipt, subject, "create_subject", "")
	if err != nil {
		return err
	}
	resource, err := c.subject(ctx, subject, []string{token})
	if err != nil {
		return err
	}
	if resource["value"].(map[string]any)["lifecycle"] != "disabled" {
		return errors.New("setup subject is not disabled")
	}
	return nil
}

func (c *Client) lifecycleReceipt(receipt map[string]any, subject, action, previous string) (string, error) {
	if c.profile.Version == "0.2" {
		return c.lifecycleReceipt02(receipt, subject, action, previous)
	}
	if c.local["operation-receipt.schema.json"].Validate(receipt) != nil {
		return "", errors.New("invalid human lifecycle receipt")
	}
	commit := receipt["commit"].(map[string]any)
	if receipt["state"] == "pending" {
		return "", unavailable("lifecycle operation is still pending; inspect its saved identity")
	}
	if receipt["state"] != "succeeded" || commit["state"] != "committed" {
		return "", unavailable("lifecycle operation did not succeed; inspect its saved identity")
	}
	resource := commit["resources"].([]any)[0].(map[string]any)
	if resource["id"] != subject || resource["revision"] == previous {
		return "", errors.New("lifecycle receipt has a different subject or unchanged revision")
	}
	effects := receipt["effects"].([]any)
	if action == "disable" {
		if len(effects) != 1 {
			return "", errors.New("disable receipt omits its admission block")
		}
		effect := effects[0].(map[string]any)
		if effect["state"] != "complete" || effect["resource"].(map[string]any)["id"] != subject {
			return "", errors.New("disable receipt has no completed admission block for this subject")
		}
	} else if len(effects) != 0 {
		return "", errors.New("unexpected lifecycle effect")
	}
	return commit["causal_token"].(string), nil
}

// Lifecycle performs one conditional activate/disable with a fresh, caller-persisted
// operation identity. It never retries an uncertain outcome with a new identity.
// The caller must restrict subject to its newly created isolated test fixture.
func (c *Client) Lifecycle(ctx context.Context, subject, action string, save SaveIntent) error {
	if c.profile.Version == "0.2" {
		return c.lifecycle02(ctx, subject, action, save)
	}
	if action != "activate" && action != "disable" {
		return errors.New("unsupported lifecycle test action")
	}
	if err := c.LifecycleReady(); err != nil {
		return err
	}
	resource, err := c.subject(ctx, subject, nil)
	if err != nil {
		return err
	}
	r, err := c.get(ctx, "/replay-window", c.profile.Version)
	if err != nil {
		return err
	}
	if err := c.checked(r, "replay-window.schema.json"); err != nil {
		return err
	}
	now, e1 := time.Parse(time.RFC3339, r.value["server_time"].(string))
	closes, e2 := time.Parse(time.RFC3339, r.value["closes_at"].(string))
	retained, e3 := time.Parse(time.RFC3339, r.value["results_retained_until"].(string))
	if e1 != nil || e2 != nil || e3 != nil || !now.Before(closes) || !closes.Before(retained) ||
		retained.Sub(closes).Seconds() < float64(c.selected.Limits.MinRetention) {
		return errors.New("invalid replay window or retention promise")
	}
	deadline := now.Add(time.Duration(min(int64(120), c.selected.Limits.MaxAhead)) * time.Second)
	if closes.Before(deadline) {
		deadline = closes
	}
	id, window := rand.Text(), r.value["replay_window"].(string)
	request := map[string]any{"schema_version": c.profile.Version, "operation": map[string]any{"id": id, "replay_window": window},
		"execute_before": deadline.Format("2006-01-02T15:04:05Z"), "required_capabilities": []string{}, "required_dependencies": []string{},
		"commands": []any{map[string]any{"action": action, "resource": map[string]any{"type": "subject", "id": subject}, "expected_revision": resource["revision"]}}}
	body, _ := json.Marshal(request)
	parsed, err := decodeJSON(body)
	if err != nil {
		return err
	}
	if c.local["subject-mutation.schema.json"].Validate(parsed) != nil || c.catalog["subject-mutation.schema.json"].Validate(parsed) != nil || len(body) > c.selected.Limits.MaxRequestBytes {
		return unavailable("mutation exceeds the advertised request contract")
	}
	// The journal binds the exact intent to its enrollment without retaining keys.
	entry, _ := json.Marshal(map[string]any{"resource": c.profile.Resource, "issuer": c.profile.Issuer, "tenant": c.profile.Tenant,
		"domain": c.profile.Domain, "client_id": c.profile.ClientID, "version": c.profile.Version, "request": request})
	if err := saveOperation(ctx, save, id, entry); err != nil {
		return unavailable("cannot durably save lifecycle intent; no mutation sent")
	}
	r, err = c.api(ctx, "POST", "/mutations", c.profile.Version, string(body))
	if err != nil {
		return err
	}
	if err := c.checked(r, "operation-receipt.schema.json"); err != nil {
		return err
	}
	u, _ := url.Parse(c.profile.Resource)
	href := u.Path + "/operations/" + window + "/" + id
	op := r.value["operation"].(map[string]any)
	if op["id"] != id || op["replay_window"] != window || op["href"] != href || r.header.Get("Location") != href {
		return errors.New("mutation receipt or Location identifies another operation")
	}
	token, err := c.lifecycleReceipt(r.value, subject, action, resource["revision"].(string))
	if err != nil {
		return err
	}
	observed, err := c.subject(ctx, subject, []string{token})
	if err != nil {
		return err
	}
	want := "active"
	if action == "disable" {
		want = "disabled"
	}
	if observed["value"].(map[string]any)["lifecycle"] != want {
		return fmt.Errorf("causally dependent subject read did not show %s", want)
	}
	return nil
}
