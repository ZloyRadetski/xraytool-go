package commandruntime

import (
	"log/slog"
	"time"

	"xraytool/internal/database"
	clusterreplication "xraytool/internal/plugins/cluster_replication"
)

// configureReplicationRuntime installs the master-only outbox publisher before
// core services are constructed. The transport itself remains entirely inside
// the cluster_replication plugin and starts only after the host has run its
// migrations.
func configureReplicationRuntime(deps *Dependencies) {
	if deps == nil || deps.Cfg == nil || deps.Engine == nil || !deps.Cfg.Replication.Enabled || !deps.Cfg.IsMaster() {
		return
	}
	db, ok := database.GormDB(deps.Registry)
	if !ok || db == nil {
		slog.Error("cluster replication disabled: registry is not GORM-backed")
		return
	}
	service := clusterreplication.NewService(deps.Registry, deps.Engine, clusterreplication.NewStore(db), slog.Default())
	deps.ReplicationService = service
	deps.ClusterProvider = clusterreplication.NewStatsProvider(service, deps.Cfg.Replication.AllowedNodes, parseDuration(deps.Cfg.Replication.StatsInterval, 30*time.Second))
	deps.Engine = clusterreplication.NewPublishingEngine(deps.Engine, service)
}

func parseDuration(raw string, fallback time.Duration) time.Duration {
	if value, err := time.ParseDuration(raw); err == nil && value > 0 {
		return value
	}
	return fallback
}
