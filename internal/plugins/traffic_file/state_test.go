package traffic_file

import (
	"context"
	"path/filepath"
	"testing"

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
	stats []domain.TrafficStat
	users []domain.VPNUserConfig
}

func (e *testTrafficEngine) QueryStats(context.Context) ([]domain.TrafficStat, error) {
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
	users, _, err := service.GenerateClusterStats(false, statePath)
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
