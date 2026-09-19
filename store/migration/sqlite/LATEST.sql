-- system_setting
CREATE TABLE system_setting (
  name TEXT NOT NULL,
  value TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  UNIQUE(name)
);

-- user
CREATE TABLE user (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  created_ts BIGINT NOT NULL DEFAULT (strftime('%s', 'now')),
  updated_ts BIGINT NOT NULL DEFAULT (strftime('%s', 'now')),
  row_status TEXT NOT NULL CHECK (row_status IN ('NORMAL', 'ARCHIVED')) DEFAULT 'NORMAL',
  username TEXT COLLATE BINARY NOT NULL UNIQUE,
  role TEXT NOT NULL DEFAULT 'USER',
  email TEXT COLLATE BINARY DEFAULT NULL,
  nickname TEXT NOT NULL DEFAULT '',
  password_hash TEXT NOT NULL,
  avatar_url TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT ''
);

CREATE UNIQUE INDEX idx_user_email ON user(email);

-- user_setting
CREATE TABLE user_setting (
  user_id INTEGER NOT NULL,
  key TEXT NOT NULL,
  value TEXT NOT NULL,
  UNIQUE(user_id, key)
);

-- space
CREATE TABLE space (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  uid TEXT NOT NULL UNIQUE,
  title TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  payload TEXT NOT NULL DEFAULT '{}'
);

-- space membership
CREATE TABLE space_member (
  space_id INTEGER NOT NULL,
  user_id INTEGER NOT NULL,
  status TEXT NOT NULL,
  role TEXT NOT NULL CHECK (role IN ('ADMIN', 'USER')),
  PRIMARY KEY (space_id, user_id)
);

CREATE INDEX idx_space_member_user_id ON space_member(user_id, space_id);

-- memo
CREATE TABLE memo (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  uid TEXT NOT NULL UNIQUE,
  creator_id INTEGER NOT NULL,
  created_ts BIGINT NOT NULL DEFAULT (strftime('%s', 'now')),
  updated_ts BIGINT NOT NULL DEFAULT (strftime('%s', 'now')),
  row_status TEXT NOT NULL CHECK (row_status IN ('NORMAL', 'ARCHIVED')) DEFAULT 'NORMAL',
  content TEXT NOT NULL DEFAULT '',
  visibility TEXT NOT NULL CHECK (visibility IN ('PUBLIC', 'PROTECTED', 'PRIVATE', 'SPACE')) DEFAULT 'PRIVATE',
  pinned INTEGER NOT NULL CHECK (pinned IN (0, 1)) DEFAULT 0,
  payload TEXT NOT NULL DEFAULT '{}',
  space_id INTEGER DEFAULT NULL
);

CREATE INDEX idx_memo_creator_id ON memo(creator_id);
CREATE INDEX idx_memo_space_id ON memo(space_id, row_status, created_ts DESC, id DESC);

-- memo_relation
CREATE TABLE memo_relation (
  memo_id INTEGER NOT NULL,
  related_memo_id INTEGER NOT NULL,
  type TEXT NOT NULL,
  UNIQUE(memo_id, related_memo_id, type)
);

CREATE INDEX idx_memo_relation_related_type_memo
  ON memo_relation(related_memo_id, type, memo_id);

-- attachment
CREATE TABLE attachment (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  uid TEXT NOT NULL UNIQUE,
  creator_id INTEGER NOT NULL,
  created_ts BIGINT NOT NULL DEFAULT (strftime('%s', 'now')),
  updated_ts BIGINT NOT NULL DEFAULT (strftime('%s', 'now')),
  filename TEXT NOT NULL DEFAULT '',
  blob BLOB DEFAULT NULL,
  type TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL DEFAULT 0,
  memo_id INTEGER,
  storage_type TEXT NOT NULL DEFAULT '',
  reference TEXT NOT NULL DEFAULT '',
  payload TEXT NOT NULL DEFAULT '{}'
);

-- idp
CREATE TABLE idp (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  uid TEXT NOT NULL UNIQUE,
  name TEXT NOT NULL,
  type TEXT NOT NULL,
  identifier_filter TEXT NOT NULL DEFAULT '',
  config TEXT NOT NULL DEFAULT '{}'
);

-- inbox
CREATE TABLE inbox (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  created_ts BIGINT NOT NULL DEFAULT (strftime('%s', 'now')),
  sender_id INTEGER NOT NULL,
  receiver_id INTEGER NOT NULL,
  status TEXT NOT NULL,
  message TEXT NOT NULL DEFAULT '{}'
);

