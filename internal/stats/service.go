package stats

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"xraytool/internal/domain"
)

// Service provides application logic for collecting and merging statistics.
type Service struct {
	cfg             Config
	engine          domain.Engine
	clusterProvider domain.ClusterStatsProvider
}

type Config struct {
	IsMaster                 bool
	StatsStatePath           string
	DetailedRetentionSeconds int64
}

func NewService(cfg Config, engine domain.Engine, clusterProvider domain.ClusterStatsProvider) *Service {
	return &Service{cfg: cfg, engine: engine, clusterProvider: clusterProvider}
}

// GenerateLocalStats performs exactly what the 'cli-stats' endpoint does internally:
// updates the local state and then returns the cumulative users.
func (s *Service) GenerateLocalStats() ([]MergedUser, error) {
	if err := s.UpdateLocalStorage(); err != nil {
		return nil, err
	}
	state, err := Load(s.cfg.StatsStatePath, s.cfg.DetailedRetentionSeconds)
	if err != nil {
		return nil, fmt.Errorf("loading stats state: %w", err)
	}
	localUsers := Cumulative(state)

	// Convert CumulativeUser to MergedUser (slave = 0)
	merged := make([]MergedUser, len(localUsers))
	for i, u := range localUsers {
		ct := u.Total.Combined
		merged[i] = MergedUser{
			Email:        u.Email,
			Xray:         u.Xray,
			Total:        u.Total,
			Slave:        0,
			ClusterTotal: &ct,
		}
	}
	return merged, nil
}

// UpdateLocalStorage pulls stats from the VPN engine and updates the on-disk state.
func (s *Service) UpdateLocalStorage() error {
	statePath := s.cfg.StatsStatePath
	rawStats, err := s.engine.QueryStats(context.Background())
	if err != nil {
		rawStats = nil
	}

	samples := make([]LiveSample, len(rawStats))
	for i, stat := range rawStats {
		samples[i] = LiveSample{Email: stat.Email, Up: stat.Up, Down: stat.Down}
	}

	if inUsers, err := s.engine.ListUsers(context.Background()); err == nil {
		existing := make(map[string]bool, len(samples))
		for _, smp := range samples {
			existing[smp.Email] = true
		}
		for _, u := range inUsers {
			if !existing[u.Email] {
				samples = append(samples, LiveSample{
					Email: u.Email,
					Up:    0,
					Down:  0,
				})
			}
		}
	}

	lockPath := statePath + ".lock"
	for i := 0; i < 50; i++ {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL, 0666)
		if err == nil {
			f.Close()
			defer os.Remove(lockPath)
			goto acquired
		}
		if os.IsExist(err) {
			if stat, err := os.Stat(lockPath); err == nil {
				if time.Since(stat.ModTime()) > 30*time.Second {
					os.Remove(lockPath)
				}
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		return fmt.Errorf("failed to create lock file: %w", err)
	}
	return fmt.Errorf("timeout waiting for stats lock file")

acquired:
	state, err := Load(statePath, s.cfg.DetailedRetentionSeconds)
	if err != nil {
		return err
	}
	Update(state, samples, s.cfg.DetailedRetentionSeconds)
	return Save(statePath, state)
}

// GenerateClusterStats builds merged stats spanning master and all slave nodes.
func (s *Service) GenerateClusterStats(inferredMode bool, statePath string) ([]MergedUser, domain.SlaveReport, error) {
	if !inferredMode {
		if err := s.UpdateLocalStorage(); err != nil {
			return nil, domain.SlaveReport{}, fmt.Errorf("stats update failed: %w", err)
		}
	}

	state, err := Load(statePath, s.cfg.DetailedRetentionSeconds)
	if err != nil {
		return nil, domain.SlaveReport{}, fmt.Errorf("loading stats state: %w", err)
	}
	localUsers := Cumulative(state)

	var slaveTotals []domain.SlaveUserTotal
	var slaveReport domain.SlaveReport

	if !inferredMode && s.cfg.IsMaster {
		slaveTotals, slaveReport = s.clusterProvider.CollectSlaveTotals()
	}

	merged := s.mergeWithSlaves(localUsers, slaveTotals)
	return merged, slaveReport, nil
}

func (s *Service) mergeWithSlaves(local []CumulativeUser, slaveTotals []domain.SlaveUserTotal) []MergedUser {
	slaveMap := make(map[string]int64, len(slaveTotals))
	for _, s := range slaveTotals {
		slaveMap[s.Email] = s.Slave
	}

	localMap := make(map[string]CumulativeUser, len(local))
	for _, u := range local {
		localMap[u.Email] = u
	}

	all := make(map[string]bool)
	for _, u := range local {
		all[u.Email] = true
	}
	for e := range slaveMap {
		all[e] = true
	}

	result := make([]MergedUser, 0, len(all))
	for email := range all {
		u := localMap[email]
		s := slaveMap[email]
		ct := u.Total.Combined + s
		result = append(result, MergedUser{
			Email: email,
			Xray:  u.Xray,
			Total: TotalCounters{
				Up:       u.Total.Up,
				Down:     u.Total.Down,
				Combined: ct,
			},
			Slave:        s,
			ClusterTotal: &ct,
		})
	}

	// Sort by total bandwidth descending
	sort.Slice(result, func(i, j int) bool {
		return *result[i].ClusterTotal > *result[j].ClusterTotal
	})

	return result
}
