package sqlite

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/usememos/memos/internal/random"
	"github.com/usememos/memos/store"
)

// The signing key survives process restart. Do not replace a missing key while
// promised traversal state exists: that would turn storage loss into invalid cursors.
func (d *DB) configureShrimpEnumeration(ctx context.Context) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM shrimp_enumeration_key WHERE id=1").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM shrimp_enumeration").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return errors.New("missing SHRIMP enumeration signing key")
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO shrimp_enumeration_key(id,secret) VALUES(1,?)", hex.EncodeToString(key)); err != nil {
			return err
		}
	}
	var encodedKey string
	if err := tx.QueryRowContext(ctx, "SELECT secret FROM shrimp_enumeration_key WHERE id=1").Scan(&encodedKey); err != nil {
		return err
	}
	key, err := hex.DecodeString(encodedKey)
	if err != nil || len(key) != 32 {
		return errors.New("invalid enumeration signing key")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("SHRIMP pilot genesis frontier"))
	genesis := "genesis-" + hex.EncodeToString(mac.Sum(nil))
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO shrimp_event(id,sequence,subject_id,revision,actor,action,created_at)
		VALUES(?,0,'','','','genesis',?)`, genesis, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

type shrimpCursor struct {
	Traversal string `json:"t"`
	Expires   int64  `json:"e"`
	Type      string `json:"k"`
	ID        string `json:"i"`
	Sequence  int64  `json:"s"`
	Frontier  string `json:"f"`
}

func encodeShrimpCursor(key []byte, cursor shrimpCursor) (string, error) {
	body, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func decodeShrimpCursor(key []byte, value string) (shrimpCursor, error) {
	var cursor shrimpCursor
	invalid := store.ShrimpEnumerationError("invalid_cursor")
	if len(value) > 1024 {
		return cursor, invalid
	}
	body, signature, ok := strings.Cut(value, ".")
	if !ok {
		return cursor, invalid
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(body)
	if err != nil {
		return cursor, invalid
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil {
		return cursor, invalid
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	if !hmac.Equal(sig, mac.Sum(nil)) || json.Unmarshal(data, &cursor) != nil || cursor.Traversal == "" || cursor.ID == "" ||
		(cursor.Type != "subject" && cursor.Type != "source_reference") || cursor.Sequence < 0 || cursor.Frontier == "" {
		return cursor, invalid
	}
	return cursor, nil
}

// EnumerateShrimp holds one authoritative SQLite snapshot through lookahead and
// cursor persistence. fits must only measure the complete wire page, without I/O.
func (d *DB) EnumerateShrimp(ctx context.Context, request store.ShrimpEnumeration, fits func(*store.ShrimpEnumerationPage) (bool, error)) (*store.ShrimpEnumerationPage, error) {
	if request.PageSize < 1 || request.PageSize > store.ShrimpEnumerationMaxPage || len(request.Dependencies) > 16 || fits == nil {
		return nil, store.ShrimpEnumerationError("limit_exceeded")
	}
	for _, kind := range request.Types {
		if kind != "subject" && kind != "source_reference" {
			return nil, store.ShrimpEnumerationError("unsupported_resource")
		}
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	observedAt := time.Now()
	var encodedKey string
	if err := tx.QueryRowContext(ctx, "SELECT secret FROM shrimp_enumeration_key WHERE id=1").Scan(&encodedKey); err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(encodedKey)
	if err != nil || len(key) != 32 {
		return nil, errors.New("invalid enumeration signing key")
	}
	now := time.Now().Unix()
	cursor := shrimpCursor{Traversal: random.UUID()}
	if request.Cursor != "" {
		cursor, err = decodeShrimpCursor(key, request.Cursor)
		if err != nil {
			return nil, err
		}
		if now >= cursor.Expires {
			return nil, store.ShrimpEnumerationError("cursor_expired")
		}
		var principal, scope, authorization, epoch, selection string
		var expiry int64
		err := tx.QueryRowContext(ctx, `SELECT principal,scope_context,authorization_context,history_epoch,selection_hash,expires_at
			FROM shrimp_enumeration WHERE id=?`, cursor.Traversal).Scan(&principal, &scope, &authorization, &epoch, &selection, &expiry)
		if err != nil {
			return nil, err // Missing promised state is unavailable, never normal expiry.
		}
		if principal != request.Principal || scope != request.Scope {
			return nil, store.ShrimpEnumerationError("cursor_scope_mismatch")
		}
		if epoch != request.Epoch {
			return nil, store.ShrimpEnumerationError("cursor_epoch_mismatch")
		}
		if selection != request.Selection || authorization != request.Authorization {
			return nil, store.ShrimpEnumerationError("view_changed")
		}
		if expiry != cursor.Expires {
			return nil, errors.New("inconsistent enumeration expiry")
		}
		if cursor.Sequence > 0 {
			var floor string
			err := tx.QueryRowContext(ctx, "SELECT id FROM shrimp_event WHERE sequence=?", cursor.Sequence).Scan(&floor)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && floor != cursor.Frontier) {
				return nil, store.ShrimpEnumerationError("consistency_unavailable")
			}
			if err != nil {
				return nil, err
			}
		}
	} else if err := shrimpDependencies(ctx, tx, request.Dependencies); err != nil {
		if errors.Is(err, errShrimpDependency) {
			return nil, store.ShrimpEnumerationError("invalid_dependency")
		}
		return nil, err
	}
	var frontier string
	var sequence int64
	err = tx.QueryRowContext(ctx, "SELECT id,sequence FROM shrimp_event ORDER BY sequence DESC LIMIT 1").Scan(&frontier, &sequence)
	if err != nil {
		return nil, err
	}
	if sequence < cursor.Sequence {
		return nil, store.ShrimpEnumerationError("consistency_unavailable")
	}
	records, err := enumerateShrimpRecords(ctx, tx, request, cursor)
	if err != nil {
		return nil, err
	}
	if request.Cursor == "" {
		// Round up and leave one observation-bound interval for serialization,
		// so issuance still promises at least the advertised 300 seconds.
		cursor.Expires = time.Now().Unix() + store.ShrimpEnumerationLifetime + 2
	}
	page := &store.ShrimpEnumerationPage{Records: []store.ShrimpRecord{}, Frontier: frontier}
	for n := 1; n <= min(request.PageSize, len(records)); n++ {
		candidate := &store.ShrimpEnumerationPage{Records: records[:n], Frontier: frontier}
		if n < len(records) {
			next := cursor
			next.Type, next.ID = records[n-1].Type, records[n-1].ID
			next.Sequence, next.Frontier = sequence, frontier
			candidate.Cursor, err = encodeShrimpCursor(key, next)
			if err != nil {
				return nil, err
			}
			candidate.ExpiresAt = cursor.Expires
		}
		ok, err := fits(candidate)
		if err != nil {
			return nil, err
		}
		if !ok {
			// The final envelope has no cursor and can be smaller than a prior
			// nonterminal envelope. Do not reject a fitting final page early.
			continue
		}
		page = candidate
	}
	if len(records) > 0 && len(page.Records) == 0 {
		return nil, store.ShrimpEnumerationError("limit_exceeded")
	}
	if ok, err := fits(page); err != nil {
		return nil, err
	} else if !ok {
		return nil, store.ShrimpEnumerationError("limit_exceeded")
	}
	if request.Cursor == "" && page.Cursor != "" {
		if _, err := tx.ExecContext(ctx, "DELETE FROM shrimp_enumeration WHERE expires_at<=?", now); err != nil {
			return nil, err
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM shrimp_enumeration WHERE principal=?", request.Principal).Scan(&count); err != nil {
			return nil, err
		}
		if count >= store.ShrimpEnumerationMaxOpen {
			return nil, store.ShrimpEnumerationError("throttled")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO shrimp_enumeration(id,principal,scope_context,authorization_context,history_epoch,selection_hash,expires_at)
			VALUES(?,?,?,?,?,?,?)`, cursor.Traversal, request.Principal, request.Scope, request.Authorization, request.Epoch, request.Selection, cursor.Expires); err != nil {
			return nil, err
		}
	}
	if time.Now().Unix() >= cursor.Expires {
		return nil, store.ShrimpEnumerationError("cursor_expired")
	}
	if time.Since(observedAt) > time.Second {
		return nil, store.ShrimpEnumerationError("consistency_unavailable")
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return page, nil
}

