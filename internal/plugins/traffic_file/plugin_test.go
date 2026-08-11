package traffic_file

import (
	"context"
	"path/filepath"
	"testing"

	"xraytool/internal/appconfig"
)

func TestPlugin_UsagePrefersInferredTraffic(t *testing.T) {
	dir := t.TempDir()
	inferred := filepath.Join(dir, "inferred.json")
	local := filepath.Join(dir, "local.json")
	if err := Save(inferred, &State{Version: 2, Users: map[string]*UserState{
		"user@example.com": {CumulativeUp: 10, CumulativeDown: 20},
	}}); err != nil {
		t.Fatalf("save inferred state: %v", err)
	}
	if err := Save(local, &State{Version: 2, Users: map[string]*UserState{
		"user@example.com": {CumulativeUp: 100, CumulativeDown: 200},
	}}); err != nil {
		t.Fatalf("save local state: %v", err)
	}
	p := New(&appconfig.Config{})
	p.cfg.Paths.InferredStats = inferred
	p.cfg.Paths.StatsState = local
	usage, found, err := p.Usage(context.Background(), "user@example.com")
	if err != nil || !found || usage.UploadBytes != 10 || usage.DownloadBytes != 20 {
		t.Fatalf("Usage = (%+v, %t, %v), want inferred usage", usage, found, err)
	}
}

func TestPlugin_GenerateClusterStatsOwnsFileBackedWorkflow(t *testing.T) {
	dir := t.TempDir()
	inferred := filepath.Join(dir, "inferred.json")
	requireNoError(t, Save(inferred, &State{Version: 2, Users: map[string]*UserState{
		"user@example.com": {CumulativeUp: 10, CumulativeDown: 20},
	}}))
	p := New(&appconfig.Config{})
	p.cfg.Paths.InferredStats = inferred
	users, report, err := p.GenerateClusterStats(true, inferred, nil, nil)
	requireNoError(t, err)
	if report.Enabled || len(users) != 1 || users[0].Email != "user@example.com" || users[0].Total.Combined != 30 {
		t.Fatalf("GenerateClusterStats() = (%+v, %+v), want cumulative file-backed user", users, report)
	}
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
