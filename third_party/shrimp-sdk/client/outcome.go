package client

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// OutcomeState describes a validated operation result, not current account state.
type OutcomeState string

const (
	OutcomeUnknown    OutcomeState = "unknown"
	OutcomePending    OutcomeState = "pending"
	OutcomeSucceeded  OutcomeState = "succeeded"
	OutcomeFailed     OutcomeState = "failed"
	OutcomeSuperseded OutcomeState = "superseded"
)

// CommitState distinguishes a known commit from a rejection or uncertain commit.
type CommitState string

const (
	CommitUnknown      CommitState = "unknown"
	CommitNotCommitted CommitState = "not_committed"
	CommitCommitted    CommitState = "committed"
)

// EffectState is the recorded progress of one required postcommit obligation.
type EffectState string

const (
	EffectNotStarted EffectState = "not_started"
	EffectPending    EffectState = "pending"
	EffectComplete   EffectState = "complete"
	EffectFailed     EffectState = "failed"
)

// Outcome holds a receipt returned through SubmitTyped or RecoverTyped. A nil
// error only means a trusted receipt was obtained; inspect State and Commit.
// A zero Outcome means unknown, never success or proof that nothing committed.
type Outcome struct {
	Receipt *Receipt
}

// State returns the recorded outcome. It refuses a success label that conflicts
// with commit or effect completion. This is not a validator for caller-created
// receipts, an authorization check, or evidence that historical state is current.
func (o Outcome) State() OutcomeState {
	r := o.Receipt
	if r == nil {
		return OutcomeUnknown
	}
	switch r.State {
	case OutcomePending, OutcomeFailed, OutcomeSuperseded:
		return r.State
	case OutcomeSucceeded:
		if r.Commit.State != CommitCommitted || r.Commit.CausalToken == nil || *r.Commit.CausalToken == "" ||
			len(r.Commit.Resources) == 0 || r.TerminalAt == nil || r.Error != nil || r.SupersededBy != nil || r.PollAfterSeconds != nil {
			return OutcomeUnknown
		}
		for _, effect := range r.Effects {
			if effect.State != EffectComplete || effect.ObservedFrontier == nil || *effect.ObservedFrontier == "" || effect.Error != nil {
				return OutcomeUnknown
			}
		}
		return OutcomeSucceeded
	default:
		return OutcomeUnknown
	}
}

// Receipt is the complete HTTP/JSON 0.2 receipt shape. Wrappers populate it only
// after the raw client's schema, scope, operation and retention checks succeed.
// Decoding JSON into this struct alone does not establish those guarantees.
type Receipt struct {
	SchemaVersion             string          `json:"schema_version"`
	Scope                     ReceiptScope    `json:"scope"`
	Operation                 Operation       `json:"operation"`
	AcceptedAt                time.Time       `json:"accepted_at"`
	State                     OutcomeState    `json:"state"`
	TerminalAt                *time.Time      `json:"terminal_at"`
	Commit                    ReceiptCommit   `json:"commit"`
	Effects                   []ReceiptEffect `json:"effects"`
	Error                     *ReceiptError   `json:"error"`
	SupersededBy              *Operation      `json:"superseded_by"`
	ResultsRetainedUntil      time.Time       `json:"results_retained_until"`
	MinResultRetentionSeconds json.Number     `json:"min_result_retention_seconds"`
	PollAfterSeconds          *int            `json:"poll_after_seconds"`
}

// ReceiptScope binds historical evidence to its target and observation scope.
type ReceiptScope struct {
	Resource             string `json:"resource"`
	Tenant               string `json:"tenant"`
	Domain               string `json:"domain"`
	HistoryEpoch         string `json:"history_epoch"`
	SchemaVersion        string `json:"schema_version"`
	AuthorizationContext string `json:"authorization_context"`
}

// Operation identifies work within the receipt's enrolled operation namespace.
type Operation struct {
	ID           string `json:"id"`
	ReplayWindow string `json:"replay_window"`
}

// ReceiptCommit reports the business commit independently of the overall result.
type ReceiptCommit struct {
	State       CommitState         `json:"state"`
	CausalToken *string             `json:"causal_token"`
	Resources   []CommittedResource `json:"resources"`
}

// CommittedResource identifies a command's historical committed revision.
type CommittedResource struct {
	CommandID string      `json:"command_id"`
	Resource  ResourceRef `json:"resource"`
	Revision  string      `json:"revision"`
}

// ReceiptEffect reports one required effect separately from the core commit.
// PolicyID is present only for an erasure effect.
type ReceiptEffect struct {
	ID               string        `json:"id"`
	Kind             string        `json:"kind"`
	Resource         ResourceRef   `json:"resource"`
	Consumer         string        `json:"consumer"`
	State            EffectState   `json:"state"`
	Deadline         time.Time     `json:"deadline"`
	ObservedFrontier *string       `json:"observed_frontier"`
	Error            *ReceiptError `json:"error"`
	PolicyID         *string       `json:"policy_id,omitempty"`
}

// ReceiptError describes a recorded failure; it is not a transport error.
// A committed failure does not imply rollback or authorize blind resubmission.
type ReceiptError struct {
	Code      string  `json:"code"`
	Stage     string  `json:"stage"`
	CommandID *string `json:"command_id"`
	Recovery  string  `json:"recovery"`
}

// SubmitTyped preserves Submit's persistence, request identity and validation.
// Pending, failed and superseded receipts return nil error with their true state.
// Any error returns an unknown Outcome with no receipt; recover the saved intent.
func (c *Client) SubmitTyped(ctx context.Context, intent Intent, save SaveIntent) (Outcome, error) {
	receipt, err := c.Submit(ctx, intent, save)
	if err != nil {
		return Outcome{}, err
	}
	return typedOutcome(receipt)
}

// RecoverTyped obtains the original operation's historical result through Recover.
// It needs current read authority, not new-write discovery or a replacement intent.
// Any lookup or validation error leaves the outcome unknown and exposes no receipt.
func (c *Client) RecoverTyped(ctx context.Context, intent Intent) (Outcome, error) {
	receipt, err := c.Recover(ctx, intent)
	if err != nil {
		return Outcome{}, err
	}
	return typedOutcome(receipt)
}

// Called only after Submit or Recover has validated the full receipt and its
// operation bindings. Do not use this conversion as independent wire validation.
func typedOutcome(value map[string]any) (Outcome, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return Outcome{}, fmt.Errorf("encode validated receipt: %w", err)
	}
	var receipt Receipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return Outcome{}, fmt.Errorf("decode typed receipt: %w", err)
	}
	return Outcome{Receipt: &receipt}, nil
}
