package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"xraytool/internal/slave"
	"xraytool/internal/stats"
	"xraytool/internal/xrayapi"
	"xraytool/internal/xrayconfig"

	"github.com/spf13/cobra"
)

func statsCmd() *cobra.Command {
	var (
		apiMode      bool
		inferredMode bool
		emailFilter  string
		nameFilter   string
	)

	cmd := &cobra.Command{
		Use:   "cli-stats",
		Short: "Show per-user traffic statistics",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := requireRoot(); err != nil {
				return err
			}
			p := newPrinter(apiMode)

			if emailFilter == "" {
				emailFilter = nameFilter
			}

			if emailFilter != "" && !regexp.MustCompile(`^[a-zA-Z0-9@._-]+$`).MatchString(emailFilter) {
				return p.Error("invalid characters in email filter (allowed: a-z A-Z 0-9 @ . _ -; cannot start with -)")
			}

			statePath := cfg.Paths.StatsState
			if inferredMode {
				statePath = cfg.Paths.InferredStats
			}

			// Update state from live xray API (unless in inferred mode).
			if !inferredMode {
				if err := updateStatsStorage(statePath); err != nil {
					if apiMode {
						printJSON(map[string]interface{}{"ok": false, "error": "STATS_UPDATE_FAILED", "message": err.Error()})
					} else {
						p.Warn("Stats update failed: %v", err)
					}
					return nil
				}
			}

			state, err := stats.Load(statePath, cfg.DetailedRetentionSeconds())
			if err != nil {
				return p.Errorf("loading stats state: %v", err)
			}

			localUsers := stats.Cumulative(state)

			// Collect slave totals (master only, non-inferred).
			var slaveTotals []slaveUserTotal
			var slaveReport stats.SlaveReportJSON
			if !inferredMode && cfg.IsMaster() {
				slaveTotals, slaveReport = collectSlaveTotals()
			}

			// Merge local + slave data.
			merged := mergeWithSlaves(localUsers, slaveTotals)

			if emailFilter != "" {
				found := false
				for _, u := range merged {
					if u.Email == emailFilter {
						found = true
						if apiMode {
							printJSON(map[string]interface{}{
								"ok":           true,
								"generated_at": nowUTC(),
								"user":         u,
								"slave_report": slaveReport,
							})
						} else {
							printUserStatsTable(u)
						}
						return nil
					}
				}
				if apiMode {
					printJSON(map[string]interface{}{"ok": false, "error": "USER_NOT_FOUND", "email": emailFilter})
				}
				if !found {
					return p.Errorf("user not found: %s", emailFilter)
				}
				return nil
			}

			// Filter zero-traffic entries for interactive display.
			sort.Slice(merged, func(i, j int) bool {
				return clusterTotal(merged[i]) > clusterTotal(merged[j])
			})

			if apiMode {
				// Compute totals.
				var xTotal, slaveTotal, clTotal int64
				for _, u := range merged {
					xTotal += u.Xray.Total
					slaveTotal += u.Slave
					clTotal += clusterTotal(u)
				}
				printJSON(map[string]interface{}{
					"ok":           true,
					"generated_at": nowUTC(),
					"partial":      slaveReport.FailedServers > 0,
					"slave_report": slaveReport,
					"users":        merged,
					"totals": map[string]interface{}{
						"xray":    map[string]int64{"up": sumField(merged, func(u stats.MergedUser) int64 { return u.Xray.Up }), "down": sumField(merged, func(u stats.MergedUser) int64 { return u.Xray.Down }), "total": xTotal},
						"slave":   map[string]int64{"total": slaveTotal},
						"cluster": map[string]int64{"combined": clTotal},
					},
				})
				return nil
			}

			// Print slave status.
			if cfg.IsMaster() && slaveReport.TotalServers > 0 {
				if slaveReport.FailedServers > 0 {
					p.Warn("Slave stats: ok=%d failed=%d total=%d",
						slaveReport.OKServers, slaveReport.FailedServers, slaveReport.TotalServers)
				} else {
					p.Info("Slave stats: all %d servers responded.", slaveReport.TotalServers)
				}
			}

			printAllStatsTable(merged)
			return nil
		},
	}

	cmd.Flags().BoolVar(&apiMode, "api", false, "Output JSON (machine-readable)")
	cmd.Flags().BoolVar(&inferredMode, "inferred", false, "Use inferred traffic stats file")
	cmd.Flags().StringVar(&emailFilter, "email", "", "Filter to a single user")
	cmd.Flags().StringVar(&nameFilter, "name", "", "Alias for --email")
	return cmd
}

