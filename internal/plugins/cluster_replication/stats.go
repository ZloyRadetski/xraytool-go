package clusterreplication

import (
	"context"
	"time"

	"xraytool/internal/domain"
)

// StatsProvider exposes the latest slave totals accumulated on the master
// through the replication stream. It retains the existing domain stats port
// without bringing HTTP polling back into the core command path.
type StatsProvider struct {
	store        *Store
	allowedNodes []string
	maxAge       time.Duration
}

func NewStatsProvider(service *Service, allowedNodes []string, statsInterval time.Duration) *StatsProvider {
	if statsInterval <= 0 {
		statsInterval = 30 * time.Second
	}
	var store *Store
	if service != nil {
		store = service.store
	}
	return &StatsProvider{
		store:        store,
		allowedNodes: append([]string(nil), allowedNodes...),
		maxAge:       statsInterval * 3,
	}
}

func (p *StatsProvider) CollectSlaveTotals() ([]domain.SlaveUserTotal, domain.SlaveReport) {
	if p == nil || p.store == nil || len(p.allowedNodes) == 0 {
		return nil, domain.SlaveReport{}
	}
	return p.store.CollectReplicaStats(context.Background(), p.allowedNodes, p.maxAge)
}

var _ domain.ClusterStatsProvider = (*StatsProvider)(nil)
