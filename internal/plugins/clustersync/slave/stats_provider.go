package slave

import (
	json "github.com/goccy/go-json"
	"strings"

	"xraytool/internal/domain"
)

type clusterStatsProvider struct {
	registry *Registry
}

func NewClusterStatsProvider(registry *Registry) domain.ClusterStatsProvider {
	return &clusterStatsProvider{
		registry: registry,
	}
}

func (p *clusterStatsProvider) CollectSlaveTotals() ([]domain.SlaveUserTotal, domain.SlaveReport) {
	servers, err := p.registry.Servers()
	if err != nil || len(servers) == 0 {
		return nil, domain.SlaveReport{Enabled: len(servers) > 0}
	}

	report := domain.SlaveReport{Enabled: true, TotalServers: len(servers)}

	type job struct {
		server string
		users  []domain.SlaveUserTotal
		ok     bool
	}
	jobs := make(chan job, len(servers))

	for name, entry := range servers {
		go func(name string, entry Entry) {
			out, err := p.registry.client.Call(entry, "cli-stats", map[string]string{"api": "true"})
			if err != nil {
				jobs <- job{server: name}
				return
			}
			var parsed struct {
				Users []struct {
					Email string `json:"email"`
					Total struct {
						Combined int64 `json:"combined"`
					} `json:"total"`
					ClusterTotal *int64 `json:"cluster_total"`
				} `json:"users"`
			}
			lines := strings.Split(out, "\n")
			var jsonStr string
			for i := len(lines) - 1; i >= 0; i-- {
				line := strings.TrimSpace(lines[i])
				if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") && strings.Contains(line, "\"ok\"") {
					jsonStr = line
					break
				}
			}

			if jsonStr == "" {
				jobs <- job{server: name}
				return
			}

			if json.Unmarshal([]byte(jsonStr), &parsed) != nil {
				jobs <- job{server: name}
				return
			}
			var totals []domain.SlaveUserTotal
			for _, u := range parsed.Users {
				t := u.Total.Combined
				if u.ClusterTotal != nil {
					t = *u.ClusterTotal
				}
				totals = append(totals, domain.SlaveUserTotal{Email: u.Email, Slave: t})
			}
			jobs <- job{server: name, users: totals, ok: true}
		}(name, entry)
	}

	combined := make(map[string]int64)
	for range servers {
		j := <-jobs
		if j.ok {
			report.OKServers++
			for _, u := range j.users {
				combined[u.Email] += u.Slave
			}
		} else {
			report.FailedServers++
		}
	}

	result := make([]domain.SlaveUserTotal, 0, len(combined))
	for email, total := range combined {
		result = append(result, domain.SlaveUserTotal{Email: email, Slave: total})
	}
	return result, report
}
