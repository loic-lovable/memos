package servertest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/server"
)

func TestRunAcceptsScriptedEvidence(t *testing.T) {
	scenarios := map[string]string{
		"immutable_replay": "replay", "failed_precondition": "precondition",
		"restart_recovery": "restart", "compound_update": "compound",
	}
	Run(t, func(t *testing.T) Fixture {
		_, name, _ := strings.Cut(t.Name(), "/")
		app := newScript(scenarios[name])
		t.Cleanup(func() {
			if app.next != len(app.steps) || app.begun != app.finished {
				t.Fatalf("unused script or unfinished audit: steps=%d/%d audits=%d/%d", app.next, len(app.steps), app.finished, app.begun)
			}
		})
		return Fixture{
			Application: app, Principal: "principal", Authority: "authority",
			Restart: func(*testing.T) server.Application { return app }, AtomicSubjectUpdates: true,
		}
	})
}

func TestScenariosDetectBrokenEvidence(t *testing.T) {
	for _, test := range []struct {
		name, scenario, want string
		breakScript          func(*script)
		reopen               func(*script) server.Application
	}{
		{
			name: "rewritten replay", scenario: "replay", want: "retained replay changed",
			breakScript: func(s *script) {
				s.steps[3] = func(server.Mutation) (*server.Result, error) {
					return copyResult(s.results[2]), nil // Later state is not the old result.
				}
			},
		},
		{
			name: "support checked before replay", scenario: "replay", want: "retained replay changed",
			breakScript: func(s *script) { s.steps[5] = fail(server.ErrUnsupportedProfile) },
		},
		{
			name: "wrong conflict category", scenario: "replay", want: "ErrReplayConflict",
			breakScript: func(s *script) { s.steps[7] = fail(server.ErrUnsupportedProfile) },
		},
		{
			name: "stale update changed state", scenario: "precondition", want: "observed subject facts or lifecycle",
			breakScript: func(s *script) { s.partialFailure(2) },
		},
		{
			name: "failed field disabled account", scenario: "compound", want: "failed compound field changed",
			breakScript: func(s *script) { s.partialFailure(2) },
		},
		{
			name: "replay lost lifecycle evidence", scenario: "compound", want: "compound replay changed",
			breakScript: func(s *script) {
				s.steps[5] = func(server.Mutation) (*server.Result, error) {
					result := copyResult(s.results[3])
					result.Lifecycle = ""
					return result, nil
				}
			},
		},
		{
			name: "restart lost result", scenario: "restart", want: "recover retained result",
			reopen: func(s *script) server.Application { clear(s.retained); return s },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := newScript(test.scenario)
			if test.breakScript != nil {
				test.breakScript(app)
			}
			err := runScript(t.Context(), test.scenario, app, test.reopen)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("broken adapter was not diagnosed as %q: %v", test.want, err)
			}
		})
	}
}

func TestUnknownFailureDoesNotClaimNonCommit(t *testing.T) {
	storageErr := errors.New("storage outcome cannot be determined")
	app := newScript("replay")
	app.steps = []step{fail(storageErr)}
	r := runner{app: app}
	_, err := r.apply(t.Context(), server.Mutation{Principal: "principal", Authority: "authority", Window: "window", ID: "operation"})
	if !errors.Is(err, storageErr) || app.outcome.Commit != "" || app.finished != 1 {
		t.Fatalf("unknown failure was hidden or classified as a non-commit: error=%v outcome=%+v finished=%d", err, app.outcome, app.finished)
	}
}

func runScript(ctx context.Context, scenario string, app *script, reopen func(*script) server.Application) error {
	f := Fixture{Application: app, Principal: "principal", Authority: "authority"}
	switch scenario {
	case "replay":
		return replay(ctx, f)
	case "precondition":
		return precondition(ctx, f)
	case "restart":
		return restart(ctx, f, func() server.Application {
			if reopen != nil {
				return reopen(app)
			}
			return app
		})
	case "compound":
		return compound(ctx, f)
	default:
		panic("unknown scenario")
	}
}

type step func(server.Mutation) (*server.Result, error)

