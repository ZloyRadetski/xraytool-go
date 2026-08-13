package traffic_file

import (
	"context"
	"fmt"
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

func (s *Service) GenerateLocalStats(ctx context.Context) ([]MergedUser, error) {
	if err := s.UpdateLocalStorage(ctx); err != nil {
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

// UpdateLocalStorage samples the live cumulative Xray counters and advances
// the file-backed state atomically. The state lock deliberately covers the
// Xray read as well as Load/Update/Save: otherwise two callers can read values
// out of order and mistake an older sample for a counter reset.
//
// A failed live read never changes LastRaw* or cumulative totals. Treating a
// transport failure as an all-zero sample would reset the baseline and count
// the entire next successful Xray counter a second time.
func (s *Service) UpdateLocalStorage(ctx context.Context) error {
	if s.engine == nil {
		return fmt.Errorf("traffic engine is unavailable")
	}
	if s.cfg.StatsStatePath == "" {
		return fmt.Errorf("traffic stats state path is empty")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return withStateLock(ctx, s.cfg.StatsStatePath, func() error {
		rawStats, err := s.engine.QueryStats(ctx)
		if err != nil {
			return fmt.Errorf("query traffic statistics: %w", err)
		}
		samples := make([]LiveSample, 0, len(rawStats))
		for _, stat := range rawStats {
			if stat.Email == "" {
				continue
			}
			samples = append(samples, LiveSample{Email: stat.Email, Up: stat.Up, Down: stat.Down})
		}
		if users, listErr := s.engine.ListUsers(ctx); listErr == nil {
			existing := make(map[string]bool, len(samples))
			for _, sample := range samples {
				existing[sample.Email] = true
			}
			for _, user := range users {
				if user.Email != "" && !existing[user.Email] {
					samples = append(samples, LiveSample{Email: user.Email})
				}
			}
		}
		state, err := Load(s.cfg.StatsStatePath, s.cfg.DetailedRetentionSeconds)
		if err != nil {
			return err
		}
		Update(state, samples, s.cfg.DetailedRetentionSeconds)
		return Save(s.cfg.StatsStatePath, state)
	})
}

func (s *Service) GenerateClusterStats(ctx context.Context, inferredMode bool, statePath string) ([]MergedUser, domain.SlaveReport, error) {
	// Master-side report generation and replication both rewrite the inferred
	// state. Use one aggregate lock so a slower report cannot overwrite a newer
	// replication result with an older local snapshot.
	if !inferredMode && s.cfg.IsMaster && s.cfg.InferredStatsPath != "" {
		var (
			users  []MergedUser
			report domain.SlaveReport
			err    error
		)
		err = withStateLock(ctx, s.cfg.InferredStatsPath+".aggregate", func() error {
			users, report, err = s.generateClusterStats(ctx, inferredMode, statePath)
			return err
		})
		return users, report, err
	}
	return s.generateClusterStats(ctx, inferredMode, statePath)
}

func (s *Service) generateClusterStats(ctx context.Context, inferredMode bool, statePath string) ([]MergedUser, domain.SlaveReport, error) {
	if !inferredMode {
		if err := s.UpdateLocalStorage(ctx); err != nil {
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
		if err := Save(s.cfg.InferredStatsPath, inferred); err != nil {
			return nil, report, fmt.Errorf("writing inferred stats state: %w", err)
		}
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
