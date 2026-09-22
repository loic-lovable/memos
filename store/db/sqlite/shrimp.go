package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/pkg/errors"
	modernsqlite "modernc.org/sqlite"

	"github.com/usememos/memos/internal/random"
	"github.com/usememos/memos/store"
)

var errShrimpConflict = errors.New("mutation_rejected")
var errShrimpDependency = errors.New("invalid_dependency")

const shrimpColumns = "id, user_id, source_id, source_revision, source_reference, revision, lifecycle, display_name, attributes"

// ShrimpEnrolled reports whether this database requires SHRIMP enforcement.
func (d *DB) ShrimpEnrolled(ctx context.Context) (bool, error) {
	var count int
	err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM shrimp_deployment").Scan(&count)
	return count > 0, err
}

func readShrimpSubject(ctx context.Context, q rowQuerier, id string) (*store.ShrimpSubject, error) {
	s := &store.ShrimpSubject{}
	var attributes string
	err := q.QueryRowContext(ctx, "SELECT "+shrimpColumns+" FROM shrimp_subject WHERE id=? OR source_id=?", id, id).
		Scan(&s.ID, &s.UserID, &s.SourceID, &s.SourceRevision, &s.SourceReference, &s.Revision, &s.Lifecycle, &s.DisplayName, &attributes)
	if err == nil {
		err = json.Unmarshal([]byte(attributes), &s.Attributes)
	}
	return s, err
}

// ConfigureShrimp binds the database history to exactly one enrolled resource.
func (d *DB) ConfigureShrimp(ctx context.Context, resource string) error {
	if _, err := d.db.ExecContext(ctx, "INSERT OR IGNORE INTO shrimp_deployment(id,resource) VALUES(1,?)", resource); err != nil {
		return err
	}
	var previous string
	if err := d.db.QueryRowContext(ctx, "SELECT resource FROM shrimp_deployment WHERE id=1").Scan(&previous); err != nil {
		return err
	}
	if previous != resource {
		return errors.New("pilot database belongs to another resource")
	}
	if err := d.configureShrimpEnumeration(ctx); err != nil {
		return err
	}
	return d.configureShrimpAudit(ctx)
}

// ShrimpWindow persists the executable window before returning it to a client.
func (d *DB) ShrimpWindow(ctx context.Context, principal string) (string, int64, error) {
	now := time.Now().Unix()
	id, closes := random.UUID(), now+300
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, err
	}
	defer tx.Rollback()
	if err = collectShrimpHistory(ctx, tx, now); err != nil {
		return "", 0, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM shrimp_window WHERE principal=?", principal).Scan(&count); err != nil {
		return "", 0, err
	}
	if count >= 128 {
		return "", 0, store.ErrShrimpWindowQuota
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO shrimp_window(id,principal,closes_at) VALUES(?,?,?)", id, principal, closes); err != nil {
		return "", 0, err
	}
	return id, closes, tx.Commit()
}

// ShrimpResult recovers immutable history independently of current account state.
func (d *DB) ShrimpResult(ctx context.Context, principal, window, id string) (*store.ShrimpResult, error) {
	var result string
	err := d.db.QueryRowContext(ctx, "SELECT result FROM shrimp_operation WHERE principal=? AND window_id=? AND id=?", principal, window, id).Scan(&result)
	if err != nil {
		return nil, err
	}
	return store.DecodeShrimpResult(result)
}

func shrimpDependencies(ctx context.Context, q rowQuerier, dependencies []string) error {
	for _, token := range dependencies {
		var exists int
		if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM shrimp_event WHERE id=?", token).Scan(&exists); err != nil {
			return err
		}
		if exists != 1 {
			return errShrimpDependency
		}
	}
	return nil
}

func shrimpEvent(ctx context.Context, tx *sql.Tx, s *store.ShrimpSubject, actor, action string) (string, error) {
	token := random.UUID()
	_, err := tx.ExecContext(ctx, `INSERT INTO shrimp_event(id,sequence,subject_id,revision,actor,action,created_at)
		SELECT ?,COALESCE(MAX(sequence),0)+1,?,?,?,?,? FROM shrimp_event`, token, s.ID, s.Revision, actor, action, time.Now().Unix())
	return token, err
}