// script returns a fixed sequence of outcomes. It deliberately implements no
// validation, authorization, replay lookup or transaction algorithm: these tests
// check the kit's observations, not a second adapter implementation.
type script struct {
	server.Application
	steps           []step
	next            int
	current         server.Subject
	results         []*server.Result
	retained        map[string]*server.Result
	begun, finished int
	outcome         server.AuditOutcome
}

func newScript(scenario string) *script {
	s := &script{retained: make(map[string]*server.Result)}
	s.steps = append(s.steps, s.commit("disabled", "Initial owned name", "r1", ""))
	switch scenario {
	case "replay":
		s.steps = append(s.steps,
			s.commit("disabled", "First observed name", "r2", ""),
			s.commit("disabled", "Later current name", "r3", ""),
			s.retry(1), s.retry(1), s.retry(1), s.retry(1),
			fail(server.ErrReplayConflict), fail(server.ErrInsufficientScope), fail(server.ErrUnsupportedProfile))
	case "precondition":
		s.steps = append(s.steps, s.commit("active", "Initial owned name", "r2", ""), s.reject("revision_conflict"))
	case "restart":
		s.steps = append(s.steps, s.commit("active", "Initial owned name", "r2", ""), s.retry(0))
	case "compound":
		s.steps = append(s.steps,
			s.commit("active", "Initial owned name", "r2", ""), s.reject("mutation_rejected"),
			s.commit("disabled", "Atomic disabled name", "r3", "disabled"),
			s.commit("active", "Atomic disabled name", "r4", ""), s.retry(3))
	}
	return s
}

func (s *script) commit(lifecycle, name, revision, effect string) step {
	return func(m server.Mutation) (*server.Result, error) {
		s.current = server.Subject{ID: "subject", Revision: revision, Lifecycle: lifecycle, DisplayName: name,
			Attributes: map[string]server.ScalarFact{"displayName": {Value: &name, Authority: "authority", Revision: revision}}}
		return &server.Result{Subject: s.current, Lifecycle: effect, Token: "token-" + revision,
			Time: 1000, RetainedUntil: 87400, Action: m.Action, CommandID: m.CommandID}, nil
	}
}

func (s *script) reject(code string) step {
	return func(m server.Mutation) (*server.Result, error) {
		return &server.Result{Error: code, Action: m.Action, CommandID: m.CommandID, Time: 1000, RetainedUntil: 87400}, nil
	}
}

func (s *script) retry(index int) step {
	return func(server.Mutation) (*server.Result, error) { return copyResult(s.results[index]), nil }
}

func fail(err error) step {
	return func(server.Mutation) (*server.Result, error) { return nil, err }
}

func (s *script) partialFailure(index int) {
	original := s.steps[index]
	s.steps[index] = func(m server.Mutation) (*server.Result, error) {
		s.current.Lifecycle = "disabled"
		return original(m)
	}
}

func (s *script) Window(context.Context, string) (string, int64, error) {
	return "window", time.Now().Unix() + 300, nil
}

func (s *script) Apply(_ context.Context, m server.Mutation) (*server.Result, error) {
	if s.next >= len(s.steps) {
		return nil, fmt.Errorf("unexpected Apply call %d", s.next)
	}
	step := s.steps[s.next]
	s.next++
	result, err := step(m)
	s.results = append(s.results, result)
	if result != nil {
		if _, found := s.retained[m.ID]; !found {
			s.retained[m.ID] = copyResult(result)
		}
	}
	return result, err
}

func (s *script) Read(context.Context, string, []string) (*server.Subject, string, error) {
	return &s.current, "frontier", nil
}

func (s *script) Result(_ context.Context, _, _, id string) (*server.Result, error) {
	if result, found := s.retained[id]; found {
		return copyResult(result), nil
	}
	return nil, server.ErrOperationResultUnavailable
}

func (s *script) BeginAudit(ctx context.Context, _, _, _ string) (context.Context, error) {
	s.begun++
	return ctx, nil
}

func (*script) IdentifyAudit(context.Context, string, string) error { return nil }

func (s *script) FinishAudit(_ context.Context, outcome server.AuditOutcome) error {
	s.finished++
	s.outcome = outcome
	return nil
}
