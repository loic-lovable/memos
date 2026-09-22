package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"time"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/profiles/humanattributes"
	"github.com/lovablelabs/shrimp-protocol/sdk/go/profiles/scalar"
	"github.com/pkg/errors"

	"github.com/usememos/memos/internal/random"
	"github.com/usememos/memos/store"
)

var errShrimpMigrationForbidden = errors.New("human attribute migration forbidden")

// AuthorizeShrimpAttributeMigration installs an approval from trusted local
// administration. No provisioning route exposes this method. The administrator
// must independently verify current migration authority and each supplied owner.
// Existing handles cannot be replaced or renewed, including consumed approvals.
func (d *DB) AuthorizeShrimpAttributeMigration(ctx context.Context, approval store.ShrimpMigrationApproval) error {
	if approval.Authorization == "" || len(approval.Authorization) > 128 || approval.Principal == "" || approval.Window == "" || approval.Operation == "" || len(approval.Fingerprint) != 64 || len(approval.Owners) == 0 {
		return errShrimpMigrationForbidden
	}
	owners, err := json.Marshal(approval.Owners)
	if err != nil {
		return err
	}
	// Re-loading the exact grant does not reset consumption or expiry. A local
	// administrator revokes a grant by deleting it; expiration never renews it.
	var principal, window, operation, fingerprint, oldOwners string
	var expires int64
	err = d.db.QueryRowContext(ctx, "SELECT principal,window_id,operation_id,fingerprint,owners,expires_at FROM shrimp_attribute_approval WHERE authorization=?", approval.Authorization).Scan(&principal, &window, &operation, &fingerprint, &oldOwners, &expires)
	if err == nil {
		if principal == approval.Principal && window == approval.Window && operation == approval.Operation && fingerprint == approval.Fingerprint && oldOwners == string(owners) && expires == approval.ExpiresAt {
			return nil
		}
		return errShrimpMigrationForbidden
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if approval.ExpiresAt <= time.Now().Unix() {
		return errShrimpMigrationForbidden
	}
	_, err = d.db.ExecContext(ctx, `INSERT INTO shrimp_attribute_approval(authorization,principal,window_id,operation_id,fingerprint,owners,expires_at) VALUES(?,?,?,?,?,?,?)`, approval.Authorization, approval.Principal, approval.Window, approval.Operation, approval.Fingerprint, string(owners), approval.ExpiresAt)
	return err
}

func consumeShrimpAttributeApproval(ctx context.Context, tx *sql.Tx, s *store.ShrimpSubject, m store.ShrimpMutation) error {
	if m.Migration == nil {
		return errShrimpMigrationForbidden
	}
	var principal, window, operation, fingerprint, rawOwners string
	var expires int64
	var consumed int
	err := tx.QueryRowContext(ctx, `SELECT principal,window_id,operation_id,fingerprint,owners,expires_at,consumed FROM shrimp_attribute_approval WHERE authorization=?`, m.Migration.Authorization).Scan(&principal, &window, &operation, &fingerprint, &rawOwners, &expires, &consumed)
	if errors.Is(err, sql.ErrNoRows) {
		return errShrimpMigrationForbidden
	}
	if err != nil {
		return err
	}
	if consumed != 0 || expires <= time.Now().Unix() || principal != m.Principal || window != m.Window || operation != m.ID || fingerprint != m.Migration.ApprovalFingerprint {
		return errShrimpMigrationForbidden
	}
	var owners []string
	if err := json.Unmarshal([]byte(rawOwners), &owners); err != nil {
		return err
	}
	// Approval includes the current administrative authority even for empty facts.
	if !slices.Contains(owners, m.Authority) {
		return errShrimpMigrationForbidden
	}
	for _, fact := range s.Attributes {
		owner := fact.Authority
		if owner == "" {
			owner = m.Authority
		}
		if !slices.Contains(owners, owner) {
			return errShrimpMigrationForbidden
		}
	}
	_, err = tx.ExecContext(ctx, "UPDATE shrimp_attribute_approval SET consumed=1 WHERE authorization=?", m.Migration.Authorization)
	return err
}

// decodeShrimpHumanState accepts the canonical envelope emitted by this store.
// Equality rejects missing/unknown/duplicate fields, null, case aliases and
// replaced malformed Unicode before any stored fact can be used for admission.
// Historical semantic validation is independent of current write configuration.
func decodeShrimpHumanState(raw string) (*humanattributes.State, error) {
	var state *humanattributes.State
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return nil, err
	}
	if state == nil {
		return nil, humanattributes.ErrInvalidState
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	if string(encoded) != raw {
		return nil, humanattributes.ErrInvalidState
	}
	if err := humanattributes.ValidateState(*state); err != nil {
		return nil, err
	}
	return state, nil
}

// decodeShrimpHumanFacts keeps private history out of read and retained snapshots.
func decodeShrimpHumanFacts(s *store.ShrimpSubject, raw string) error {
	if s.AttributeProfile == "" {
		if raw != "null" {
			return humanattributes.ErrInvalidState
		}
		return nil
	}
	if s.AttributeProfile != humanattributes.ID || s.Attributes != nil {
		return humanattributes.ErrInvalidState
	}
	state, err := decodeShrimpHumanState(raw)
	if err != nil {
		return err
	}
	s.HumanAttributes = &state.Facts
	return nil
}

// prepareShrimpAttributes runs after retained lookup, inside the business
// savepoint. It returns private persisted state; public Subject holds facts only.
func prepareShrimpAttributes(ctx context.Context, tx *sql.Tx, s *store.ShrimpSubject, m store.ShrimpMutation, revision string) (*humanattributes.State, error) {
	if s.AttributeProfile == "" && m.AttributeProfile == "" && m.Action != "migrate_human_attributes" {
		return nil, applyShrimpAttributes(s, m, revision)
	}
	var current humanattributes.State
	if s.AttributeProfile != "" {
		var raw string
		if err := tx.QueryRowContext(ctx, "SELECT human_attributes FROM shrimp_subject WHERE id=?", s.ID).Scan(&raw); err != nil {
			return nil, err
		}
		state, err := decodeShrimpHumanState(raw)
		if err != nil {
			return nil, err
		}
		current = *state
		if s.AttributeProfile != humanattributes.ID {
			return nil, humanattributes.ErrInvalidState
		}
	}
	if m.Action == "activate" || m.Action == "disable" || m.Action == "retire" {
		return &current, nil
	}
	if m.HumanProfile == nil {
		return nil, store.ErrShrimpUnsupportedProfile
	}
	allocate := func() (string, error) { return random.UUID(), nil }
	var result humanattributes.Result
	var err error
	switch m.Action {
	case "create_subject":
		if m.AttributeProfile != humanattributes.ID {
			return nil, humanattributes.ErrInvalidValue
		}
		var values humanattributes.Values
		values, err = m.HumanProfile.DecodeValues(m.HumanAttributes)
		if err == nil {
			result, err = m.HumanProfile.Create(values, m.Authority, revision, allocate)
		}
	case "update_subject":
		if s.AttributeProfile != humanattributes.ID || m.AttributeProfile != humanattributes.ID {
			return nil, humanattributes.ErrInvalidValue
		}
		var changes humanattributes.Changes
		changes, err = m.HumanProfile.DecodeChanges(m.HumanAttributes)
		if err == nil {
			result, err = m.HumanProfile.Update(current, changes, m.Authority, revision, allocate)
		}
	case "migrate_human_attributes":
		if s.AttributeProfile != "" {
			return nil, humanattributes.ErrRevisionConflict
		}
		// Materialize legacy displayName at its old observable revision, resolving
		// its fixed-enrollment owner before checking administrative approval.
		if s.Attributes == nil {
			name := s.DisplayName
			s.Attributes = map[string]store.ShrimpScalarFact{"displayName": {Value: &name, Authority: m.Authority, Revision: s.Revision}}
		}
		if err := consumeShrimpAttributeApproval(ctx, tx, s, m); err != nil {
			return nil, err
		}
		legacy := humanattributes.LegacyState{Facts: map[string]scalar.Fact{}}
		for name, fact := range s.Attributes {
			owner := fact.Authority
			if owner == "" {
				owner = m.Authority
			}
			legacy.Facts[name] = scalar.Fact{Value: fact.Value, Authority: owner, Revision: fact.Revision}
		}
		result, err = m.HumanProfile.Migrate(legacy, m.Migration.EmailEntryID, revision, allocate)
	default:
		return nil, humanattributes.ErrInvalidValue
	}
	if err != nil {
		return nil, err
	}
	// This pilot implements no verification assertions. Native Memos email and
	// login identities remain separate, so no verification evidence is inferred.
	s.AttributeProfile = humanattributes.ID
	s.HumanAttributes = &result.State.Facts
	s.Attributes = nil
	s.DisplayName = ""
	if fact := result.State.Facts.DisplayName; fact != nil && fact.Value != nil {
		s.DisplayName = *fact.Value
	}
	return &result.State, nil
}

func shrimpHumanRejection(err error) string {
	switch {
	case errors.Is(err, humanattributes.ErrInvalidValue):
		return "invalid_request"
	case errors.Is(err, humanattributes.ErrLimitExceeded):
		return "limit_exceeded"
	case errors.Is(err, humanattributes.ErrRevisionConflict):
		return "revision_conflict"
	case errors.Is(err, humanattributes.ErrAuthorityConflict), errors.Is(err, errShrimpMigrationForbidden):
		return "forbidden"
	case errors.Is(err, store.ErrShrimpUnsupportedProfile):
		return "unsupported_profile"
	default:
		return ""
	}
}
