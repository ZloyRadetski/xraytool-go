package traffic_file

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xraytool/internal/appconfig"
	"xraytool/internal/domain"
)

func TestStatePersistsAndCountsCounterReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.json")
	state, err := Load(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	Update(state, []LiveSample{{Email: "u@example.com", Up: 100, Down: 200}}, 0)
	Update(state, []LiveSample{{Email: "u@example.com", Up: 10, Down: 20}}, 0)
	if got := state.Users["u@example.com"]; got.CumulativeUp != 110 || got.CumulativeDown != 220 {
		t.Fatalf("reset counters = %+v, want cumulative 110/220", got)
	}
	if err := Save(path, state); err != nil {
		t.Fatal(err)
	}
	restored, err := Load(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := restored.Users["u@example.com"]; got.CumulativeUp != 110 || got.LastRawUp != 10 {
		t.Fatalf("restored counters = %+v", got)
	}
}

func TestHumanBytes(t *testing.T) {
	for input, want := range map[int64]string{0: "0B", 500: "500B", 1024: "1.0K", 1048576: "1.0M", 1073741824: "1.00G"} {
		if got := HumanBytes(input); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", input, got, want)
		}
	}
}

type testTrafficEngine struct {
	domain.Engine
	stats    []domain.TrafficStat
	statsErr error
	users    []domain.VPNUserConfig
}

func (e *testTrafficEngine) QueryStats(context.Context) ([]domain.TrafficStat, error) {
	if e.statsErr != nil {
		return nil, e.statsErr
	}
	return e.stats, nil
}
func (e *testTrafficEngine) ListUsers(context.Context) ([]domain.VPNUserConfig, error) {
	return e.users, nil
}

type testClusterStats struct{ totals []domain.SlaveUserTotal }

func (s testClusterStats) CollectSlaveTotals() ([]domain.SlaveUserTotal, domain.SlaveReport) {
	return s.totals, domain.SlaveReport{Enabled: true, TotalServers: 1, OKServers: 1}
}

func TestServiceWritesInferredClusterTotals(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	inferredPath := filepath.Join(dir, "inferred.json")
	if err := Save(statePath, &State{Version: 2, Users: map[string]*UserState{"u@example.com": {CumulativeUp: 100, CumulativeDown: 200}}}); err != nil {
		t.Fatal(err)
	}
	service := NewService(Config{IsMaster: true, StatsStatePath: statePath, InferredStatsPath: inferredPath}, &testTrafficEngine{}, testClusterStats{totals: []domain.SlaveUserTotal{{Email: "u@example.com", Slave: 500}}})
	users, _, err := service.GenerateClusterStats(context.Background(), false, statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].Total.Combined != 800 {
		t.Fatalf("cluster users = %+v, want total 800", users)
	}
	inferred, err := Load(inferredPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := inferred.Users["u@example.com"]; got.CumulativeUp != 100 || got.CumulativeDown != 700 {
		t.Fatalf("inferred = %+v, want 100/700", got)
	}
}

func TestPluginLocalTrafficSnapshotUsesCumulativeBackend(t *testing.T) {
	dir := t.TempDir()
	p := New(&appconfig.Config{})
	p.cfg.Paths.StatsState = filepath.Join(dir, "state.json")
	p.engine = &testTrafficEngine{stats: []domain.TrafficStat{{Email: "u@example.com", Up: 4, Down: 9}}}
	snapshot, err := p.LocalTrafficSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot) != 1 || snapshot[0].Email != "u@example.com" || snapshot[0].Usage.UploadBytes != 4 || snapshot[0].Usage.DownloadBytes != 9 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestServiceKeepsCounterBaselineWhenLiveStatsReadFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := Save(path, &State{Version: 2, Users: map[string]*UserState{
		"user@example.com": {
			CumulativeUp: 100, CumulativeDown: 200,
			LastRawUp: 100, LastRawDown: 200,
		},
	}}); err != nil {
		t.Fatal(err)
	}
	engine := &testTrafficEngine{statsErr: errors.New("xray is unavailable")}
	service := NewService(Config{StatsStatePath: path}, engine, nil)
	if err := service.UpdateLocalStorage(context.Background()); err == nil {
		t.Fatal("UpdateLocalStorage() succeeded after a failed live stats read")
	}

	state, err := Load(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := state.Users["user@example.com"]
	if got == nil || got.CumulativeUp != 100 || got.CumulativeDown != 200 || got.LastRawUp != 100 || got.LastRawDown != 200 {
		t.Fatalf("state changed after failed sample: %+v", got)
	}

	engine.statsErr = nil
	engine.stats = []domain.TrafficStat{{Email: "user@example.com", Up: 150, Down: 250}}
	if err := service.UpdateLocalStorage(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err = Load(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	got = state.Users["user@example.com"]
	if got == nil || got.CumulativeUp != 150 || got.CumulativeDown != 250 {
		t.Fatalf("successful sample was counted from a reset baseline: %+v", got)
	}
}

type serializedStatsEngine struct {
	domain.Engine
	active    atomic.Int32
	maxActive atomic.Int32
}

func (e *serializedStatsEngine) QueryStats(ctx context.Context) ([]domain.TrafficStat, error) {
	active := e.active.Add(1)
	for {
		maximum := e.maxActive.Load()
		if active <= maximum || e.maxActive.CompareAndSwap(maximum, active) {
			break
		}
	}
	defer e.active.Add(-1)

	timer := time.NewTimer(25 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return []domain.TrafficStat{{Email: "user@example.com", Up: 100, Down: 200}}, nil
	}
}

func (*serializedStatsEngine) ListUsers(context.Context) ([]domain.VPNUserConfig, error) {
	return nil, nil
}

func TestServiceSerializesLiveReadAndStateUpdate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	engine := &serializedStatsEngine{}
	service := NewService(Config{StatsStatePath: path}, engine, nil)

	const workers = 4
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- service.UpdateLocalStorage(context.Background())
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := engine.maxActive.Load(); got != 1 {
		t.Fatalf("concurrent QueryStats calls = %d, want 1 while state is sampled", got)
	}
	state, err := Load(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := state.Users["user@example.com"]
	if got == nil || got.CumulativeUp != 100 || got.CumulativeDown != 200 {
		t.Fatalf("cumulative state after serialized samples = %+v, want 100/200", got)
	}
}
