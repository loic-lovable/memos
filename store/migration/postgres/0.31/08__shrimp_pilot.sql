
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
