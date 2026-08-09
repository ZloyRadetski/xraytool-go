package traffic_file

import (
	"context"
	"path/filepath"
	"testing"

	"xraytool/internal/appconfig"
	"xraytool/internal/stats"
)

func TestPlugin_UsagePrefersInferredTraffic(t *testing.T) {
	dir := t.TempDir()
	inferred := filepath.Join(dir, "inferred.json")
	local := filepath.Join(dir, "local.json")
	if err := stats.Save(inferred, &stats.State{Version: 2, Users: map[string]*stats.UserState{
		"user@example.com": {CumulativeUp: 10, CumulativeDown: 20},
	}}); err != nil {
		t.Fatalf("save inferred state: %v", err)
	}
	if err := stats.Save(local, &stats.State{Version: 2, Users: map[string]*stats.UserState{
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
