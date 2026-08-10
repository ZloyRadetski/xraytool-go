# Cluster replication

`cluster_replication` is the only master/slave replication mechanism in xraytool. It replaces the removed `cluster_sync` plugin and the HTTP endpoints under `/api/v1/internal/xray/sync`.

The master is the only source of truth. Slaves establish a long-lived gRPC connection to it using TLS 1.3 mutual authentication. Each slave writes an inbox record before acknowledging an event, so reconnects safely replay unacknowledged data. Initialisation and forced recovery use a streamed snapshot: every user and configuration artifact is sent as a separate frame rather than as one large JSON request.

## Cutover

Upgrade every node to the same xraytool release, install certificates, update the configuration on each node, then start the master and slaves. There is no compatibility mode with the old HTTP synchronisation protocol. `master_api`, `slave_api`, `slave_servers`, and `plugins.cluster_sync` are rejected by configuration loading.

Keep the old database backup until the rollout is confirmed. The old `sync_events` and `sync_states` tables are not dropped automatically; they are unused by the new plugin and remain only to avoid destructive migration behaviour.

## Configuration

On the master:

```yaml
mode: master
replication:
  enabled: true
  node_id: master-1
  listen_address: "0.0.0.0:9443"
  allowed_nodes: ["slave-ru-1", "slave-eu-1"]
  ca_file: "/etc/xraytool/replication/ca.pem"
  cert_file: "/etc/xraytool/replication/master.pem"
  key_file: "/etc/xraytool/replication/master-key.pem"
  master_scan_interval: "30s"
  stats_interval: "30s"
  drift_interval: "1m"
```

On a slave:

```yaml
mode: slave
replication:
  enabled: true
  node_id: slave-ru-1
  master_address: "master.example.com:9443"
  server_name: "master.example.com" # optional SNI override
  ca_file: "/etc/xraytool/replication/ca.pem"
  cert_file: "/etc/xraytool/replication/slave-ru-1.pem"
  key_file: "/etc/xraytool/replication/slave-ru-1-key.pem"
  reconnect_interval: "5s"
  drift_interval: "1m"
  stats_interval: "30s"
```

The common name (CN) in each slave certificate must equal `node_id`, and the master must list that value in `allowed_nodes`. Certificate and key files should be readable only by the xraytool service account.

## Behaviour and operations

- Master-side engine mutations create compact durable outbox records. A periodic desired-state digest additionally catches database changes made outside the engine path and emits a streamed resnapshot marker. The master retains events until every configured slave has acknowledged them; a lagging node whose retained history is unavailable receives a fresh stream snapshot.
- Static template clients and Reality keys are configuration artifacts. They are transmitted only through the mTLS stream; Reality keys are written atomically with mode `0600` on a slave.
- Slave traffic totals are reported over the same stream and retained by node on the master, so cluster statistics no longer poll the removed HTTP endpoint.
- A slave persists the desired user projection. Its drift loop rebuilds managed engine state from that projection, so a manually deleted user in one inbound is restored; slave edits never flow back to the master.
- Plugin operations are available through `xraytool plugin run cluster_replication status` and `xraytool plugin run cluster_replication snapshot` on a master. `snapshot` appends a compact marker; connected slaves pull the expanded stream themselves.
