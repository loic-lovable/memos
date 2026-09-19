
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
