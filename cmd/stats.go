package cmd

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"xraytool/internal/stats"

	"github.com/spf13/cobra"
)

func statsCmd(deps *AppDeps) *cobra.Command {
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

			statePath := deps.Cfg.Paths.StatsState
			if inferredMode {
				statePath = deps.Cfg.Paths.InferredStats
			}

			svc := stats.NewService(stats.Config{
				IsMaster:                 deps.Cfg.IsMaster(),
				StatsStatePath:           deps.Cfg.Paths.StatsState,
				InferredStatsPath:        deps.Cfg.Paths.InferredStats,
				DetailedRetentionSeconds: deps.Cfg.DetailedRetentionSeconds(),
			}, deps.Engine, deps.ClusterProvider)
			merged, slaveReport, err := svc.GenerateClusterStats(inferredMode, statePath)
			if err != nil {
				if apiMode {
					printJSON(map[string]interface{}{"ok": false, "error": "STATS_UPDATE_FAILED", "message": err.Error()})
				} else {
					p.Warn("Stats update failed: %v", err)
				}
				return nil
			}

			if emailFilter != "" {
				found := false
				for _, u := range merged {
					if u.Email == emailFilter {
						found = true //nolint:ineffassign //nolint:ineffassign
						if apiMode {
							printJSON(map[string]interface{}{
								"ok":           true,
								"generated_at": nowUTC(),
								"user": map[string]interface{}{
									"email": u.Email,
									"xray": map[string]int64{
										"up":    u.Xray.Up,
										"down":  u.Xray.Down,
										"total": u.Xray.Total,
									},
									"total": map[string]int64{
										"up":       u.Total.Up,
										"down":     u.Total.Down,
										"combined": u.Total.Combined,
									},
									"slave":         u.Slave,
									"cluster_total": u.ClusterTotal,
								},
								"slave_report": map[string]interface{}{
									"enabled":        slaveReport.Enabled,
									"total_servers":  slaveReport.TotalServers,
									"ok_servers":     slaveReport.OKServers,
									"failed_servers": slaveReport.FailedServers,
								},
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
					"slave_report": map[string]interface{}{
						"enabled":        slaveReport.Enabled,
						"total_servers":  slaveReport.TotalServers,
						"ok_servers":     slaveReport.OKServers,
						"failed_servers": slaveReport.FailedServers,
					},
					"users": merged,
					"totals": map[string]interface{}{
						"xray":    map[string]int64{"up": sumField(merged, func(u stats.MergedUser) int64 { return u.Xray.Up }), "down": sumField(merged, func(u stats.MergedUser) int64 { return u.Xray.Down }), "total": xTotal},
						"slave":   map[string]int64{"total": slaveTotal},
						"cluster": map[string]int64{"combined": clTotal},
					},
				})
				return nil
			}

			// Print slave status.
			if deps.Cfg.IsMaster() && slaveReport.TotalServers > 0 {
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
