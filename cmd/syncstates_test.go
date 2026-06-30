package cmd

import (
	"os"
	"testing"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"
	"xraytool/internal/xrayconfig"
)

func TestSyncMasterUUIDsFromDB(t *testing.T) {
	setupTest(t)
	defer teardownTest()

	// 1. Setup DB
	dbCfg := database.Config{
		Driver:      "sqlite",
		SQLitePath:  "test_xraytool.db",
		AutoMigrate: true,
	}
	if err := database.Init(dbCfg); err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	db := database.DB()
	
	defer func() {
		database.Close()
		os.Remove("test_xraytool.db")
	}()

	// Add user to DB with new UUID
	db.Create(&database.User{ID: "user-1", IsBlocked: false})
	db.Create(&database.Subscription{
		ID:       "sub-1",
		UserID:   "user-1",
		Email:    "test@example.com",
		XrayUUID: "new-uuid-from-db", // THIS is the correct one
	})

	// 2. Setup Xray config with old UUID
	dummyCfg := `{
		"inbounds": [
			{
				"tag": "vless-tcp",
				"protocol": "vless",
				"settings": {
					"clients": [
						{"id": "old-uuid", "email": "test@example.com"}
					]
				}
			}
		]
	}`
	os.WriteFile("test_xray_config.json", []byte(dummyCfg), 0644)
	
	// Override path
	cfg.Paths = appconfig.PathsConf{
		XrayConfig: "test_xray_config.json",
	}

	// Not using xrayCfg directly here

	// 3. Run sync
	changed, err := syncMasterUUIDsFromDB()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !changed {
		t.Errorf("expected changed to be true")
	}

	// 4. Verify config changed on disk
	xrayCfgAfter, _ := xrayconfig.Read("test_xray_config.json")
	userAfter, _ := xrayconfig.FindUser(xrayCfgAfter, "test@example.com")
	
	if userAfter.GetString("id") != "new-uuid-from-db" {
		t.Errorf("expected config id to be 'new-uuid-from-db', got %s", userAfter.GetString("id"))
	}
}

func TestSyncMasterUUIDsFromDB_NoChange(t *testing.T) {
	setupTest(t)
	defer teardownTest()

	dbCfg := database.Config{
		Driver:      "sqlite",
		SQLitePath:  "test_xraytool2.db",
		AutoMigrate: true,
	}
	if err := database.Init(dbCfg); err != nil {
		t.Fatalf("failed to init db: %v", err)
	}
	db := database.DB()
	
	defer func() {
		database.Close()
		os.Remove("test_xraytool2.db")
	}()

	db.Create(&database.User{ID: "user-1", IsBlocked: false})
	db.Create(&database.Subscription{
		ID:       "sub-1",
		UserID:   "user-1",
		Email:    "test@example.com",
		XrayUUID: "perfect-uuid", 
	})

	dummyCfg := `{
		"inbounds": [
			{
				"tag": "vless-tcp",
				"settings": {
					"clients": [
						{"id": "perfect-uuid", "email": "test@example.com"}
					]
				}
			}
		]
	}`
	os.WriteFile("test_xray_config2.json", []byte(dummyCfg), 0644)
	cfg.Paths = appconfig.PathsConf{
		XrayConfig: "test_xray_config2.json",
	}

	// Not using xrayCfg directly here

	changed, err := syncMasterUUIDsFromDB()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if changed {
		t.Errorf("expected changed to be false, config was already perfect")
	}
}