-- memo reaction
CREATE TABLE reaction (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  created_ts BIGINT NOT NULL DEFAULT (strftime('%s', 'now')),
  creator_id INTEGER NOT NULL,
  memo_id INTEGER NOT NULL,
  reaction_type TEXT NOT NULL,
  UNIQUE(creator_id, memo_id, reaction_type)
);

-- memo_share
CREATE TABLE memo_share (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  uid        TEXT    NOT NULL UNIQUE,
  memo_id    INTEGER NOT NULL,
  creator_id INTEGER NOT NULL,
  created_ts BIGINT  NOT NULL DEFAULT (strftime('%s', 'now')),
  expires_ts BIGINT  DEFAULT NULL,
  FOREIGN KEY (memo_id) REFERENCES memo(id) ON DELETE CASCADE
);

CREATE INDEX idx_memo_share_memo_id ON memo_share(memo_id);

-- user_identity
CREATE TABLE user_identity (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id    INTEGER NOT NULL,
  provider   TEXT    NOT NULL,
  extern_uid TEXT    NOT NULL,
  created_ts BIGINT  NOT NULL DEFAULT (strftime('%s', 'now')),
  updated_ts BIGINT  NOT NULL DEFAULT (strftime('%s', 'now')),
  UNIQUE (provider, extern_uid),
  UNIQUE (user_id, provider)
);

CREATE INDEX idx_user_identity_user_id ON user_identity(user_id);

-- Isolated SHRIMP pilot state. Other drivers retain schema parity but do not expose the pilot.
CREATE TABLE IF NOT EXISTS shrimp_subject (
  id VARCHAR(128) PRIMARY KEY,
  user_id INTEGER NOT NULL UNIQUE,
  source_id VARCHAR(128) NOT NULL UNIQUE,
  source_revision VARCHAR(128) NOT NULL,
  source_reference TEXT NOT NULL,
  source_key VARCHAR(64) NOT NULL UNIQUE,
  revision VARCHAR(128) NOT NULL,
  lifecycle VARCHAR(16) NOT NULL,
  display_name TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS shrimp_window (
  id VARCHAR(128) PRIMARY KEY,
  principal VARCHAR(128) NOT NULL,
  closes_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS shrimp_operation (
  principal VARCHAR(128) NOT NULL,
  window_id VARCHAR(128) NOT NULL,
  id VARCHAR(128) NOT NULL,
  fingerprint VARCHAR(64) NOT NULL,
  result TEXT NOT NULL,
  retained_until BIGINT NOT NULL,
  PRIMARY KEY (principal, window_id, id)
);
CREATE TABLE IF NOT EXISTS shrimp_event (
  id VARCHAR(128) PRIMARY KEY,
  sequence BIGINT NOT NULL UNIQUE,
  subject_id VARCHAR(128) NOT NULL,
  revision VARCHAR(128) NOT NULL,
  actor VARCHAR(128) NOT NULL,
  action VARCHAR(128) NOT NULL,
  created_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS shrimp_proof (
  id VARCHAR(64) PRIMARY KEY,
  expires_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS shrimp_deployment (
  id INTEGER PRIMARY KEY,
  resource TEXT NOT NULL
);

-- Durable bounded enumerations for the isolated SHRIMP pilot.
CREATE TABLE IF NOT EXISTS shrimp_enumeration_key (
  id INTEGER PRIMARY KEY,
  secret VARCHAR(64) NOT NULL
);
CREATE TABLE IF NOT EXISTS shrimp_enumeration (
  id VARCHAR(128) PRIMARY KEY,
  principal VARCHAR(128) NOT NULL,
  scope_context TEXT NOT NULL,
  authorization_context TEXT NOT NULL,
  history_epoch VARCHAR(128) NOT NULL,
  selection_hash VARCHAR(64) NOT NULL,
  expires_at BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS shrimp_enumeration_principal_expiry ON shrimp_enumeration(principal, expires_at);

-- Bounded append-only operational evidence for the isolated SHRIMP pilot.
CREATE TABLE IF NOT EXISTS shrimp_audit_state (
  id INTEGER PRIMARY KEY,
  epoch VARCHAR(128) NOT NULL,
  since BIGINT NOT NULL,
  legacy_gap BOOLEAN NOT NULL
);
CREATE TABLE IF NOT EXISTS shrimp_audit_attempt (
  id VARCHAR(128) PRIMARY KEY
);
CREATE TABLE IF NOT EXISTS shrimp_audit_event (
  sequence BIGINT PRIMARY KEY,
  id VARCHAR(128) NOT NULL UNIQUE,
  attempt_id VARCHAR(128) NOT NULL,
  kind VARCHAR(32) NOT NULL,
  body TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS shrimp_audit_attempt_kind ON shrimp_audit_event(attempt_id, kind);
