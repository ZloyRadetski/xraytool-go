package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"xraytool/internal/database"
	"xraytool/internal/slave"
	"xraytool/internal/userdb"
	"xraytool/internal/xrayconfig"

	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------------------
// usersnapshot — serialises the current node's user state to JSON
// ---------------------------------------------------------------------------

// Snapshot is the JSON payload returned by usersnapshot.
type Snapshot struct {
	Active  []SnapshotUser    `json:"active"`
	Limited []SnapshotLimited `json:"limited"`
}

type SnapshotUser struct {
	Email   string   `json:"email"`
	UUID    string   `json:"uuid,omitempty"`
	Auth    string   `json:"auth,omitempty"`
	Subfile string   `json:"subfile"`
	Expire  string   `json:"expire"`
	Limit   *float64 `json:"limit,omitempty"`
}

type SnapshotLimited struct {
	Email   string   `json:"email"`
	Subfile string   `json:"subfile"`
	Limit   *float64 `json:"limit,omitempty"`
}

func getBlockedEmails() map[string]bool {
	db := database.DB()
	var blockedSubs []database.Subscription
	// Only run this query if the database connection exists.
	if db != nil {
		db.Joins("JOIN users ON users.id = subscriptions.user_id").
			Where("users.is_blocked = ?", true).
			Find(&blockedSubs)
	}

	blockedMap := make(map[string]bool)
	for _, sub := range blockedSubs {
		blockedMap[sub.Email] = true
	}
	return blockedMap
}

func userSnapshotCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "usersnapshot",
		Short: "Dump current user state as JSON (used by syncstates)",
		Run: func(cmd *cobra.Command, _ []string) {
			requireRoot()

			xrayCfg, err := xrayconfig.Read(cfg.Paths.XrayConfig)
			if err != nil {
				printJSON(map[string]interface{}{"ok": false, "error": err.Error()})
				return
			}

			blockedMap := getBlockedEmails()

			users, _ := xrayconfig.ListUsers(xrayCfg)
			active := make([]SnapshotUser, 0, len(users))
			for _, u := range users {
				if blockedMap[u.Email()] {
					continue
				}
				authVal := u.GetString("auth")
				su := SnapshotUser{
					Email:   u.Email(),
					UUID:    u.GetString("id"),
					Auth:    authVal,
					Subfile: u.GetString("subfile"),
					Expire:  u.GetString("expire"),
				}
				if lv, ok := u.GetNumber("limit"); ok {
					su.Limit = &lv
				}
				active = append(active, su)
			}

			db := userdb.New(cfg.Paths.LimitedDB)
			limited, _ := db.All()
			sl := make([]SnapshotLimited, 0, len(limited))
			for _, e := range limited {
				if blockedMap[e.Email] {
					continue
				}
				sl = append(sl, SnapshotLimited{
					Email:   e.Email,
					Subfile: e.Subfile,
					Limit:   e.Limit,
				})
			}

			snap := Snapshot{Active: active, Limited: sl}
			data, _ := json.Marshal(snap)
			fmt.Println(string(data))
		},
	}
	return cmd
}

// ---------------------------------------------------------------------------
// syncstates — reconcile master config with each slave's state
// ---------------------------------------------------------------------------

