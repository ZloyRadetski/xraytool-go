package subscription

import (
	"encoding/json"
	"os"
	"testing"
)

func TestCacheManager_UpdateDeviceState(t *testing.T) {
	_, cfg, cleanup := setupTestEnv(t)
	defer cleanup()

	// Initialize CacheManager
	cm := NewCacheManager(cfg)
	cm.Refresh()

	// 1. Initial State - No devices
	if cm.HasDeviceHistory("sub1", "uuid-1") {
		t.Fatalf("Expected no device history initially")
	}

	// 2. Add first device
	limitReached, err := cm.UpdateDeviceState("sub1", "uuid-1", "hwid-a", 2, "iPhone", "iOS", "16.0", "Shadowrocket")
	if err != nil {
		t.Fatalf("UpdateDeviceState failed: %v", err)
	}
	if limitReached {
		t.Fatalf("Expected limit NOT to be reached (1/2)")
	}

	// Check if history is present
	if !cm.HasDeviceHistory("sub1", "uuid-1") {
		t.Fatalf("Expected device history after first update")
	}

	// 3. Add second device
	limitReached, err = cm.UpdateDeviceState("sub1", "uuid-1", "hwid-b", 2, "MacBook", "macOS", "13.0", "V2rayU")
	if err != nil {
		t.Fatalf("UpdateDeviceState failed: %v", err)
	}
	if limitReached {
		t.Fatalf("Expected limit NOT to be reached (2/2)")
	}

	// 4. Try adding third device (Should hit limit)
	limitReached, err = cm.UpdateDeviceState("sub1", "uuid-1", "hwid-c", 2, "Android", "Android", "12", "v2rayNG")
	if err != nil {
		t.Fatalf("UpdateDeviceState failed: %v", err)
	}
	if !limitReached {
		t.Fatalf("Expected limit TO BE reached (3/2)")
	}

	// 5. Update existing device (Should NOT hit limit, should just update RequestCount/LastSeen)
	limitReached, err = cm.UpdateDeviceState("sub1", "uuid-1", "hwid-a", 2, "iPhone", "iOS", "16.1", "Shadowrocket")
	if err != nil {
		t.Fatalf("UpdateDeviceState failed: %v", err)
	}
	if limitReached {
		t.Fatalf("Expected limit NOT to be reached when updating existing device")
	}

	// 6. Verify Flush
	cm.FlushDeviceState()

	// Read raw file to verify
	data, err := os.ReadFile(resolveDeviceStatePath(cfg.Paths.DevicesState))
	if err != nil {
		t.Fatalf("Failed to read flushed devices_state.json: %v", err)
	}
	var state DeviceState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("Failed to unmarshal flushed state: %v", err)
	}

	clientKey := "sub1.txt"
	if state.Clients[clientKey] == nil {
		t.Fatalf("Expected clientKey %q to not be nil in saved state", clientKey)
	}
	if len(state.Clients[clientKey].Devices) != 2 {
		t.Fatalf("Expected exactly 2 devices saved to disk, got %d", len(state.Clients[clientKey].Devices))
	}

	// Check if VerOs was updated for hwid-a
	for _, d := range state.Clients[clientKey].Devices {
		if d.Hwid == "hwid-a" {
			if d.VerOs != "16.1" {
				t.Fatalf("Expected VerOs to be updated to 16.1, got %s", d.VerOs)
			}
			if d.RequestCount != 2 {
				t.Fatalf("Expected RequestCount 2, got %d", d.RequestCount)
			}
		}
	}
}

func TestDeviceState_DeduplicationAndVariants(t *testing.T) {
	_, cfg, cleanup := setupTestEnv(t)
	defer cleanup()

	// Write a messy state file with legacy variant formats
	messyState := `{
		"clients": {
			"sub2": {
				"devices": [
					{"hwid": "hwid1", "request_count": 1, "device_model": "PhoneA"}
				]
			},
			"uuid-2.txt": {
				"devices": [
					{"hwid": "hwid1", "request_count": 2, "device_model": "PhoneA-Updated"},
					{"hwid": "hwid2", "request_count": 1, "device_model": "PhoneB"}
				]
			}
		}
	}`
	os.WriteFile(resolveDeviceStatePath(cfg.Paths.DevicesState), []byte(messyState), 0644)

	cm := NewCacheManager(cfg)
	// Trigger load and deduplication
	cm.UpdateDeviceState("sub2", "uuid-2", "hwid3", 10, "PhoneC", "OS", "1", "UA")

	cm.FlushDeviceState()

	// Verify deduplication
	data, _ := os.ReadFile(resolveDeviceStatePath(cfg.Paths.DevicesState))
	var state DeviceState
	json.Unmarshal(data, &state)

	// "sub2.txt" should be the canonical key, old variants should be deleted
	if state.Clients["sub2"] != nil || state.Clients["uuid-2.txt"] != nil {
		t.Fatalf("Expected variants to be cleaned up")
	}

	cd := state.Clients["sub2.txt"]
	if cd == nil {
		t.Fatalf("Expected canonical key 'sub2.txt' to be created")
	}
	if len(cd.Devices) != 3 {
		t.Fatalf("Expected 3 deduplicated devices, got %d", len(cd.Devices))
	}

	// Verify hwid1 was merged
	for _, d := range cd.Devices {
		if d.Hwid == "hwid1" {
			if d.RequestCount != 3 {
				t.Fatalf("Expected merged RequestCount 3 (1+2), got %d", d.RequestCount)
			}
			if d.DeviceModel != "PhoneA-Updated" {
				t.Fatalf("Expected merged DeviceModel 'PhoneA-Updated', got %s", d.DeviceModel)
			}
		}
	}
}