// ApplyShrimp commits account state, source association, evidence and retry result
// in one IMMEDIATE transaction. A retained retry never re-evaluates preconditions.
func (d *DB) ApplyShrimp(ctx context.Context, m store.ShrimpMutation) (*store.ShrimpResult, error) {
	// Commit closure independently: a rejected mutation must not resurrect an
	// expired execution window if the wall clock subsequently moves backwards.
	if err := d.collectShrimpHistory(ctx, time.Now().Unix()); err != nil {
		return nil, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var oldHash, oldResult string
	err = tx.QueryRowContext(ctx, "SELECT fingerprint,result FROM shrimp_operation WHERE principal=? AND window_id=? AND id=?", m.Principal, m.Window, m.ID).Scan(&oldHash, &oldResult)
	if err == nil {
		if oldHash != m.Fingerprint {
			return nil, errors.New("replay_conflict")
		}
		result, err := store.DecodeShrimpResult(oldResult)
		if err != nil {
			return nil, err
		}
		if err := journalShrimpOutcome(ctx, tx, m, result, true); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if m.UnsupportedProfiles {
		return nil, errors.New("unsupported_profile")
	}
	if m.RecoverOnly {
		return nil, errors.New("insufficient_scope")
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM shrimp_operation").Scan(&count); err != nil {
		return nil, err
	}
	if count >= 10000 {
		return nil, store.ErrShrimpHistoryCapacity
	}
	var closes int64
	if err := tx.QueryRowContext(ctx, "SELECT closes_at FROM shrimp_window WHERE id=? AND principal=?", m.Window, m.Principal).Scan(&closes); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("operation_result_unavailable")
		}
		return nil, err
	}
	now := time.Now().Unix()
	if now >= closes {
		return nil, errors.New("operation_result_unavailable")
	}
	result := &store.ShrimpResult{Time: now, RetainedUntil: max(closes, now) + 86400, Action: m.Action, CommandID: m.CommandID}
	if m.Deadline <= now || m.Deadline > closes {
		result.Error = "execution_deadline_expired"
	}
	if result.Error == "" {
		if err := shrimpDependencies(ctx, tx, m.Dependencies); err != nil {
			if !errors.Is(err, errShrimpDependency) {
				return nil, err
			}
			result.Error = "invalid_dependency"
		}
	}
	if result.Error == "" {
		// A savepoint preserves the terminal rejection while rolling back any
		// partially prepared business writes (including uniqueness failures).
		if _, err := tx.ExecContext(ctx, "SAVEPOINT business"); err != nil {
			return nil, err
		}
		subject, err := applyShrimpAccount(ctx, tx, m)
		if err != nil {
			var sqliteErr *modernsqlite.Error
			expected := errors.Is(err, errShrimpConflict) || errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrLastSpaceAdmin) ||
				(errors.As(err, &sqliteErr) && sqliteErr.Code()&255 == 19)
			if !expected {
				return nil, err
			}
			if _, rollbackErr := tx.ExecContext(ctx, "ROLLBACK TO business"); rollbackErr != nil {
				return nil, rollbackErr
			}
			result.Error = "mutation_rejected"
		} else {
			result.Subject = *subject
			result.Token, err = shrimpEvent(ctx, tx, subject, m.Principal, m.Action)
			if err != nil {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, "RELEASE business"); err != nil {
			return nil, err
		}
	}
	// Recheck after preparation, before the journal and business state commit.
	if result.Error == "" && time.Now().Unix() >= m.Deadline {
		return nil, errors.New("execution_deadline_expired")
	}
	result.Time = time.Now().Unix()
	result.RetainedUntil = max(closes, result.Time) + 86400
	if attempt := store.ShrimpAuditFromContext(ctx); attempt != nil {
		result.AuditAttempt = attempt.AttemptID
	}
	if err := journalShrimpOutcome(ctx, tx, m, result, false); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO shrimp_operation(principal,window_id,id,fingerprint,result,retained_until) VALUES(?,?,?,?,?,?)", m.Principal, m.Window, m.ID, m.Fingerprint, string(encoded), result.RetainedUntil); err != nil {
		return nil, err
	}
	store.ShrimpCheckpoint(ctx, "before_commit")
	if result.Error == "" && time.Now().Unix() >= m.Deadline {
		return nil, errors.New("execution_deadline_expired")
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	store.ShrimpCheckpoint(ctx, "after_commit")
	return result, nil
}

