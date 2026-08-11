package traffic_file

import (
	"context"
	"path/filepath"
	"testing"

	"xraytool/internal/appconfig"
	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
)

func TestPluginSyncClusterTrafficWritesSubscriptionState(t *testing.T) {
	dir := t.TempDir()
	p := New(&appconfig.Config{})
	p.cfg.Mode = "master"
	p.cfg.Paths.StatsState = filepath.Join(dir, "local.json")
	p.cfg.Paths.InferredStats = filepath.Join(dir, "inferred.json")
	p.engine = &testTrafficEngine{stats: []domain.TrafficStat{{Email: "user@example.com", Up: 100, Down: 200}}}

	err := p.SyncClusterTraffic(context.Background(), []pluginapi.TrafficSnapshot{{
		Email: "user@example.com", Usage: pluginapi.TrafficUsage{DownloadBytes: 500},
	}})
	if err != nil {
		t.Fatal(err)
	}
	inferred, err := Load(p.cfg.Paths.InferredStats, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := inferred.Users["user@example.com"]
	if got == nil || got.CumulativeUp != 100 || got.CumulativeDown != 700 {
		t.Fatalf("inferred state = %+v, want 100/700", got)
	}
}
