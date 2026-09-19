package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"

	"github.com/pkg/errors"
)

// ErrShrimpManagedWrite rejects native changes to source-owned lifecycle facts.
var ErrShrimpManagedWrite = errors.New("managed lifecycle and display name require SHRIMP authority; retire before native deletion")

// ErrAdmissionDenied means the current account or original attempt is fenced.
var ErrAdmissionDenied = errors.New("account admission fenced")

// ErrShrimpProofReplay identifies an already consumed proof.
var ErrShrimpProofReplay = errors.New("proof already consumed")

// ErrShrimpProofStorage identifies unavailable replay protection, not bad credentials.
var ErrShrimpProofStorage = errors.New("proof replay storage unavailable")

// ShrimpSubject is the persistent pilot identity, separate from a Memos username.
type ShrimpSubject struct {
	ID, SourceID, SourceRevision, SourceReference, Revision, Lifecycle, DisplayName string
	UserID                                                                          int32
}

// ShrimpMutation is a validated, single-account provisioning intent.
type ShrimpMutation struct {
	Principal, Window, ID, Fingerprint, Action, SubjectID, ExpectedRevision string
	SourceReference, DisplayName                                            string
	Deadline                                                                int64
	Dependencies                                                            []string
	CommandID                                                               string
	RecoverOnly                                                             bool
	UnsupportedProfiles                                                     bool
}

// ShrimpResult is immutable commit evidence retained with the account write.
type ShrimpResult struct {
	AuditAttempt        string
	Subject             ShrimpSubject
	Token               string
	Time, RetainedUntil int64
	Action              string
	Error               string
	CommandID           string
}

// ShrimpDriver is deliberately optional: only SQLite implements this pilot.
type ShrimpDriver interface {
	ConfigureShrimp(context.Context, string) error
	ShrimpWindow(context.Context, string) (string, int64, error)
	ApplyShrimp(context.Context, ShrimpMutation) (*ShrimpResult, error)
	ShrimpResult(context.Context, string, string, string) (*ShrimpResult, error)
	ReadShrimp(context.Context, string, []string) (*ShrimpSubject, string, error)
	EnumerateShrimp(context.Context, ShrimpEnumeration, func(*ShrimpEnumerationPage) (bool, error)) (*ShrimpEnumerationPage, error)
	ShrimpAdmission(context.Context, int32) (*User, string, error)
	ShrimpPasswordAdmission(context.Context, string) (*User, string, error)
	ConsumeShrimpProof(context.Context, string, int64) error
}

// EnableShrimpPilot must run before serving requests, in one process per database.
func (s *Store) EnableShrimpPilot(ctx context.Context, resource string) error {
	driver, ok := s.driver.(ShrimpDriver)
	if !ok {
		return errors.New("SHRIMP pilot requires SQLite")
	}
	if err := driver.ConfigureShrimp(ctx, resource); err != nil {
		return err
	}
	s.shrimpPilot = true
	return nil
}

// ApplyShrimp serializes the commit against native writes and credential publication.
func (s *Store) ApplyShrimp(ctx context.Context, mutation ShrimpMutation) (*ShrimpResult, error) {
	if !s.shrimpPilot {
		return nil, errors.New("SHRIMP pilot is disabled")
	}
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	result, err := s.driver.(ShrimpDriver).ApplyShrimp(ctx, mutation)
	if err == nil {
		s.userCache.Delete(ctx, userCacheKey(result.Subject.UserID))
	}
	return result, err
}

type admissionScope struct {
	mu       sync.Mutex
	releases []func()
}
type admissionScopeKey struct{}

// PasswordAdmission reads the credential and its revision from one authoritative
// snapshot. Separately fetching a ticket could bless a password reset in between.
func (s *Store) PasswordAdmission(ctx context.Context, username string) (*User, string, error) {
	if !s.shrimpPilot {
		user, err := s.GetUser(ctx, &FindUser{Username: &username})
		return user, "", err
	}
	user, revision, err := s.driver.(ShrimpDriver).ShrimpPasswordAdmission(ctx, username)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil
	}
	return user, revision, err
}

// WithAdmissionScope keeps successful issuance locked until the transport finishes
// writing its response. A service-method return is too early for this release.
func WithAdmissionScope(ctx context.Context) (context.Context, func()) {
	scope := &admissionScope{}
	return context.WithValue(ctx, admissionScopeKey{}, scope), func() {
		scope.mu.Lock()
		defer scope.mu.Unlock()
		for _, release := range scope.releases {
			release()
		}
		scope.releases = nil
	}
}

// AdmissionTicket captures the original attempt's revision before it can pause.
func (s *Store) AdmissionTicket(ctx context.Context, id int32) (string, error) {
	if !s.shrimpPilot {
		return "", nil
	}
	user, revision, err := s.driver.(ShrimpDriver).ShrimpAdmission(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrAdmissionDenied
	}
	if err != nil {
		return "", err
	}
	if user == nil || user.RowStatus != Normal {
		return "", ErrAdmissionDenied
	}
	return revision, nil
}

// PublishAdmission rechecks authoritative state, never the user cache, and keeps
// disable from completing until the response carrying the credential is written.
func (s *Store) PublishAdmission(ctx context.Context, id int32, ticket string, publish func(*User) error) error {
	if !s.shrimpPilot {
		user, err := s.GetUser(ctx, &FindUser{ID: &id})
		if err != nil {
			return err
		}
		if user == nil {
			return ErrAdmissionDenied
		}
		return publish(user)
	}
	scope, ok := ctx.Value(admissionScopeKey{}).(*admissionScope)
	if !ok {
		return errors.New("credential publication requires a transport admission scope")
	}
	s.admissionMu.RLock()
	user, revision, err := s.driver.(ShrimpDriver).ShrimpAdmission(ctx, id)
	if err != nil || user == nil || user.RowStatus != Normal || revision != ticket {
		s.admissionMu.RUnlock()
		if err != nil {
			return err
		}
		return ErrAdmissionDenied
	}
	if err := publish(user); err != nil {
		s.admissionMu.RUnlock()
		return err
	}
	scope.mu.Lock()
	scope.releases = append(scope.releases, s.admissionMu.RUnlock)
	scope.mu.Unlock()
	return nil
}

// DecodeShrimpResult reads a retained immutable result without reconstructing it
// from current account state, which may already have changed again.
func DecodeShrimpResult(value string) (*ShrimpResult, error) {
	var result ShrimpResult
	if err := json.Unmarshal([]byte(value), &result); err != nil {
		return nil, errors.Wrap(err, "decode SHRIMP result")
	}
	return &result, nil
}

type shrimpFaultKey struct{}

// WithShrimpFault installs an isolated-test commit boundary, never a wire input.
func WithShrimpFault(ctx context.Context, fault func(string)) context.Context {
	return context.WithValue(ctx, shrimpFaultKey{}, fault)
}

// ShrimpCheckpoint invokes a trusted test hook when one is installed.
func ShrimpCheckpoint(ctx context.Context, point string) {
	if fault, ok := ctx.Value(shrimpFaultKey{}).(func(string)); ok {
		fault(point)
	}
}