func applyShrimpAccount(ctx context.Context, tx *sql.Tx, m store.ShrimpMutation) (*store.ShrimpSubject, error) {
	if m.Action == "create_subject" {
		id, source, revision := random.UUID(), random.UUID(), random.UUID()
		hash := sha256.Sum256([]byte(m.SourceReference))
		s := &store.ShrimpSubject{ID: id, SourceID: source, SourceRevision: revision, SourceReference: m.SourceReference, Revision: revision, Lifecycle: "disabled", DisplayName: m.DisplayName}
		if err := applyShrimpAttributes(s, m, revision); err != nil {
			return nil, err
		}
		attributes, err := json.Marshal(s.Attributes)
		if err != nil {
			return nil, err
		}
		// The random username is an explicit mapping for this fresh installation.
		// No existing account is discovered or adopted from a matching attribute.
		if err := tx.QueryRowContext(ctx, "INSERT INTO user(username,role,nickname,password_hash,row_status) VALUES(?,'USER',?,'','ARCHIVED') RETURNING id", "p"+strings.ReplaceAll(id, "-", ""), s.DisplayName).Scan(&s.UserID); err != nil {
			return nil, err
		}
		if m.SSOProvider != "" {
			// The trusted deployment maps the opaque source reference to this
			// provider's stable subject. A collision rolls back the new account;
			// it never adopts an existing user by email or username.
			if err := insertUserIdentity(ctx, tx, &store.UserIdentity{UserID: s.UserID, Provider: m.SSOProvider, ExternUID: m.SourceReference}); err != nil {
				return nil, err
			}
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO shrimp_subject(id,user_id,source_id,source_revision,source_reference,source_key,revision,lifecycle,display_name,attributes) VALUES(?,?,?,?,?,?,?,?,?,?)", id, s.UserID, source, revision, m.SourceReference, hex.EncodeToString(hash[:]), revision, s.Lifecycle, s.DisplayName, string(attributes))
		return s, err
	}
	s, err := readShrimpSubject(ctx, tx, m.SubjectID)
	if err != nil {
		return nil, err
	}
	if s.ID != m.SubjectID || s.Revision != m.ExpectedRevision || s.Lifecycle == "retired" {
		return nil, errShrimpConflict
	}
	revision := random.UUID()
	if err := applyShrimpAttributes(s, m, revision); err != nil {
		return nil, err
	}
	update := &store.UpdateUser{ID: s.UserID}
	switch m.Action {
	case "activate":
		if s.Lifecycle != "disabled" {
			return nil, errShrimpConflict
		}
		s.Lifecycle = "active"
		state := store.Normal
		update.RowStatus = &state
	case "disable":
		if s.Lifecycle != "active" && s.Lifecycle != "disabled" {
			return nil, errShrimpConflict
		}
		s.Lifecycle = "disabled"
		state := store.Archived
		update.RowStatus = &state
	case "retire":
		if s.Lifecycle != "active" && s.Lifecycle != "disabled" {
			return nil, errShrimpConflict
		}
		s.Lifecycle = "retired"
		state := store.Archived
		update.RowStatus = &state
	case "update_subject":
		if _, set := m.Set["displayName"]; set || m.Set == nil || slices.Contains(m.Clear, "displayName") {
			update.Nickname = &s.DisplayName
		}
	default:
		return nil, errShrimpConflict
	}
	if update.RowStatus != nil || update.Nickname != nil {
		if _, err := updateUserTx(ctx, tx, update); err != nil {
			return nil, err
		}
	}
	s.Revision = revision
	attributes, err := json.Marshal(s.Attributes)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, "UPDATE shrimp_subject SET revision=?,lifecycle=?,display_name=?,attributes=? WHERE id=?", s.Revision, s.Lifecycle, s.DisplayName, string(attributes), s.ID)
	return s, err
}