func enumerateShrimpRecords(ctx context.Context, tx *sql.Tx, request store.ShrimpEnumeration, cursor shrimpCursor) ([]store.ShrimpRecord, error) {
	rows, err := tx.QueryContext(ctx, `SELECT kind,record_id,`+shrimpColumns+` FROM (
		SELECT 'source_reference' AS kind,source_id AS record_id,`+shrimpColumns+` FROM shrimp_subject WHERE ?
		UNION ALL SELECT 'subject' AS kind,id AS record_id,`+shrimpColumns+` FROM shrimp_subject WHERE ?
		) WHERE kind COLLATE BINARY > ? OR (kind=? AND record_id COLLATE BINARY > ?)
		ORDER BY kind COLLATE BINARY,record_id COLLATE BINARY LIMIT ?`,
		request.Visible && slices.Contains(request.Types, "source_reference"), request.Visible && slices.Contains(request.Types, "subject"),
		cursor.Type, cursor.Type, cursor.ID, request.PageSize+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []store.ShrimpRecord{}
	for rows.Next() {
		var record store.ShrimpRecord
		s := &record.Subject
		var attributes string
		if err := rows.Scan(&record.Type, &record.ID, &s.ID, &s.UserID, &s.SourceID, &s.SourceRevision, &s.SourceReference, &s.Revision, &s.Lifecycle, &s.DisplayName, &attributes); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(attributes), &s.Attributes); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}
