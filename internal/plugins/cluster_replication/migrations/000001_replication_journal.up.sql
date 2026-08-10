CREATE TABLE IF NOT EXISTS cluster_replication_outbox (
  revision INTEGER PRIMARY KEY AUTOINCREMENT,
  kind TEXT NOT NULL,
  payload TEXT NOT NULL,
  checksum TEXT NOT NULL,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS cluster_replication_inbox (
  master_id TEXT NOT NULL,
  revision INTEGER NOT NULL,
  checksum TEXT NOT NULL,
  applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (master_id, revision)
);

CREATE TABLE IF NOT EXISTS cluster_replication_desired_users (
  email TEXT PRIMARY KEY,
  payload TEXT NOT NULL,
  revision INTEGER NOT NULL,
  updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS cluster_replication_staging_users (
  snapshot_id TEXT NOT NULL,
  email TEXT NOT NULL,
  payload TEXT NOT NULL,
  PRIMARY KEY (snapshot_id, email)
);

CREATE TABLE IF NOT EXISTS cluster_replication_artifacts (
  kind TEXT PRIMARY KEY,
  payload TEXT NOT NULL,
  checksum TEXT NOT NULL,
  revision INTEGER NOT NULL,
  updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS cluster_replication_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