// ReadShrimp observes the account and complete frontier in one database snapshot.
func (d *DB) ReadShrimp(ctx context.Context, id string, dependencies []string) (*store.ShrimpSubject, string, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = tx.Rollback() }()
	if err := shrimpDependencies(ctx, tx, dependencies); err != nil {
		return nil, "", err
	}
	s, err := readShrimpSubject(ctx, tx, id)
	if err != nil {
		return nil, "", err
	}
	var frontier string
	err = tx.QueryRowContext(ctx, "SELECT id FROM shrimp_event ORDER BY sequence DESC LIMIT 1").Scan(&frontier)
	return s, frontier, err
}

// ShrimpAdmission bypasses both Memos caches and reads state with its attempt fence.
func (d *DB) ShrimpAdmission(ctx context.Context, id int32) (*store.User, string, error) {
	u := &store.User{ID: id}
	var revision string
	err := d.db.QueryRowContext(ctx, `SELECT u.username,u.role,u.row_status,COALESCE(s.revision,'')
		FROM user u LEFT JOIN shrimp_subject s ON s.user_id=u.id WHERE u.id=?`, id).Scan(&u.Username, &u.Role, &u.RowStatus, &revision)
	return u, revision, err
}

func (d *DB) ShrimpPasswordAdmission(ctx context.Context, username string) (*store.User, string, error) {
	u := &store.User{}
	var revision string
	var email sql.NullString
	err := d.db.QueryRowContext(ctx, `SELECT u.id,u.username,u.role,u.row_status,u.password_hash,
		u.nickname,u.email,u.avatar_url,u.description,u.created_ts,u.updated_ts,COALESCE(s.revision,'')
		FROM user u LEFT JOIN shrimp_subject s ON s.user_id=u.id WHERE u.username=?`, username).
		Scan(&u.ID, &u.Username, &u.Role, &u.RowStatus, &u.PasswordHash, &u.Nickname, &email,
			&u.AvatarURL, &u.Description, &u.CreatedTs, &u.UpdatedTs, &revision)
	u.Email = email.String
	return u, revision, err
}

func trackShrimpNativeUpdate(ctx context.Context, tx *sql.Tx, update *store.UpdateUser) error {
	var id string
	err := tx.QueryRowContext(ctx, "SELECT id FROM shrimp_subject WHERE user_id=?", update.ID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// These facts are owned by the enrolled provisioning authority. Native
	// password/profile edits remain available and advance the same revision.
	if update.RowStatus != nil || update.Nickname != nil {
		return store.ErrShrimpManagedWrite
	}
	s, err := readShrimpSubject(ctx, tx, id)
	if err != nil {
		return err
	}
	s.Revision = random.UUID()
	if _, err := tx.ExecContext(ctx, "UPDATE shrimp_subject SET revision=? WHERE id=?", s.Revision, id); err != nil {
		return err
	}
	_, err = shrimpEvent(ctx, tx, s, "memos-native", "update_native_account")
	return err
}

func trackShrimpNativeDelete(ctx context.Context, tx *sql.Tx, userID int32) error {
	var state string
	err := tx.QueryRowContext(ctx, "SELECT lifecycle FROM shrimp_subject WHERE user_id=?", userID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if state != "retired" {
		return store.ErrShrimpManagedWrite
	}
	return nil
}

// ConsumeShrimpProof makes proof replay rejection survive process restarts.
func (d *DB) ConsumeShrimpProof(ctx context.Context, id string, expires int64) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM shrimp_proof WHERE expires_at < ?", time.Now().Unix()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO shrimp_proof(id,expires_at) VALUES(?,?)", id, expires); err != nil {
		var conflict *modernsqlite.Error
		if errors.As(err, &conflict) && conflict.Code()&255 == 19 {
			return store.ErrShrimpProofReplay
		}
		return err
	}
	return tx.Commit()
}
