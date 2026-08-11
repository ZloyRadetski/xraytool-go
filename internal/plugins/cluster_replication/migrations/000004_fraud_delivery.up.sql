-- Slave-local durable anti-fraud queue. Event IDs are UUIDs so an ACK cannot
-- accidentally acknowledge a later observation after local row pruning.
CREATE TABLE IF NOT EXISTS cluster_replication_fraud_outbox (
  event_id TEXT PRIMARY KEY,
  email TEXT NOT NULL,
  ip_hash TEXT NOT NULL,
  hash_key_id TEXT NOT NULL DEFAULT '',
  occurred_at DATETIME NOT NULL,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS cluster_replication_fraud_outbox_created_idx
  ON cluster_replication_fraud_outbox (created_at, event_id);

-- Master-side idempotency receipts. A slave retries until it receives an ACK;
-- duplicate delivery must therefore never inflate the anti-fraud IP count.
CREATE TABLE IF NOT EXISTS cluster_replication_fraud_inbox (
  node_id TEXT NOT NULL,
  event_id TEXT NOT NULL,
  email TEXT NOT NULL,
  ip_hash TEXT NOT NULL,
  hash_key_id TEXT NOT NULL DEFAULT '',
  occurred_at DATETIME NOT NULL,
  received_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (node_id, event_id)
);
