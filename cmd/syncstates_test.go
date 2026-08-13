package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"xraytool/internal/appconfig"
	"xraytool/internal/database"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestSyncstatesDryRunWorksWithoutReplicationService(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&database.User{}, &database.Subscription{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&database.Subscription{ID: "sub", UserID: "user", Email: "user@example.test", UUID: "uuid", Status: "active"}).Error; err != nil {
		t.Fatal(err)
	}

	deps := &AppDeps{Cfg: &appconfig.Config{Mode: "master"}, Registry: database.NewRegistry(db)}
	cmd := syncStatesCmd(deps)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--dry-run"})

	originalGOOS := currentGOOS
	currentGOOS = "windows"
	t.Cleanup(func() { currentGOOS = originalGOOS })
	cmd.SetContext(context.Background())
	if err := cmd.Execute(); err != nil {
		t.Fatalf("dry-run without replication: %v", err)
	}
	if !strings.Contains(output.String(), "Would reconcile 1 users") {
		t.Fatalf("unexpected output: %q", output.String())
	}
}
