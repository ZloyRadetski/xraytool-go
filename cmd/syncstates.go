package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

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

// syncMasterUUIDsFromDB cross-references the loaded Xray config with the DB and updates UUIDs if mismatched.
func syncMasterUUIDsFromDB(xrayCfg xrayconfig.RawConfig) (bool, error) {
	db := database.DB()
	if db == nil {
		return false, fmt.Errorf("database not initialized")
	}

	var subs []database.Subscription
	if err := db.Find(&subs).Error; err != nil {
		return false, fmt.Errorf("failed to load subscriptions: %w", err)
	}

	updatedCount := 0
	for _, sub := range subs {
		if sub.Email == "" || sub.XrayUUID == "" {
			continue
		}
		
		client, err := xrayconfig.FindUser(xrayCfg, sub.Email)
		if err != nil || client == nil {
			continue
		}

		if client.GetString("id") != sub.XrayUUID {
			err = xrayconfig.UpdateStringField(xrayCfg, sub.Email, "id", sub.XrayUUID)
			if err == nil {
				updatedCount++
			}
		}
	}

	if updatedCount > 0 {
		if err := xrayconfig.Write(cfg.Paths.XrayConfig, xrayCfg); err != nil {
			return false, fmt.Errorf("failed to save xray config: %w", err)
		}
		systemctlRestart("xray")
		return true, nil
	}

	return false, nil
}

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
			// Self-heal: Sync UUIDs from Database to Master Xray config before building snapshot
			changed, err := syncMasterUUIDsFromDB(xrayCfg)
			if err != nil {
				fmt.Printf("ERROR|syncing UUIDs from DB: %v\n", err)
				// non-fatal, continue anyway
			} else if changed {
				fmt.Println("INFO|Self-healing complete. Reloading xray config...")
				xrayCfg, _ = xrayconfig.Read(cfg.Paths.XrayConfig)
			}

			masterSnap := buildMasterSnapshot(xrayCfg)

			reg := slaveRegistry(cfg)
			servers, err := reg.Servers()
			if err != nil || len(servers) == 0 {
				fmt.Println("No slave servers configured.")
				return
			}

			var wg sync.WaitGroup
			for srvName := range servers {
				wg.Add(1)
				go func(name string) {
					defer wg.Done()
					syncSlave(reg, name, masterSnap, dryRun)
				}(srvName)
			}
			wg.Wait()
			fmt.Println("All slaves synchronized.")
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
// syncSlave compares masterSnap with a slave's current snapshot and issues
// a single batch payload to reconcile the slave to match the master.
func syncSlave(reg *slave.Registry, srvName string, master Snapshot, dryRun bool) {
	out, err := reg.CallOne(srvName, "usersnapshot", map[string]string{})
	if err != nil {
		fmt.Printf("  [FAIL] %s: Could not get snapshot: %v\n", srvName, err)
		return
	}

	var slaveSnap Snapshot
	
	firstBrace := strings.Index(out, "{")
	lastBrace := strings.LastIndex(out, "}")
	if firstBrace == -1 || lastBrace == -1 || firstBrace > lastBrace {
		fmt.Printf("  [FAIL] %s: No JSON object found in snapshot output\n", srvName)
		return
	}
	
	jsonStr := out[firstBrace : lastBrace+1]
	if err := json.Unmarshal([]byte(jsonStr), &slaveSnap); err != nil {
		fmt.Printf("  [FAIL] %s: Could not parse snapshot JSON: %v\n", srvName, err)
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

	batch := BatchPayload{
		Add:    []SnapshotUser{},
		Remove: []string{},
		Limit:  []SnapshotLimited{},
	}

	// 1. Add or update users that are on master but missing/different on slave.
	for _, mu := range master.Active {
		su, existsActive := slaveActive[mu.Email]
		_, existsLimited := slaveLimited[mu.Email]

		if existsLimited {
			var zero float64 = 0
			batch.Limit = append(batch.Limit, SnapshotLimited{Email: mu.Email, Limit: &zero})
			batch.Add = append(batch.Add, mu)
			continue
		}

		if !existsActive {
			batch.Add = append(batch.Add, mu)
			continue
		}

		needsUpdate := false
		if su.UUID != "" && mu.UUID != "" && su.UUID != mu.UUID {
			needsUpdate = true
		}
		if su.Expire != mu.Expire && mu.Expire != "" {
			needsUpdate = true
		}
		if needsUpdate {
			batch.Add = append(batch.Add, mu)
		}

		if limitStr(mu.Limit) != limitStr(su.Limit) {
			batch.Limit = append(batch.Limit, SnapshotLimited{Email: mu.Email, Subfile: mu.Subfile, Limit: mu.Limit})
		}
	}

	// 2. Remove users that are active on slave but gone from master.
	for _, su := range slaveSnap.Active {
		_, okActive := masterActive[su.Email]
		_, okLimited := masterLimited[su.Email]
		if !okActive && !okLimited {
			batch.Remove = append(batch.Remove, su.Email)
		} else if !okActive && okLimited {
			var zero float64 = 0
			limitVal := &zero
			if l, ok := masterLimited[su.Email]; ok && l.Limit != nil {
				limitVal = l.Limit
			}
			batch.Limit = append(batch.Limit, SnapshotLimited{Email: su.Email, Subfile: su.Subfile, Limit: limitVal})
			batch.Remove = append(batch.Remove, su.Email)
		}
	}

	// 3. Clean up limited users on slave that are gone from master
	for _, su := range slaveSnap.Limited {
		_, okActive := masterActive[su.Email]
		_, okLimited := masterLimited[su.Email]
		if !okActive && !okLimited {
			batch.Remove = append(batch.Remove, su.Email)
			var zero float64 = 0
			batch.Limit = append(batch.Limit, SnapshotLimited{Email: su.Email, Limit: &zero})
		}
	}

	totalOps := len(batch.Add) + len(batch.Remove) + len(batch.Limit)
	if totalOps == 0 {
		fmt.Printf("  [OK] %s: Already in sync.\n", srvName)
		return
	}

	if dryRun {
		fmt.Printf("  [DRY-RUN] %s: Would apply %d ops (Add: %d, Rm: %d, Lim: %d)\n", 
			srvName, totalOps, len(batch.Add), len(batch.Remove), len(batch.Limit))
		return
	}

	payloadBytes, _ := json.Marshal(batch)
	outBatch, errBatch := reg.CallOne(srvName, "apply-batch", map[string]string{
		"payload": string(payloadBytes),
	})
	
	if errBatch != nil {
		fmt.Printf("  [FAIL] %s: Batch apply failed: %v\n", srvName, errBatch)
	} else {
		fmt.Printf("  [OK] %s: Applied %d operations. Response: %s\n", srvName, totalOps, outBatch)
	}
}

func limitStr(l *float64) string {
	if l == nil {
		return ""
	}
	return fmt.Sprintf("%.0f", *l)
}
