package traffic_file

import (
	"context"
	"time"

	"xraytool/internal/appconfig"
	"xraytool/internal/domain"
)

type MergedUser struct {
	Email        string        `json:"email"`
	Xray         XrayCounters  `json:"xray"`
	Total        TotalCounters `json:"total"`
	Slave        int64         `json:"slave"`
	ClusterTotal *int64        `json:"cluster_total,omitempty"`
}

type SlaveReportJSON struct {
	Enabled       bool `json:"enabled"`
	TotalServers  int  `json:"total_servers"`
	OKServers     int  `json:"ok_servers"`
	FailedServers int  `json:"failed_servers"`
}
type NodeStatsReport struct {
	Ok          bool                   `json:"ok"`
	Error       string                 `json:"error,omitempty"`
	Message     string                 `json:"message,omitempty"`
	GeneratedAt string                 `json:"generated_at,omitempty"`
	Partial     bool                   `json:"partial,omitempty"`
	SlaveReport SlaveReportJSON        `json:"slave_report,omitempty"`
	Users       []MergedUser           `json:"users,omitempty"`
	Totals      map[string]interface{} `json:"totals,omitempty"`
}

func sumField(users []MergedUser, get func(MergedUser) int64) int64 {
	var total int64
	for _, user := range users {
		total += get(user)
	}
	return total
}

// GenerateLocalStats preserves the historical programmatic report API for
// callers inside this plugin family. Cross-plugin callers use LocalTrafficSnapshot.
func GenerateLocalStats(cfg *appconfig.Config, engine domain.Engine) NodeStatsReport {
	statePath := cfg.Paths.StatsState
	rawStats, err := engine.QueryStats(context.Background())
	if err != nil {
		rawStats = nil
	}
	samples := make([]LiveSample, 0, len(rawStats))
	for _, stat := range rawStats {
		samples = append(samples, LiveSample{Email: stat.Email, Up: stat.Up, Down: stat.Down})
	}
	state, err := Load(statePath, cfg.DetailedRetentionSeconds())
	if err != nil {
		return NodeStatsReport{Ok: false, Error: "STATS_LOAD_FAILED", Message: err.Error()}
	}
	Update(state, samples, cfg.DetailedRetentionSeconds())
	if err := Save(statePath, state); err != nil {
		return NodeStatsReport{Ok: false, Error: "STATS_SAVE_FAILED", Message: err.Error()}
	}
	local := Cumulative(state)
	users := make([]MergedUser, 0, len(local))
	var xrayTotal, clusterTotal int64
	for _, user := range local {
		total := user.Total.Combined
		users = append(users, MergedUser{Email: user.Email, Xray: user.Xray, Total: user.Total, ClusterTotal: &total})
		xrayTotal += user.Xray.Total
		clusterTotal += total
	}
	return NodeStatsReport{Ok: true, GeneratedAt: time.Now().UTC().Format(time.RFC3339), Users: users, SlaveReport: SlaveReportJSON{Enabled: false}, Totals: map[string]interface{}{"xray": map[string]int64{"up": sumField(users, func(user MergedUser) int64 { return user.Xray.Up }), "down": sumField(users, func(user MergedUser) int64 { return user.Xray.Down }), "total": xrayTotal}, "slave": map[string]int64{"total": 0}, "cluster": map[string]int64{"combined": clusterTotal}}}
}
