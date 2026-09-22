package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"unicode/utf8"

	"github.com/lovablelabs/shrimp-protocol/sdk/go/profiles/humanattributes"
)

var humanAttributeProfiles = []string{"baseline", "human", humanattributes.ID}

// HumanWithAttributes prepares a selected-profile human. Like Human, this
// convenience API requires a stable source reference and creates disabled state.
type HumanWithAttributes struct {
	Authority       string
	SourceReference string
	Attributes      humanattributes.Values
}

// HumanAttributeLimits returns the selected peer's limits and pinned catalog
// version. Callers must obtain and verify that catalog before constructing a
// humanattributes.Profile. The server remains responsible for commit-time checks.
func (c *Client) HumanAttributeLimits() (maxEmails int, tzdbVersion string, err error) {
	if c.profile.Version != "0.2" || c.selected == nil {
		return 0, "", unavailable("human attributes require 0.2 discovery")
	}
	for _, profile := range humanAttributeProfiles {
		if !slices.Contains(c.selected.Profiles, profile) {
			return 0, "", unavailable("human attribute profile dependencies are not advertised")
		}
	}
	metadata := c.selected.HumanAttributes
	if metadata == nil || metadata.MaxEmails < 1 || metadata.MaxEmails > 16 || metadata.TZDBVersion == "" {
		return 0, "", unavailable("human attribute metadata is unavailable")
	}
	return metadata.MaxEmails, metadata.TZDBVersion, nil
}

func (c *Client) humanAttributeProfile(p *humanattributes.Profile) error {
	maximum, version, err := c.HumanAttributeLimits()
	if err != nil {
		return err
	}
	if err := p.Validate(humanattributes.Values{}); err != nil {
		return err
	}
	if p.MaxEmails() != maximum || p.TZDBVersion() != version {
		return unavailable("human attribute validator differs from selected metadata")
	}
	return nil
}

func humanToken(value string) bool {
	return value != "" && utf8.ValidString(value) && utf8.RuneCountInString(value) <= 1024
}

func humanVersion(s SubjectVersion) bool {
	return humanToken(s.ID) && humanToken(s.Revision) && humanToken(s.Authority)
}

// PrepareHumanCreate validates typed attributes against the selected peer's
// profile and prepares one disabled creation. It does not submit a mutation.
func (c *Client) PrepareHumanCreate(ctx context.Context, p *humanattributes.Profile, h HumanWithAttributes) (Intent, error) {
	if err := c.humanAttributeProfile(p); err != nil {
		return Intent{}, err
	}
	if !humanToken(h.Authority) || !humanToken(h.SourceReference) {
		return Intent{}, errors.New("authority and stable source reference are required")
	}
	if err := p.Validate(h.Attributes); err != nil {
		return Intent{}, err
	}
	if h.Attributes.Emails != nil {
		for _, email := range *h.Attributes.Emails {
			if email.ExpectedGeneration != nil {
				return Intent{}, errors.New("new human email entries require absent generations")
			}
		}
	}
	return c.prepareProfiles(ctx, h.Authority, map[string]any{
		"command_id": "c1", "action": "create_subject", "if_absent": true,
		"profile": "human", "attribute_profile": humanattributes.ID,
		"attributes": h.Attributes, "expires_at": nil, "source_reference": h.SourceReference,
	}, humanAttributeProfiles)
}

// PrepareHumanUpdate prepares a complete-field update under the caller's exact
// observed subject revision. The target checks current field ownership and email
// generations; a conflict never causes this client to refresh or replace intent.
func (c *Client) PrepareHumanUpdate(ctx context.Context, p *humanattributes.Profile, s SubjectVersion, changes humanattributes.Changes) (Intent, error) {
	if err := c.humanAttributeProfile(p); err != nil {
		return Intent{}, err
	}
	if !humanVersion(s) {
		return Intent{}, errors.New("subject ID, revision and authority are required")
	}
	if changes.Clear == nil {
		changes.Clear = []humanattributes.Field{}
	}
	// Validate strings before JSON encoding can replace invalid Unicode.
	if err := p.Validate(changes.Set); err != nil {
		return Intent{}, err
	}
	raw, err := json.Marshal(changes)
	if err != nil {
		return Intent{}, err
	}
	if _, err := p.DecodeChanges(raw); err != nil {
		return Intent{}, err
	}
	return c.prepareProfiles(ctx, s.Authority, map[string]any{
		"command_id": "c1", "action": "update_subject", "resource": map[string]any{"type": "subject", "id": s.ID},
		"expected_revision": s.Revision, "attribute_profile": humanattributes.ID,
		"set": changes.Set, "clear": changes.Clear,
	}, humanAttributeProfiles)
}

// PendingHumanMigration is a fixed, enrollment-bound proposal awaiting a trusted
// administrative authorization handle. Persist Bytes before requesting approval.
// It cannot be submitted or restored as an ordinary Intent. A zero value is invalid.
type PendingHumanMigration struct {
	raw     string
	request string
}

// Bytes returns the full proposal, including enrollment and original window
// evidence, for protected caller-owned persistence. It includes personal data.
func (p PendingHumanMigration) Bytes() []byte { return []byte(p.raw) }

