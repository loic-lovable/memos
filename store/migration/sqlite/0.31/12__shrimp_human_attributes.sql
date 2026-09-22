-- Profile selection and private email identity history commit with the account.
ALTER TABLE shrimp_subject ADD COLUMN attribute_profile VARCHAR(64) NOT NULL DEFAULT '';
ALTER TABLE shrimp_subject ADD COLUMN human_attributes TEXT NOT NULL DEFAULT 'null';
CREATE TABLE IF NOT EXISTS shrimp_attribute_approval (
  authorization VARCHAR(128) PRIMARY KEY,
  principal VARCHAR(128) NOT NULL,
  window_id VARCHAR(128) NOT NULL,
  operation_id VARCHAR(128) NOT NULL,
  fingerprint VARCHAR(64) NOT NULL,
  owners TEXT NOT NULL,
  expires_at BIGINT NOT NULL,
  consumed INTEGER NOT NULL DEFAULT 0
);
