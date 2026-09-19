
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
