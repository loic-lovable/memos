package store

import "context"

// ShrimpAudit is a bounded, redacted operational event, not the optional wire audit API.
// Supplied operation identifiers are correlation only unless OperationKnown is true.
type ShrimpAudit struct {
	Sequence        int64    `json:"sequence"`
	ID              string   `json:"id"`
	AttemptID       string   `json:"attempt_id"`
	Actor           string   `json:"actor"`
	Authority       string   `json:"authority"`
	OccurredAt      int64    `json:"occurred_at"`
	RecordedAt      int64    `json:"recorded_at"`
	Kind            string   `json:"kind"`
	Action          string   `json:"action,omitempty"`
	Window          string   `json:"replay_window,omitempty"`
	Operation       string   `json:"operation_id,omitempty"`
	OperationKnown  bool     `json:"operation_known"`
	Commit          string   `json:"commit"`
	Code            string   `json:"code,omitempty"`
	Stage           string   `json:"stage,omitempty"`
	NativeUserID    int32    `json:"native_user_id,omitempty"`
	SubjectID       string   `json:"subject_id,omitempty"`
	SourceID        string   `json:"source_id,omitempty"`
	Revision        string   `json:"revision,omitempty"`
	CausalToken     string   `json:"causal_token,omitempty"`
	Dependencies    []string `json:"dependencies,omitempty"`
	Effect          string   `json:"effect,omitempty"`
	OriginalAttempt string   `json:"original_attempt,omitempty"`
}

// ShrimpAuditPage contains an append-ordered slice and the current retained coverage.
type ShrimpAuditPage struct {
	Events     []ShrimpAudit `json:"events"`
	Epoch      string        `json:"epoch"`
	Since      int64         `json:"retained_since"`
	Boundary   int64         `json:"boundary"`
	Next       int64         `json:"next"`
	More       bool          `json:"more"`
	Unresolved int64         `json:"unresolved_attempts"`
	LegacyGap  bool          `json:"history_before_audit"`
}

// ShrimpAuditDriver is separate from the portable protocol interface.
type ShrimpAuditDriver interface {
	BeginShrimpAudit(context.Context, string, string, string) (*ShrimpAudit, error)
	IdentifyShrimpNativeAudit(context.Context, ShrimpAudit, int32) error
	IdentifyShrimpAudit(context.Context, ShrimpAudit) error
	FinishShrimpAudit(context.Context, ShrimpAudit) error
	InspectShrimpAudit(context.Context, string, int64, int) (*ShrimpAuditPage, error)
}

type shrimpAuditKey struct{}

// WithShrimpAudit carries the trusted request marker to the commit transaction.
func WithShrimpAudit(ctx context.Context, event *ShrimpAudit) context.Context {
	return context.WithValue(ctx, shrimpAuditKey{}, event)
}

// ShrimpAuditFromContext returns the authenticated request marker, if present.
func ShrimpAuditFromContext(ctx context.Context) *ShrimpAudit {
	event, _ := ctx.Value(shrimpAuditKey{}).(*ShrimpAudit)
	return event
}

// BeginShrimpNativeAudit is a no-op outside the explicitly enabled pilot.
func (s *Store) BeginShrimpNativeAudit(ctx context.Context, actor, action string) (context.Context, error) {
	if !s.shrimpPilot {
		return ctx, nil
	}
	event, err := s.driver.(ShrimpAuditDriver).BeginShrimpAudit(ctx, actor, "memos-native-administration", action)
	if err != nil {
		return ctx, err
	}
	return WithShrimpAudit(ctx, event), nil
}

// FinishShrimpNativeAudit records known native rejections without guessing storage outcomes.
func (s *Store) FinishShrimpNativeAudit(ctx context.Context, code string, knownRejection bool) error {
	event := ShrimpAuditFromContext(ctx)
	if event == nil {
		return nil
	}
	outcome := *event
	outcome.Code, outcome.Stage = code, "native_api"
	if knownRejection {
		outcome.Commit = "not_committed"
	}
	return s.driver.(ShrimpAuditDriver).FinishShrimpAudit(ctx, outcome)
}

// ShrimpPilotEnabled reports the explicit disposable deployment mode.
func (s *Store) ShrimpPilotEnabled() bool { return s.shrimpPilot }

// IdentifyShrimpNativeAudit attaches a resolved target after native authorization.
func (s *Store) IdentifyShrimpNativeAudit(ctx context.Context, userID int32) error {
	event := ShrimpAuditFromContext(ctx)
	if event == nil {
		return nil
	}
	return s.driver.(ShrimpAuditDriver).IdentifyShrimpNativeAudit(ctx, *event, userID)
}
