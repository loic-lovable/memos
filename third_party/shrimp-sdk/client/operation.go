package client

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strconv"
	"time"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/profiles/scalar"
)

// Human identifies a new human in the caller's enrolled authority. SourceReference
// must be a stable source identifier, never an email-derived account link.
type Human struct {
	Authority       string
	SourceReference string
	DisplayName     string
	Department      *string
	Email           *string
}

// SubjectVersion binds an update or lifecycle decision to the observed subject state.
// A revision conflict requires a new decision, not silently refreshing this value.
type SubjectVersion struct{ ID, Revision, Authority string }

// Intent is an immutable, enrollment-bound request and its original window evidence.
// Persist Bytes before sending. It contains account data but no credentials.
// A zero Intent cannot be submitted.
type Intent struct {
	raw    string
	body   string
	id     string
	window string
}

// Bytes returns a copy of the complete intent for durable caller-owned storage.
func (i Intent) Bytes() []byte { return []byte(i.raw) }

// ID returns the original operation identifier.
func (i Intent) ID() string { return i.id }

// ReplayWindow returns the original operation namespace.
func (i Intent) ReplayWindow() string { return i.window }

// AuthenticateWrite obtains read/write authority. Renew explicitly after token
// expiry; an authentication error never authorizes replacing an operation ID.
func (c *Client) AuthenticateWrite(ctx context.Context) error { return c.authenticate(ctx, true) }

// ReadSubject returns a validated 0.2 human record after authentication/discovery.
func (c *Client) ReadSubject(ctx context.Context, id string) (map[string]any, error) {
	if c.profile.Version != "0.2" || c.selected == nil {
		return nil, errors.New("0.2 discovery is required")
	}
	return c.subject02(ctx, id, nil)
}

// PrepareCreate prepares one disabled human creation without submitting it.
func (c *Client) PrepareCreate(ctx context.Context, h Human) (Intent, error) {
	if h.Authority == "" || h.SourceReference == "" {
		return Intent{}, errors.New("authority and stable source reference are required")
	}
	attributes := map[string]any{"displayName": h.DisplayName}
	if h.Department != nil {
		attributes["department"] = *h.Department
	}
	if h.Email != nil {
		attributes["email"] = *h.Email
	}
	return c.prepare(ctx, h.Authority, map[string]any{"command_id": "c1", "action": "create_subject", "if_absent": true, "profile": "human", "attributes": attributes, "expires_at": nil, "source_reference": h.SourceReference})
}

// PrepareActivate prepares activation conditional on the supplied revision.
func (c *Client) PrepareActivate(ctx context.Context, s SubjectVersion) (Intent, error) {
	return c.prepareTransition(ctx, s, "activate")
}

// PrepareDisable prepares disable conditional on the supplied revision.
func (c *Client) PrepareDisable(ctx context.Context, s SubjectVersion) (Intent, error) {
	return c.prepareTransition(ctx, s, "disable")
}
func (c *Client) prepareTransition(ctx context.Context, s SubjectVersion, action string) (Intent, error) {
	if s.ID == "" || s.Revision == "" || s.Authority == "" {
		return Intent{}, errors.New("subject ID, revision and authority are required")
	}
	return c.prepare(ctx, s.Authority, map[string]any{"command_id": "c1", "action": action, "resource": map[string]any{"type": "subject", "id": s.ID}, "expected_revision": s.Revision})
}

func (c *Client) prepare(ctx context.Context, authority string, command map[string]any) (Intent, error) {
	if c.profile.Version != "0.2" {
		return Intent{}, errors.New("provisioning requires version 0.2")
	}
	if err := c.lifecycleReady02(); err != nil {
		return Intent{}, err
	}
	r, err := c.get(ctx, "/replay-window", "0.2")
	if err != nil {
		return Intent{}, err
	}
	if err = c.checked(r, "replay-window-v0.2.schema.json"); err != nil {
		return Intent{}, err
	}
	if !c.scope02(r.value) {
		return Intent{}, errors.New("replay window belongs to a different scope")
	}
	now, closes, _, minimum, err := windowTimes(r.value)
	if err != nil {
		return Intent{}, err
	}
	if minimum.Cmp(new(big.Rat).SetInt64(c.selected.Limits.MinRetention02)) < 0 {
		return Intent{}, errors.New("window shortened advertised retention")
	}
	deadline := now.Add(time.Duration(min(int64(120), c.selected.Limits.MaxAhead)) * time.Second)
	if closes.Before(deadline) {
		deadline = closes
	}
	request := map[string]any{"schema_version": "0.2", "operation": map[string]any{"id": rand.Text(), "replay_window": r.value["replay_window"]}, "expected_authority": authority, "required_capabilities": []string{}, "required_profiles": c.profile.DiscoveryProfiles(), "required_dependencies": []string{}, "reconciliation": nil, "execute_before": deadline.Format("2006-01-02T15:04:05Z"), "commands": []any{command}}
	if _, err := c.lifecycleBody02(request, mutation02); err != nil {
		return Intent{}, err
	}
	raw, err := json.Marshal(map[string]any{"resource": c.profile.Resource, "issuer": c.profile.Issuer, "tenant": c.profile.Tenant, "domain": c.profile.Domain, "client_id": c.profile.ClientID, "version": "0.2", "request": request, "replay_window_evidence": r.value})
	if err != nil {
		return Intent{}, err
	}
	return c.RestoreIntent(raw)
}

