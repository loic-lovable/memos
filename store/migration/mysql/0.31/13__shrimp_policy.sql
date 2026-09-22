-- Bind the local trust anchor and prevent unreconciled writer reinstatement.
CREATE TABLE IF NOT EXISTS shrimp_policy (
  id INTEGER PRIMARY KEY,
  issuer_key VARCHAR(64) NOT NULL,
  write_withdrawn BOOLEAN NOT NULL
);