// ---------------------------------------------------------------------------
// Stats update
// ---------------------------------------------------------------------------

func updateStatsStorage(statePath string) error {
	apiClient := xrayapi.NewGRPCClient(cfg.Xray.APIAddr)
	rawStats, err := apiClient.QueryStats()
	if err != nil {
		// Non-fatal; xray might be restarting.
		rawStats = nil
	}

	samples := make([]stats.LiveSample, len(rawStats))
	for i, s := range rawStats {
		samples[i] = stats.LiveSample{Email: s.Email, Up: s.Up, Down: s.Down}
	}

	// Also include all users from config (so they appear even with zero traffic).
	if xrayCfg, err := xrayconfig.Read(cfg.Paths.XrayConfig); err == nil {
		inUsers, _ := xrayconfig.ListUsers(xrayCfg)
		existing := make(map[string]bool, len(samples))
		for _, s := range samples {
			existing[s.Email] = true
		}
		for _, u := range inUsers {
			if e := u.Email(); !existing[e] {
				samples = append(samples, stats.LiveSample{Email: e})
			}
		}
	}

	lockPath := statePath + ".lock"
	for i := 0; i < 50; i++ {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL, 0666)
		if err == nil {
			f.Close()
			defer os.Remove(lockPath)
			goto acquired
		}
		if os.IsExist(err) {
			if stat, err := os.Stat(lockPath); err == nil {
				if time.Since(stat.ModTime()) > 30*time.Second {
					os.Remove(lockPath) // Stale lock
				}
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		return fmt.Errorf("failed to create lock file: %w", err)
	}
	return fmt.Errorf("timeout waiting for stats lock file")

acquired:
	state, err := stats.Load(statePath, cfg.DetailedRetentionSeconds())
	if err != nil {
		return err
	}
	stats.Update(state, samples, cfg.DetailedRetentionSeconds())
	return stats.Save(statePath, state)
}

// ---------------------------------------------------------------------------
// Slave totals collection
// ---------------------------------------------------------------------------

type slaveUserTotal struct {
	Email string `json:"email"`
	Slave int64  `json:"slave"`
}


func collectSlaveTotals() ([]slaveUserTotal, stats.SlaveReportJSON) {
	reg := slaveRegistry(cfg)
	servers, err := reg.Servers()
	if err != nil || len(servers) == 0 {
		return nil, stats.SlaveReportJSON{Enabled: len(servers) > 0}
	}

	report := stats.SlaveReportJSON{Enabled: true, TotalServers: len(servers)}

	type job struct {
		server string
		users  []slaveUserTotal
		ok     bool
	}
	jobs := make(chan job, len(servers))

	cli := slave.NewClient(cfg.SlaveAPI.ConnectTimeout, cfg.SlaveAPI.RequestTimeout, cfg.SlaveAPI.RemotePath)
	srvMap, _ := reg.Servers()

	for name, entry := range srvMap {
		go func(name string, entry slave.Entry) {
			out, err := cli.Call(entry, "cli-stats", map[string]string{"api": "true"})
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
			var totals []slaveUserTotal
			for _, u := range parsed.Users {
				t := u.Total.Combined
				if u.ClusterTotal != nil {
					t = *u.ClusterTotal
				}
				totals = append(totals, slaveUserTotal{Email: u.Email, Slave: t})
			}
			jobs <- job{server: name, users: totals, ok: true}
		}(name, entry)
	}

	combined := make(map[string]int64)
	for range srvMap {
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

	result := make([]slaveUserTotal, 0, len(combined))
	for email, total := range combined {
		result = append(result, slaveUserTotal{Email: email, Slave: total})
	}
	return result, report
}

// ---------------------------------------------------------------------------
// Merge + display
// ---------------------------------------------------------------------------

func mergeWithSlaves(local []stats.CumulativeUser, slaveTotals []slaveUserTotal) []stats.MergedUser {
	slaveMap := make(map[string]int64, len(slaveTotals))
	for _, s := range slaveTotals {
		slaveMap[s.Email] = s.Slave
	}

	localMap := make(map[string]stats.CumulativeUser, len(local))
	for _, u := range local {
		localMap[u.Email] = u
	}

	// Union of all emails.
	all := make(map[string]bool)
	for _, u := range local {
		all[u.Email] = true
	}
	for e := range slaveMap {
		all[e] = true
	}

	result := make([]stats.MergedUser, 0, len(all))
	for email := range all {
		u := localMap[email]
		s := slaveMap[email]
		ct := u.Total.Combined + s
		result = append(result, stats.MergedUser{
			Email:        email,
			Xray:         u.Xray,
			Total:        u.Total,
			Slave:        s,
			ClusterTotal: &ct,
		})
	}
	return result
}

func clusterTotal(u stats.MergedUser) int64 {
	if u.ClusterTotal == nil {
		return 0
	}
	return *u.ClusterTotal
}

func sumField(users []stats.MergedUser, f func(stats.MergedUser) int64) int64 {
	var sum int64
	for _, u := range users {
		sum += f(u)
	}
	return sum
}

func printAllStatsTable(users []stats.MergedUser) {
	cyan := "\033[0;36m"
	green := "\033[1;32m"
	nc := "\033[0m"

	// Only show users with traffic.
	var active []stats.MergedUser
	for _, u := range users {
		if clusterTotal(u) > 0 || u.Slave > 0 {
			active = append(active, u)
		}
	}
	if len(active) == 0 {
		fmt.Println("No traffic data found.")
		return
	}

	fmt.Printf("%s%-28s %12s %12s %12s%s\n", cyan, "User", "Xray", "Slave", "Total", nc)
	fmt.Println("--------------------------------------------------------------")

	var sumX, sumSlave, sumTotal int64
	for _, u := range active {
		fmt.Printf("%-28s %12s %12s %12s\n",
			u.Email,
			stats.HumanBytes(u.Xray.Total),
			stats.HumanBytes(u.Slave),
			stats.HumanBytes(clusterTotal(u)),
		)
		sumX += u.Xray.Total
		sumSlave += u.Slave
		sumTotal += clusterTotal(u)
	}
	fmt.Println("--------------------------------------------------------------")
	fmt.Printf("%-28s %12s %12s %12s\n", "SUBTOTALS",
		stats.HumanBytes(sumX), stats.HumanBytes(sumSlave), stats.HumanBytes(sumTotal))
	fmt.Printf("\n%sGLOBAL TOTAL: %.3f GB (%d users)%s\n",
		green, float64(sumTotal)/1024/1024/1024, len(active), nc)
}

func printUserStatsTable(u stats.MergedUser) {
	fmt.Printf("%-25s %12s %12s\n", "Field", "Direction", "Traffic")
	fmt.Println("-------------------------------------------------------")
	fmt.Printf("%-25s %12s %12s\n", u.Email, "Xray up", stats.HumanBytes(u.Xray.Up))
	fmt.Printf("%-25s %12s %12s\n", u.Email, "Xray down", stats.HumanBytes(u.Xray.Down))
	fmt.Printf("%-25s %12s %12s\n", u.Email, "Slave", stats.HumanBytes(u.Slave))
	fmt.Println("-------------------------------------------------------")
	fmt.Printf("%-25s %12s %12s\n", u.Email, "Total up", stats.HumanBytes(u.Total.Up))
	fmt.Printf("%-25s %12s %12s\n", u.Email, "Total down", stats.HumanBytes(u.Total.Down))
	fmt.Printf("%-25s %12s %12s\n", u.Email, "TOTAL", stats.HumanBytes(clusterTotal(u)))
}

func printJSON(v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		fmt.Println(`{"error":"failed to serialize response"}`)
		return
	}
	fmt.Println(string(data))
}

func nowUTC() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}