func syncStatesCmd() *cobra.Command {
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "syncstates",
		Short: "Synchronise user state from master to all slaves",
		Run: func(cmd *cobra.Command, _ []string) {
			requireRoot()

			if !cfg.IsMaster() {
				fmt.Println("ERROR|syncstates can only run on master node")
				return
			}

			// Build master snapshot.
			xrayCfg, err := xrayconfig.Read(cfg.Paths.XrayConfig)
			if err != nil {
				fmt.Printf("ERROR|reading xray config: %v\n", err)
				return
			}
			masterSnap := buildMasterSnapshot(xrayCfg)

			reg := slaveRegistry(cfg)
			servers, err := reg.Servers()
			if err != nil || len(servers) == 0 {
				fmt.Println("No slave servers configured.")
				return
			}

			cyan := "\033[0;36m"
			nc := "\033[0m"

			for srvName := range servers {
				fmt.Printf("\n%s--- Syncing: %s ---%s\n", cyan, srvName, nc)
				syncSlave(reg, srvName, masterSnap, dryRun)
			}
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print changes without applying them")
	return cmd
}

// buildMasterSnapshot returns the current master user state.
func buildMasterSnapshot(xrayCfg xrayconfig.RawConfig) Snapshot {
	blockedMap := getBlockedEmails()

	users, _ := xrayconfig.ListUsers(xrayCfg)
	active := make([]SnapshotUser, 0, len(users))
	for _, u := range users {
		if blockedMap[u.Email()] {
			continue
		}
		authVal := u.GetString("auth")
		su := SnapshotUser{
			Email:   u.Email(),
			UUID:    u.GetString("id"),
			Auth:    authVal,
			Subfile: u.GetString("subfile"),
			Expire:  u.GetString("expire"),
		}
		if lv, ok := u.GetNumber("limit"); ok {
			su.Limit = &lv
		}
		active = append(active, su)
	}

	db := userdb.New(cfg.Paths.LimitedDB)
	limited, _ := db.All()
	sl := make([]SnapshotLimited, 0, len(limited))
	for _, e := range limited {
		if blockedMap[e.Email] {
			continue
		}
		sl = append(sl, SnapshotLimited{
			Email:   e.Email,
			Subfile: e.Subfile,
			Limit:   e.Limit,
		})
	}

	return Snapshot{Active: active, Limited: sl}
}

// syncSlave compares masterSnap with a slave's current snapshot and issues
// the minimum set of commands to reconcile the slave to match the master.
func syncSlave(reg *slave.Registry, srvName string, master Snapshot, dryRun bool) {
	out, err := reg.CallOne(srvName, "usersnapshot", map[string]string{})
	if err != nil {
		fmt.Printf("  [FAIL] Could not get snapshot: %v\n", err)
		return
	}

	var slaveSnap Snapshot
	
	// Safely extract JSON payload to ignore any leading warnings or text
	firstBrace := strings.Index(out, "{")
	lastBrace := strings.LastIndex(out, "}")
	if firstBrace == -1 || lastBrace == -1 || firstBrace > lastBrace {
		fmt.Printf("  [FAIL] No JSON object found in snapshot output\n  raw: %s\n", out)
		return
	}
	
	jsonStr := out[firstBrace : lastBrace+1]
	if err := json.Unmarshal([]byte(jsonStr), &slaveSnap); err != nil {
		fmt.Printf("  [FAIL] Could not parse snapshot JSON: %v\n  raw: %s\n", err, out)
		return
	}

	// Build lookup maps.
	slaveActive := make(map[string]SnapshotUser, len(slaveSnap.Active))
	for _, u := range slaveSnap.Active {
		slaveActive[u.Email] = u
	}
	slaveLimited := make(map[string]SnapshotLimited, len(slaveSnap.Limited))
	for _, l := range slaveSnap.Limited {
		slaveLimited[l.Email] = l
	}
	masterActive := make(map[string]SnapshotUser, len(master.Active))
	for _, u := range master.Active {
		masterActive[u.Email] = u
	}
	masterLimited := make(map[string]SnapshotLimited, len(master.Limited))
	for _, l := range master.Limited {
		masterLimited[l.Email] = l
	}

	var ops []string

	// 1. Add or update users that are on master but missing/different on slave.
	for _, mu := range master.Active {
		su, existsActive := slaveActive[mu.Email]
		_, existsLimited := slaveLimited[mu.Email]

		if existsLimited {
			// Master active, slave has blocked → unlimit on slave.
			op := fmt.Sprintf("unlimit %s", mu.Email)
			ops = append(ops, op)
			if !dryRun {
				params := map[string]string{
					"email": mu.Email, "uuid": mu.UUID,
					"subfile": mu.Subfile, "expire": mu.Expire,
					"auth": mu.Auth,
				}
				if mu.Limit != nil {
					params["limit"] = fmt.Sprintf("%.0f", *mu.Limit)
				}
				out, err := reg.CallOne(srvName, "unlimit", params)
				printSyncResult(op, out, err)
			}
			continue
		}

		if !existsActive {
			// Master has user, slave doesn't → newuser on slave.
			op := fmt.Sprintf("newuser %s", mu.Email)
			ops = append(ops, op)
			if !dryRun {
				params := map[string]string{
					"email": mu.Email, "uuid": mu.UUID,
					"subfile": mu.Subfile, "expire": mu.Expire,
					"auth": mu.Auth,
				}
				if mu.Limit != nil {
					params["limit"] = fmt.Sprintf("%.0f", *mu.Limit)
				}
				out, err := reg.CallOne(srvName, "newuser", params)
				printSyncResult(op, out, err)
			}
			continue
		}

		// Both have the user; check for diffs.
		if su.UUID != "" && mu.UUID != "" && su.UUID != mu.UUID {
			// UUID mismatch — remove and re-add.
			op := fmt.Sprintf("repair-uuid %s (slave=%s master=%s)", mu.Email, su.UUID[:8], mu.UUID[:8])
			ops = append(ops, op)
			if !dryRun {
				rmOut, rmErr := reg.CallOne(srvName, "rmuser", map[string]string{"email": mu.Email})
				if rmErr != nil {
					printSyncResult("repair-uuid:rmuser "+mu.Email, rmOut, rmErr)
					// Don't proceed with newuser if rmuser failed — would create a duplicate.
				} else {
					params := map[string]string{
						"email": mu.Email, "uuid": mu.UUID,
						"subfile": mu.Subfile, "expire": mu.Expire,
						"auth": mu.Auth,
					}
					if mu.Limit != nil {
						params["limit"] = fmt.Sprintf("%.0f", *mu.Limit)
					}
					out, err := reg.CallOne(srvName, "newuser", params)
					printSyncResult(op, out, err)
				}
			}
		}
		if su.Expire != mu.Expire && mu.Expire != "" {
			op := fmt.Sprintf("setexpire %s → %s", mu.Email, mu.Expire)
			ops = append(ops, op)
			if !dryRun {
				out, err := reg.CallOne(srvName, "setexpire", map[string]string{
					"email": mu.Email, "expire": mu.Expire,
				})
				printSyncResult(op, out, err)
			}
		}

		masterLimitStr := limitStr(mu.Limit)
		slaveLimitStr := limitStr(su.Limit)
		if masterLimitStr != "" && masterLimitStr != slaveLimitStr {
			op := fmt.Sprintf("setlimit %s → %s", mu.Email, masterLimitStr)
			ops = append(ops, op)
			if !dryRun {
				out, err := reg.CallOne(srvName, "setlimit", map[string]string{
					"email": mu.Email, "limit": masterLimitStr,
				})
				printSyncResult(op, out, err)
			}
		}
	}

	// 2. Remove users that are active on slave but gone from master.
	for _, su := range slaveSnap.Active {
		_, okActive := masterActive[su.Email]
		_, okLimited := masterLimited[su.Email]
		if !okActive && !okLimited {
			op := fmt.Sprintf("rmuser %s (not on master)", su.Email)
			ops = append(ops, op)
			if !dryRun {
				out, err := reg.CallOne(srvName, "rmuser", map[string]string{"email": su.Email})
				printSyncResult(op, out, err)
			}
		} else if !okActive && okLimited {
			op := fmt.Sprintf("limit %s (limited on master)", su.Email)
			ops = append(ops, op)
			if !dryRun {
				out, err := reg.CallOne(srvName, "limit", map[string]string{"email": su.Email})
				printSyncResult(op, out, err)
			}
		}
	}

	// 3. Clean up limited users on slave that are gone from master
	for _, su := range slaveSnap.Limited {
		_, okActive := masterActive[su.Email]
		_, okLimited := masterLimited[su.Email]
		if !okActive && !okLimited {
			op := fmt.Sprintf("rmuser %s (was limited on slave, not on master)", su.Email)
			ops = append(ops, op)
			if !dryRun {
				out, err := reg.CallOne(srvName, "rmuser", map[string]string{"email": su.Email})
				printSyncResult(op, out, err)
			}
		}
	}

	if len(ops) == 0 {
		fmt.Println("  Already in sync.")
		return
	}
	if dryRun {
		fmt.Printf("  Would apply %d operations:\n", len(ops))
		for _, op := range ops {
			fmt.Printf("    • %s\n", op)
		}
	} else {
		fmt.Printf("  Applied %d operations.\n", len(ops))
	}
}

func printSyncResult(op, out string, err error) {
	if err != nil {
		fmt.Printf("    [FAIL] %s: %v\n", op, err)
	} else {
		result := "OK"
		if strings.Contains(strings.ToUpper(out), "ERROR") {
			result = "FAIL: " + out
		}
		fmt.Printf("    [%s] %s\n", result, op)
	}
}

func limitStr(l *float64) string {
	if l == nil {
		return ""
	}
	return fmt.Sprintf("%.0f", *l)
}