// RestoreIntent loads trusted caller-owned bytes under the original enrollment.
// It does not need new-work discovery: withdrawal of a catalog must not prevent
// recovery. Protect stored intents against tampering; this is validation, not a signature.
func (c *Client) RestoreIntent(raw []byte) (Intent, error) {
	if c.profile.Version != "0.2" {
		return Intent{}, errors.New("intent recovery requires version 0.2")
	}
	value, err := decodeJSON(raw)
	if err != nil {
		return Intent{}, err
	}
	entry, ok := value.(map[string]any)
	if !ok {
		return Intent{}, errors.New("intent must be an object")
	}
	for field, want := range map[string]string{"resource": c.profile.Resource, "issuer": c.profile.Issuer, "tenant": c.profile.Tenant, "domain": c.profile.Domain, "client_id": c.profile.ClientID, "version": "0.2"} {
		if entry[field] != want {
			return Intent{}, fmt.Errorf("intent enrollment mismatch: %s", field)
		}
	}
	request, ok := entry["request"].(map[string]any)
	if !ok || c.local[mutation02].Validate(request) != nil {
		return Intent{}, errors.New("invalid saved mutation")
	}
	commands := request["commands"].([]any)
	if len(commands) != 1 {
		return Intent{}, errors.New("only single human commands are supported")
	}
	command := commands[0].(map[string]any)
	switch command["action"] {
	case "create_subject":
		if command["profile"] != "human" {
			return Intent{}, errors.New("only human creation is supported")
		}
	case "activate", "disable", "update_subject":
	default:
		return Intent{}, errors.New("unsupported saved action")
	}
	window, ok := entry["replay_window_evidence"].(map[string]any)
	if !ok || c.local["replay-window-v0.2.schema.json"].Validate(window) != nil || !c.scope02(window) {
		return Intent{}, errors.New("invalid original replay window")
	}
	now, closes, _, _, err := windowTimes(window)
	if err != nil {
		return Intent{}, err
	}
	op := request["operation"].(map[string]any)
	deadline, err := time.Parse(time.RFC3339, request["execute_before"].(string))
	if err != nil || !deadline.After(now) || deadline.After(closes) || op["replay_window"] != window["replay_window"] {
		return Intent{}, errors.New("intent does not match its original window")
	}
	body, err := json.Marshal(request)
	if err != nil {
		return Intent{}, err
	}
	return Intent{raw: string(raw), body: string(body), id: op["id"].(string), window: op["replay_window"].(string)}, nil
}

// Submit saves the intent before sending its exact request. Call it again with
// the same intent to retry; it never generates a replacement operation. The saver
// must accept an identical existing intent and reject conflicting replacements.
// An error after sending leaves the outcome unknown. Inspect the saved identity.
// A returned receipt may be pending or failed: inspect its state before acknowledging success.
func (c *Client) Submit(ctx context.Context, intent Intent, save SaveIntent) (map[string]any, error) {
	if !c.writeScope {
		return nil, errors.New("write authentication is required")
	}
	original, err := c.RestoreIntent(intent.Bytes())
	if err != nil {
		return nil, err
	}
	if err := saveOperation(ctx, save, original.id, original.Bytes()); err != nil {
		return nil, unavailable("cannot durably save intent; no mutation sent: %v", err)
	}
	r, err := c.api(ctx, "POST", "/mutations", "0.2", original.body)
	if err != nil {
		return nil, err
	}
	if err := c.submissionReceipt(r); err != nil {
		return nil, err
	}
	if err := c.validateReceipt02(r.value); err != nil {
		return nil, err
	}
	op := r.value["operation"].(map[string]any)
	u, _ := url.Parse(c.profile.Resource)
	if op["id"] != original.id || op["replay_window"] != original.window || r.header.Get("Location") != u.Path+"/operations/"+original.window+"/"+original.id {
		return nil, errors.New("mutation receipt or Location identifies another operation")
	}
	if err := c.operationReceipt(original, r.value); err != nil {
		return nil, err
	}
	return r.value, nil
}

// Recover queries the original operation under current authority without sending
// a mutation or requiring new-work discovery. Absence never proves non-commit.
func (c *Client) Recover(ctx context.Context, intent Intent) (map[string]any, error) {
	original, err := c.RestoreIntent(intent.Bytes())
	if err != nil {
		return nil, err
	}
	receipt, err := c.Inspect(ctx, original.window, original.id)
	if err != nil {
		return nil, err
	}
	if err := c.operationReceipt(original, receipt); err != nil {
		return nil, err
	}
	return receipt, nil
}

