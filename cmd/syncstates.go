package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"xraytool/internal/database"
	"xraytool/internal/slave"
	"xraytool/internal/xrayconfig"

	"github.com/spf13/cobra"
)


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

			masterSnap := slave.BuildMasterSnapshot(xrayCfg)

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

// syncSlave compares masterSnap with a slave's current snapshot and issues
// a single batch payload to reconcile the slave to match the master.
func syncSlave(reg *slave.Registry, srvName string, master slave.Snapshot, dryRun bool) {
	out, err := reg.CallOne(srvName, "usersnapshot", map[string]string{})
	if err != nil {
		fmt.Printf("  [FAIL] %s: Could not get snapshot: %v\n", srvName, err)
		return
	}

	var slaveSnap slave.Snapshot
	
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
	masterActive := make(map[string]slave.SnapshotUser, len(master.Active))
	for _, mu := range master.Active {
		masterActive[mu.Email] = mu
	}
	slaveActive := make(map[string]slave.SnapshotUser, len(slaveSnap.Active))
	for _, u := range slaveSnap.Active {
		slaveActive[u.Email] = u
	}

	batch := slave.BatchPayload{
		Add:    []slave.SnapshotUser{},
		Remove: []string{},
	}

	// 1. Add or update users that are on master but missing/different on slave.
	for _, mu := range master.Active {
		su, existsActive := slaveActive[mu.Email]

		if !existsActive {
			batch.Add = append(batch.Add, mu)
			continue
		}

		needsUpdate := false
		if su.UUID != "" && mu.UUID != "" && su.UUID != mu.UUID {
			needsUpdate = true
		}
		if su.Expire != mu.Expire {
			needsUpdate = true
		}
		if limitStr(mu.Limit) != limitStr(su.Limit) {
			needsUpdate = true
		}
		
		if needsUpdate {
			batch.Add = append(batch.Add, mu)
		}
	}

	// 2. Remove users that are active on slave but gone from master.
	for _, su := range slaveSnap.Active {
		_, okActive := masterActive[su.Email]
		if !okActive {
			batch.Remove = append(batch.Remove, su.Email)
		}
	}

	totalOps := len(batch.Add) + len(batch.Remove)
	if totalOps == 0 {
		fmt.Printf("  [OK] %s: Already in sync.\n", srvName)
		return
	}

	if dryRun {
		fmt.Printf("  [DRY-RUN] %s: Would apply %d ops (Add: %d, Rm: %d)\n", 
			srvName, totalOps, len(batch.Add), len(batch.Remove))
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