// Request returns canonical request JSON with authorization omitted. Approval
// must additionally bind the enrollment and affected facts at the expected
// subject revision. These bytes are not a submit-ready wire request.
func (p PendingHumanMigration) Request() []byte { return []byte(p.request) }

// Fingerprint is the SHA-256 of Request, excluding the authorization handle.
// This is an approval binding aid, not an authorization proof or operation ID.
// The final operation fingerprint includes the attached handle.
func (p PendingHumanMigration) Fingerprint() string {
	sum := sha256.Sum256([]byte(p.request))
	return hex.EncodeToString(sum[:])
}

const migrationPlaceholder = "pending-administrative-authorization"

// PrepareHumanMigration fixes the operation, window, deadline, subject revision
// and requested entry ID before administrative approval. It performs no mutation
// or approval. Nil entryID is required when the current scalar email is absent or
// cleared; the application validates that condition against current state.
func (c *Client) PrepareHumanMigration(ctx context.Context, s SubjectVersion, entryID *string) (PendingHumanMigration, error) {
	if _, _, err := c.HumanAttributeLimits(); err != nil {
		return PendingHumanMigration{}, err
	}
	if !humanVersion(s) || (entryID != nil && !identifier.MatchString(*entryID)) {
		return PendingHumanMigration{}, errors.New("invalid migration subject or email entry ID")
	}
	// A private placeholder permits reuse of exact schema/window validation.
	// It is removed before exposing the separate, non-submittable proposal.
	intent, err := c.prepareProfiles(ctx, s.Authority, map[string]any{
		"command_id": "c1", "action": "migrate_human_attributes", "resource": map[string]any{"type": "subject", "id": s.ID},
		"expected_revision": s.Revision, "email_entry_id": entryID, "authorization": migrationPlaceholder,
	}, humanAttributeProfiles)
	if err != nil {
		return PendingHumanMigration{}, err
	}
	value, err := decodeJSON(intent.Bytes())
	if err != nil {
		return PendingHumanMigration{}, err
	}
	entry := value.(map[string]any)
	request := entry["request"].(map[string]any)
	command := request["commands"].([]any)[0].(map[string]any)
	delete(command, "authorization")
	entry["migration_request"] = request
	delete(entry, "request")
	raw, err := json.Marshal(entry)
	if err != nil {
		return PendingHumanMigration{}, err
	}
	return c.RestoreHumanMigration(raw)
}

// RestoreHumanMigration validates a saved proposal under the original enrollment
// without new-work discovery, a new window or a changed deadline. Protect saved
// bytes against tampering; this is structural validation, not signature checking.
func (c *Client) RestoreHumanMigration(raw []byte) (PendingHumanMigration, error) {
	value, err := decodeJSON(raw)
	if err != nil {
		return PendingHumanMigration{}, err
	}
	entry, ok := value.(map[string]any)
	if !ok {
		return PendingHumanMigration{}, errors.New("invalid migration proposal")
	}
	request, ok := entry["migration_request"].(map[string]any)
	if !ok || entry["request"] != nil || len(entry) != 8 {
		return PendingHumanMigration{}, errors.New("invalid migration proposal envelope")
	}
	commands, ok := request["commands"].([]any)
	if !ok || len(commands) != 1 {
		return PendingHumanMigration{}, errors.New("invalid migration proposal command")
	}
	command, ok := commands[0].(map[string]any)
	if !ok || command["action"] != "migrate_human_attributes" {
		return PendingHumanMigration{}, errors.New("invalid migration proposal action")
	}
	if _, exists := command["authorization"]; exists {
		return PendingHumanMigration{}, errors.New("migration proposal already has authorization")
	}
	requestRaw, err := json.Marshal(request)
	if err != nil {
		return PendingHumanMigration{}, err
	}
	command["authorization"] = migrationPlaceholder
	entry["request"] = request
	delete(entry, "migration_request")
	validationRaw, err := json.Marshal(entry)
	if err != nil {
		return PendingHumanMigration{}, err
	}
	if _, err := c.RestoreIntent(validationRaw); err != nil {
		return PendingHumanMigration{}, err
	}
	return PendingHumanMigration{raw: string(raw), request: string(requestRaw)}, nil
}

// AuthorizeHumanMigration attaches a separately issued administrative handle;
// it does not issue or validate the grant. It changes no approved operation,
// revision, entry ID or deadline and makes no network requests. Persist the final
// Intent before submission. Approval withdrawal and stale state remain target checks.
func (c *Client) AuthorizeHumanMigration(pending PendingHumanMigration, authorization string) (Intent, error) {
	if !humanToken(authorization) {
		return Intent{}, errors.New("administrative authorization handle is required")
	}
	verified, err := c.RestoreHumanMigration(pending.Bytes())
	if err != nil {
		return Intent{}, err
	}
	value, err := decodeJSON(verified.Bytes())
	if err != nil {
		return Intent{}, err
	}
	entry := value.(map[string]any)
	request := entry["migration_request"].(map[string]any)
	command := request["commands"].([]any)[0].(map[string]any)
	command["authorization"] = authorization
	entry["request"] = request
	delete(entry, "migration_request")
	raw, err := json.Marshal(entry)
	if err != nil {
		return Intent{}, err
	}
	return c.RestoreIntent(raw)
}