func windowTimes(window map[string]any) (time.Time, time.Time, time.Time, *big.Rat, error) {
	now, e1 := time.Parse(time.RFC3339, window["server_time"].(string))
	closes, e2 := time.Parse(time.RFC3339, window["closes_at"].(string))
	retained, e3 := time.Parse(time.RFC3339, window["results_retained_until"].(string))
	minimum, ok := new(big.Rat).SetString(window["min_result_retention_seconds"].(json.Number).String())
	if e1 != nil || e2 != nil || e3 != nil || !now.Before(closes) || !ok || minimum.Cmp(new(big.Rat).SetInt64(retained.Unix()-closes.Unix())) > 0 {
		return now, closes, retained, minimum, errors.New("invalid replay window or retention promise")
	}
	return now, closes, retained, minimum, nil
}
func (c *Client) originalRetention(intent Intent, receipt map[string]any) error {
	value, err := decodeJSON(intent.Bytes())
	if err != nil {
		return err
	}
	window := value.(map[string]any)["replay_window_evidence"].(map[string]any)
	_, closes, retained, minimum, err := windowTimes(window)
	if err != nil {
		return err
	}
	receiptRetention, e1 := time.Parse(time.RFC3339, receipt["results_retained_until"].(string))
	receiptMinimum, ok := new(big.Rat).SetString(receipt["min_result_retention_seconds"].(json.Number).String())
	floor := closes
	if terminal, ok := receipt["terminal_at"].(string); ok {
		at, err := time.Parse(time.RFC3339, terminal)
		if err != nil {
			return err
		}
		if at.After(floor) {
			floor = at
		}
	}
	if e1 != nil || !ok || receiptRetention.Before(retained) || receiptMinimum.Cmp(minimum) < 0 || minimum.Cmp(new(big.Rat).SetInt64(receiptRetention.Unix()-floor.Unix())) > 0 {
		return errors.New("receipt shortened the original replay-window retention promise")
	}
	return nil
}

// A successful receipt must describe the command we actually submitted.
func (c *Client) operationReceipt(intent Intent, receipt map[string]any) error {
	if err := c.originalRetention(intent, receipt); err != nil {
		return err
	}
	if receipt["state"] != "succeeded" {
		return nil
	}
	value, err := decodeJSON([]byte(intent.body))
	if err != nil {
		return err
	}
	command := value.(map[string]any)["commands"].([]any)[0].(map[string]any)
	action := command["action"].(string)
	subject, previous := "", ""
	if action == "create_subject" {
		for _, item := range receipt["commit"].(map[string]any)["resources"].([]any) {
			resource := item.(map[string]any)
			ref := resource["resource"].(map[string]any)
			if resource["command_id"] != command["command_id"] {
				return errors.New("receipt identifies a different command")
			}
			if ref["type"] == "subject" {
				subject = ref["id"].(string)
			}
		}
	} else {
		subject = command["resource"].(map[string]any)["id"].(string)
		previous = command["expected_revision"].(string)
	}
	if subject == "" {
		return errors.New("receipt omitted created subject")
	}
	_, err = c.lifecycleReceipt02(receipt, subject, action, previous)
	return err
}

func (c *Client) submissionReceipt(r response) error {
	checked := r
	if r.status == 202 {
		checked.status = 200
	}
	if err := c.checked(checked, "receipt-v0.2.schema.json"); err != nil {
		return err
	}
	pending := r.value["state"] == "pending"
	if pending != (r.status == 202) {
		return errors.New("receipt state contradicts mutation HTTP status")
	}
	if pending {
		seconds, err := strconv.ParseUint(r.header.Get("Retry-After"), 10, 31)
		poll, ok := new(big.Rat).SetString(r.value["poll_after_seconds"].(json.Number).String())
		if len(r.header.Values("Retry-After")) != 1 || err != nil || !ok || poll.Cmp(new(big.Rat).SetInt64(int64(seconds))) != 0 {
			return errors.New("pending receipt has inconsistent Retry-After")
		}
	}
	return nil
}

// ScalarChanges updates the existing scalar representation, not human-attributes-v1.
// Omitted fields remain unchanged; Clear writes an owned null fact.
type ScalarChanges = scalar.Changes

// PrepareUpdate prepares an atomic scalar update under an observed subject revision.
func (c *Client) PrepareUpdate(ctx context.Context, s SubjectVersion, changes ScalarChanges) (Intent, error) {
	if s.ID == "" || s.Revision == "" || s.Authority == "" {
		return Intent{}, errors.New("subject ID, revision and authority are required")
	}
	if err := scalar.Validate(changes); err != nil {
		return Intent{}, err
	}
	set := changes.Set
	if set == nil {
		set = map[string]string{}
	}
	clear := changes.Clear
	if clear == nil {
		clear = []string{}
	}
	return c.prepare(ctx, s.Authority, map[string]any{"command_id": "c1", "action": "update_subject", "resource": map[string]any{"type": "subject", "id": s.ID}, "expected_revision": s.Revision, "set": set, "clear": clear})
}
