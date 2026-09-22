// Package servertest checks a bounded set of server.Application transaction and
// recovery contracts against application-owned fixtures. It supplies no storage
// and does not establish native admission enforcement or full conformance.
package servertest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/server"
)

// Factory creates a fresh enrolled application for each subtest. The host owns
// its disposable storage, enrollment and cleanup through t.Cleanup. It must enable
// the compatibility scalar create/update and activate/disable operations.
type Factory func(t *testing.T) Fixture

// Fixture describes an application under test, not an SDK server configuration.
// Principal and Authority identify the host's trusted test enrollment.
type Fixture struct {
	Application server.Application
	Principal   string
	Authority   string
	// Restart closes and reopens the same durable application state, returning
	// a new adapter. The host owns cleanup. A nil hook explicitly skips recovery
	// after restart; a reopen is not evidence of arbitrary process-crash safety.
	Restart func(t *testing.T) server.Application
	// AtomicSubjectUpdates opts into the compound scalar/lifecycle case only
	// when the application implements that transaction boundary.
	AtomicSubjectUpdates bool
}

// Run executes four isolated subtests. Missing optional integration hooks are
// reported as skips, never passes. Native authorization/admission and transport
// checks must be exercised separately by the host application.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	if factory == nil {
		t.Fatal("servertest: fixture factory is required")
	}
	for _, name := range []string{"immutable_replay", "failed_precondition", "restart_recovery", "compound_update"} {
		t.Run(name, func(t *testing.T) {
			fixture := factory(t)
			if fixture.Application == nil || fixture.Principal == "" || fixture.Authority == "" {
				t.Fatal("servertest: application, principal and authority are required")
			}
			var err error
			switch name {
			case "immutable_replay":
				err = replay(t.Context(), fixture)
			case "failed_precondition":
				err = precondition(t.Context(), fixture)
			case "restart_recovery":
				if fixture.Restart == nil {
					t.Skip("host supplied no durable restart hook")
				}
				err = restart(t.Context(), fixture, func() server.Application { return fixture.Restart(t) })
			case "compound_update":
				if !fixture.AtomicSubjectUpdates {
					t.Skip("host did not opt into atomic scalar/lifecycle updates")
				}
				err = compound(t.Context(), fixture)
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func replay(ctx context.Context, f Fixture) error {
	r, create, created, err := begin(ctx, f)
	if err != nil {
		return err
	}
	first := r.mutation("update_subject", &created.Subject)
	first.Set = map[string]string{"displayName": "First observed name"}
	first = seal(first)
	accepted, err := r.success(ctx, first)
	if err != nil {
		return err
	}
	evidence := snapshot(accepted)
	later := r.mutation("update_subject", &accepted.Subject)
	later.Set = map[string]string{"displayName": "Later current name"}
	later = seal(later)
	current, err := r.success(ctx, later)
	if err != nil {
		return err
	}
	for _, flags := range []struct{ recover, unsupported bool }{{}, {true, false}, {false, true}, {true, true}} {
		retry := first
		retry.RecoverOnly, retry.UnsupportedProfiles = flags.recover, flags.unsupported
		got, err := r.apply(ctx, retry)
		if err != nil || snapshot(got) != evidence {
			return fmt.Errorf("retained replay changed or revalidated old intent (recover=%t unsupported=%t): %w", flags.recover, flags.unsupported, nonNilError(err))
		}
		if err := r.unchanged(ctx, current); err != nil {
			return err
		}
	}
	conflict := first
	conflict.Set = map[string]string{"displayName": "Conflicting replacement"}
	conflict = seal(conflict)
	conflict.RecoverOnly, conflict.UnsupportedProfiles = true, true
	if _, err := r.apply(ctx, conflict); !errors.Is(err, server.ErrReplayConflict) {
		return fmt.Errorf("conflicting retained intent must return ErrReplayConflict before current restrictions: %w", nonNilError(err))
	}
	for _, unsupported := range []bool{false, true} {
		blocked := r.mutation("update_subject", &current.Subject)
		blocked.Set = map[string]string{"displayName": "Forbidden new work"}
		blocked = seal(blocked)
		blocked.RecoverOnly, blocked.UnsupportedProfiles = !unsupported, unsupported
		want := server.ErrInsufficientScope
		if unsupported {
			want = server.ErrUnsupportedProfile
		}
		if _, err := r.apply(ctx, blocked); !errors.Is(err, want) {
			return fmt.Errorf("new restricted work must return %v: %w", want, nonNilError(err))
		}
	}
	if err := r.unchanged(ctx, current); err != nil {
		return err
	}
	// Creation evidence must also remain historical after later mutations.
	return r.retained(ctx, create, snapshot(created))
}

func precondition(ctx context.Context, f Fixture) error {
	r, _, created, err := begin(ctx, f)
	if err != nil {
		return err
	}
	activate := seal(r.mutation("activate", &created.Subject))
	active, err := r.success(ctx, activate)
	if err != nil {
		return err
	}
	stale := r.mutation("update_subject", &created.Subject)
	stale.Set = map[string]string{"displayName": "Must not appear"}
	stale = seal(stale)
	failed, err := r.rejected(ctx, stale, "revision_conflict", "mutation_rejected")
	if err != nil {
		return err
	}
	if err := r.unchanged(ctx, active); err != nil {
		return err
	}
	return r.retained(ctx, stale, snapshot(failed))
}

func restart(ctx context.Context, f Fixture, reopen func() server.Application) error {
	r, create, created, err := begin(ctx, f)
	if err != nil {
		return err
	}
	evidence := snapshot(created)
	active, err := r.success(ctx, seal(r.mutation("activate", &created.Subject)))
	if err != nil {
		return err
	}
	r.app = reopen()
	if r.app == nil {
		return errors.New("restart hook returned no application")
	}
	if err := r.retained(ctx, create, evidence); err != nil {
		return err
	}
	create.RecoverOnly, create.UnsupportedProfiles = true, true
	got, err := r.apply(ctx, create)
	if err != nil || snapshot(got) != evidence {
		return fmt.Errorf("restart lost exact retained replay: %w", nonNilError(err))
	}
	return r.unchanged(ctx, active)
}

func compound(ctx context.Context, f Fixture) error {
	r, _, created, err := begin(ctx, f)
	if err != nil {
		return err
	}
	active, err := r.success(ctx, seal(r.mutation("activate", &created.Subject)))
	if err != nil {
		return err
	}
	invalid := r.mutation("update_subject", &active.Subject)
	invalid.Set = map[string]string{"displayName": strings.Repeat("x", 1025)}
	invalid.Lifecycle = "disabled"
	invalid = seal(invalid)
	failed, err := r.rejected(ctx, invalid, "invalid_request", "mutation_rejected")
	if err != nil {
		return err
	}
	if err := r.unchanged(ctx, active); err != nil {
		return fmt.Errorf("failed compound field changed facts or lifecycle: %w", err)
	}
	if err := r.retained(ctx, invalid, snapshot(failed)); err != nil {
		return err
	}
	change := r.mutation("update_subject", &active.Subject)
	change.Set = map[string]string{"displayName": "Atomic disabled name"}
	change.Lifecycle = "disabled"
	change = seal(change)
	disabled, err := r.success(ctx, change)
	if err != nil {
		return err
	}
	if disabled.Lifecycle != "disabled" || disabled.Subject.Lifecycle != "disabled" {
		return errors.New("compound result omitted its lifecycle completion evidence")
	}
	evidence := snapshot(disabled)
	current, err := r.success(ctx, seal(r.mutation("activate", &disabled.Subject)))
	if err != nil {
		return err
	}
	change.RecoverOnly, change.UnsupportedProfiles = true, true
	got, err := r.apply(ctx, change)
	if err != nil || snapshot(got) != evidence {
		return fmt.Errorf("compound replay changed retained facts or lifecycle completion evidence: %w", nonNilError(err))
	}
	if err := r.retained(ctx, change, evidence); err != nil {
		return err
	}
	return r.unchanged(ctx, current)
}

type runner struct {
	app                          server.Application
	principal, authority, window string
	deadline                     int64
}

func begin(ctx context.Context, f Fixture) (*runner, server.Mutation, *server.Result, error) {
	window, closes, err := f.Application.Window(ctx, f.Principal)
	if err != nil {
		return nil, server.Mutation{}, nil, fmt.Errorf("open execution window: %w", err)
	}
	if window == "" || closes <= time.Now().Unix()+1 {
		return nil, server.Mutation{}, nil, errors.New("application returned no usable execution window")
	}
	r := &runner{app: f.Application, principal: f.Principal, authority: f.Authority, window: window, deadline: min(closes-1, time.Now().Unix()+120)}
	m := r.mutation("create_subject", nil)
	m.SourceReference = "servertest-" + rand.Text()
	m.Set = map[string]string{"displayName": "Initial owned name"}
	m.DisplayName = m.Set["displayName"]
	m = seal(m)
	created, err := r.success(ctx, m)
	if err != nil {
		return nil, server.Mutation{}, nil, err
	}
	return r, m, created, nil
}

func (r *runner) mutation(action string, current *server.Subject) server.Mutation {
	m := server.Mutation{Principal: r.principal, Authority: r.authority, Window: r.window, ID: rand.Text(), Action: action, CommandID: "c1", Deadline: r.deadline}
	if current != nil {
		m.SubjectID, m.ExpectedRevision = current.ID, current.Revision
	}
	return m
}

func seal(m server.Mutation) server.Mutation {
	intent := m
	intent.Fingerprint, intent.RecoverOnly, intent.UnsupportedProfiles = "", false, false
	raw, err := json.Marshal(intent)
	if err != nil {
		panic(err) // The kit constructs scalar-only JSON-compatible intents.
	}
	digest := sha256.Sum256(raw)
	m.Fingerprint = hex.EncodeToString(digest[:])
	return m
}

func (r *runner) apply(ctx context.Context, m server.Mutation) (*server.Result, error) {
	attempt, err := r.app.BeginAudit(ctx, m.Principal, m.Authority, "mutation")
	if err != nil {
		return nil, fmt.Errorf("begin mutation audit: %w", err)
	}
	if attempt == nil {
		return nil, errors.New("BeginAudit returned no context")
	}
	err = r.app.IdentifyAudit(attempt, m.Window, m.ID)
	var result *server.Result
	if err == nil {
		result, err = r.app.Apply(attempt, m)
	}
	outcome := server.AuditOutcome{}
	if err != nil {
		// Unclassified failures keep commit unknown. Never label a storage error
		// as a known non-commit merely to finish the audit fixture.
		outcome = server.AuditOutcome{Stage: "commit", Code: "storage_unavailable"}
		for _, known := range []struct {
			err         error
			code, stage string
		}{
			{server.ErrReplayConflict, "replay_conflict", "commit"},
			{server.ErrUnsupportedProfile, "unsupported_profile", "acceptance"},
			{server.ErrInsufficientScope, "insufficient_scope", "authorization"},
		} {
			if errors.Is(err, known.err) {
				outcome = server.AuditOutcome{Stage: known.stage, Code: known.code, Commit: "not_committed"}
				break
			}
		}
	}
	finish, cancel := context.WithTimeout(context.WithoutCancel(attempt), 5*time.Second)
	defer cancel()
	if finishErr := r.app.FinishAudit(finish, outcome); finishErr != nil {
		return nil, errors.Join(err, fmt.Errorf("finish mutation audit: %w", finishErr))
	}
	return result, err
}

func (r *runner) success(ctx context.Context, m server.Mutation) (*server.Result, error) {
	result, err := r.apply(ctx, m)
	if err != nil {
		return nil, fmt.Errorf("%s failed: %w", m.Action, err)
	}
	if result == nil || result.Error != "" || result.Token == "" || result.Time <= 0 || result.RetainedUntil <= result.Time ||
		result.Action != m.Action || result.CommandID != m.CommandID || result.Subject.ID == "" || result.Subject.Revision == "" {
		return nil, errors.New("successful mutation omitted committed result evidence")
	}
	if m.SubjectID != "" && (result.Subject.ID != m.SubjectID || result.Subject.Revision == m.ExpectedRevision) {
		return nil, errors.New("successful mutation changed subject identity or retained its old revision")
	}
	for field, value := range m.Set {
		fact, exists := result.Subject.Attributes[field]
		if !exists || fact.Value == nil || *fact.Value != value || fact.Authority != m.Authority || fact.Revision == "" {
			return nil, errors.New("successful mutation omitted exact owned scalar facts")
		}
	}
	wantLifecycle := m.Lifecycle
	switch m.Action {
	case "create_subject", "disable":
		wantLifecycle = "disabled"
	case "activate":
		wantLifecycle = "active"
	}
	if wantLifecycle != "" && result.Subject.Lifecycle != wantLifecycle {
		return nil, errors.New("successful mutation omitted the requested lifecycle state")
	}
	if err := r.unchanged(ctx, result); err != nil {
		return nil, err
	}
	return copyResult(result), nil
}

func (r *runner) rejected(ctx context.Context, m server.Mutation, codes ...string) (*server.Result, error) {
	result, err := r.apply(ctx, m)
	if err != nil {
		return nil, fmt.Errorf("expected retained known rejection, received uncertain application error: %w", err)
	}
	if result == nil || !slices.Contains(codes, result.Error) || result.Token != "" ||
		result.Action != m.Action || result.CommandID != m.CommandID || result.Time <= 0 || result.RetainedUntil <= result.Time {
		return nil, errors.New("failed precondition did not return its known rejection without a commit token")
	}
	return copyResult(result), nil
}

func (r *runner) unchanged(ctx context.Context, expected *server.Result) error {
	subject, _, err := r.app.Read(ctx, expected.Subject.ID, []string{expected.Token})
	if err != nil {
		return fmt.Errorf("read committed subject with its causal evidence: %w", err)
	}
	if subject == nil || snapshot(subject) != snapshot(expected.Subject) {
		return errors.New("observed subject facts or lifecycle differ from expected committed state")
	}
	return nil
}

func (r *runner) retained(ctx context.Context, m server.Mutation, evidence string) error {
	result, err := r.app.Result(ctx, m.Principal, m.Window, m.ID)
	if err != nil {
		return fmt.Errorf("recover retained result: %w", err)
	}
	if result == nil || snapshot(result) != evidence {
		return errors.New("retained result differs from original immutable evidence")
	}
	return nil
}

func snapshot(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err) // Subjects and results contain only JSON-compatible SDK types.
	}
	return string(raw)
}

func copyResult(value *server.Result) *server.Result {
	var copied server.Result
	if err := json.Unmarshal([]byte(snapshot(value)), &copied); err != nil {
		panic(err)
	}
	return &copied
}

func nonNilError(err error) error {
	if err == nil {
		return errors.New("unexpected application outcome")
	}
	return err
}
