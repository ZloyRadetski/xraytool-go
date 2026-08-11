package traffic_file

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"xraytool/internal/domain"
)

// Service is private application logic for this traffic backend. Other plugins
// consume only the traffic snapshot contract from pluginapi.
type Service struct {
	cfg             Config
	engine          domain.Engine
	clusterProvider domain.ClusterStatsProvider
}

type Config struct {
	IsMaster                 bool
	StatsStatePath           string
	InferredStatsPath        string
	DetailedRetentionSeconds int64
}

func NewService(cfg Config, engine domain.Engine, clusterProvider domain.ClusterStatsProvider) *Service {
	return &Service{cfg: cfg, engine: engine, clusterProvider: clusterProvider}
}

func (s *Service) GenerateLocalStats() ([]MergedUser, error) {
	if err := s.UpdateLocalStorage(); err != nil {
		return nil, err
	}
	state, err := Load(s.cfg.StatsStatePath, s.cfg.DetailedRetentionSeconds)
	if err != nil {
		return nil, fmt.Errorf("loading stats state: %w", err)
	}
	localUsers := Cumulative(state)
	merged := make([]MergedUser, len(localUsers))
	for i, user := range localUsers {
		combined := user.Total.Combined
		merged[i] = MergedUser{Email: user.Email, Xray: user.Xray, Total: user.Total, ClusterTotal: &combined}
	}
	return merged, nil
}

func (s *Service) UpdateLocalStorage() error {
	if s.engine == nil {
		return fmt.Errorf("traffic engine is unavailable")
	}
	rawStats, err := s.engine.QueryStats(context.Background())
	if err != nil {
		rawStats = nil
	}
	samples := make([]LiveSample, len(rawStats))
	for i, stat := range rawStats {
		samples[i] = LiveSample{Email: stat.Email, Up: stat.Up, Down: stat.Down}
	}
	if users, err := s.engine.ListUsers(context.Background()); err == nil {
		existing := make(map[string]bool, len(samples))
		for _, sample := range samples {
			existing[sample.Email] = true
		}
		for _, user := range users {
			if !existing[user.Email] {
				samples = append(samples, LiveSample{Email: user.Email})
			}
		}
	}
	lockPath := s.cfg.StatsStatePath + ".lock"
	acquired := false
	for i := 0; i < 50; i++ {
		file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL, 0o666)
		if err == nil {
			file.Close()
			acquired = true
			defer os.Remove(lockPath)
			break
		}
		if os.IsExist(err) {
			if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > 30*time.Second {
				_ = os.Remove(lockPath)
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		return fmt.Errorf("failed to create lock file: %w", err)
	}
	if !acquired {
		return fmt.Errorf("timeout waiting for stats lock file")
	}
	state, err := Load(s.cfg.StatsStatePath, s.cfg.DetailedRetentionSeconds)
	if err != nil {
		return err
	}
	Update(state, samples, s.cfg.DetailedRetentionSeconds)
	return Save(s.cfg.StatsStatePath, state)
}

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
	var report domain.SlaveReport
	if !inferredMode && s.cfg.IsMaster && s.clusterProvider != nil {
		slaveTotals, report = s.clusterProvider.CollectSlaveTotals()
	}
	merged := s.mergeWithSlaves(localUsers, slaveTotals)
	if !inferredMode && s.cfg.IsMaster && s.cfg.InferredStatsPath != "" {
		inferred := defaultState()
		for _, user := range merged {
			inferred.Users[user.Email] = &UserState{CumulativeUp: user.Total.Up, CumulativeDown: user.Total.Down + user.Slave}
		}
		inferred.LastSampleTS = time.Now().Unix()
		_ = Save(s.cfg.InferredStatsPath, inferred)
	}
	return merged, report, nil
}

func (s *Service) mergeWithSlaves(local []CumulativeUser, slaveTotals []domain.SlaveUserTotal) []MergedUser {
	slaves := make(map[string]int64, len(slaveTotals))
	for _, total := range slaveTotals {
		slaves[total.Email] = total.Slave
	}
	locals := make(map[string]CumulativeUser, len(local))
	all := make(map[string]bool, len(local)+len(slaves))
	for _, user := range local {
		locals[user.Email] = user
		all[user.Email] = true
	}
	for email := range slaves {
		all[email] = true
	}
	result := make([]MergedUser, 0, len(all))
	for email := range all {
		user, slave := locals[email], slaves[email]
		combined := user.Total.Combined + slave
		result = append(result, MergedUser{Email: email, Xray: user.Xray, Total: TotalCounters{Up: user.Total.Up, Down: user.Total.Down, Combined: combined}, Slave: slave, ClusterTotal: &combined})
	}
	sort.Slice(result, func(i, j int) bool { return *result[i].ClusterTotal > *result[j].ClusterTotal })
	return result
}
