package stats

import (
	"time"

	"xraytool/internal/appconfig"
	"xraytool/internal/xrayapi"
	"xraytool/internal/xrayconfig"
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

func sumField(users []MergedUser, f func(MergedUser) int64) int64 {
	var s int64
	for _, u := range users {
		s += f(u)
	}
	return s
}

// GenerateLocalStats retrieves traffic stats directly from the local node.
func GenerateLocalStats(cfg *appconfig.Config) NodeStatsReport {
	statePath := cfg.Paths.StatsState
	
	apiClient := xrayapi.NewGRPCClient(cfg.Xray.APIAddr)
	rawStats, err := apiClient.QueryStats()
	if err != nil {
		rawStats = nil
	}

	samples := make([]LiveSample, len(rawStats))
	for i, s := range rawStats {
		samples[i] = LiveSample{Email: s.Email, Up: s.Up, Down: s.Down}
	}

	if xrayCfg, err := xrayconfig.Read(cfg.Paths.XrayConfig); err == nil {
		inUsers, _ := xrayconfig.ListUsers(xrayCfg)
		existing := make(map[string]bool, len(samples))
		for _, s := range samples {
			existing[s.Email] = true
		}
		for _, u := range inUsers {
			if e := u.Email(); !existing[e] {
				samples = append(samples, LiveSample{Email: e})
			}
		}
	}

	state, err := Load(statePath, cfg.DetailedRetentionSeconds())
	if err != nil {
		return NodeStatsReport{Ok: false, Error: "STATS_LOAD_FAILED", Message: err.Error()}
	}
	Update(state, samples, cfg.DetailedRetentionSeconds())
	if err := Save(statePath, state); err != nil {
		return NodeStatsReport{Ok: false, Error: "STATS_SAVE_FAILED", Message: err.Error()}
	}

	localUsers := Cumulative(state)
	merged := make([]MergedUser, 0, len(localUsers))
	var xTotal int64
	for _, u := range localUsers {
		mu := MergedUser{
			Email: u.Email,
			Xray:  u.Xray,
			Total: u.Total,
		}
		merged = append(merged, mu)
		xTotal += mu.Xray.Total
	}

	var clTotal int64
	for i := range merged {
		t := merged[i].Total.Combined + merged[i].Slave
		merged[i].ClusterTotal = &t
		clTotal += t
	}

	return NodeStatsReport{
		Ok:          true,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Users:       merged,
		SlaveReport: SlaveReportJSON{Enabled: false},
		Totals: map[string]interface{}{
			"xray":    map[string]int64{"up": sumField(merged, func(u MergedUser) int64 { return u.Xray.Up }), "down": sumField(merged, func(u MergedUser) int64 { return u.Xray.Down }), "total": xTotal},
			"slave":   map[string]int64{"total": 0},
			"cluster": map[string]int64{"combined": clTotal},
		},
	}
}
