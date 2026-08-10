CREATE TABLE IF NOT EXISTS cluster_replication_stats (
  node_id TEXT NOT NULL,
  email TEXT NOT NULL,
  total INTEGER NOT NULL,
  updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (node_id, email)
);
