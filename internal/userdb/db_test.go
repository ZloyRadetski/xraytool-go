package userdb

import (
	"path/filepath"
	"testing"
)

func TestDB(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "limited_users.db")
	db := New(tmpFile)

	limit1 := 1024.0
	e1 := Entry{Email: "user1@example.com", Subfile: "sub1.txt", Limit: &limit1}
	e2 := Entry{Email: "user2@example.com", Subfile: "sub2.txt", Limit: nil}

	// Test Upsert
	err := db.Upsert(e1)
	if err != nil {
		t.Fatalf("Upsert e1 error: %v", err)
	}
	err = db.Upsert(e2)
	if err != nil {
		t.Fatalf("Upsert e2 error: %v", err)
	}

	// Test Exists
	exists, err := db.Exists("user1@example.com")
	if err != nil || !exists {
		t.Errorf("Exists(user1) expected true, got %v, err %v", exists, err)
	}

	exists, err = db.Exists("nonexistent@example.com")
	if err != nil || exists {
		t.Errorf("Exists(nonexistent) expected false, got %v, err %v", exists, err)
	}

	// Test Get
	get1, err := db.Get("user1@example.com")
	if err != nil || get1 == nil {
		t.Fatalf("Get(user1) error %v, got %v", err, get1)
	}
	if get1.Subfile != "sub1.txt" || get1.Limit == nil || *get1.Limit != limit1 {
		t.Errorf("Get(user1) returned invalid entry: %+v", get1)
	}

	get2, err := db.Get("user2@example.com")
	if err != nil || get2 == nil {
		t.Fatalf("Get(user2) error %v, got %v", err, get2)
	}
	if get2.Limit != nil {
		t.Errorf("Get(user2) expected nil limit, got %v", *get2.Limit)
	}

	// Test UpdateLimit
	limit2 := 2048.0
	err = db.UpdateLimit("user2@example.com", &limit2)
	if err != nil {
		t.Fatalf("UpdateLimit error: %v", err)
	}

	get2Updated, _ := db.Get("user2@example.com")
	if get2Updated.Limit == nil || *get2Updated.Limit != limit2 {
		t.Errorf("Get(user2) after UpdateLimit returned invalid entry: %+v", get2Updated)
	}

	// Test All
	all, err := db.All()
	if err != nil {
		t.Fatalf("All error: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("All returned %d entries, expected 2", len(all))
	}

	// Test Remove
	err = db.Remove("user1@example.com")
	if err != nil {
		t.Fatalf("Remove error: %v", err)
	}

	exists, _ = db.Exists("user1@example.com")
	if exists {
		t.Errorf("Exists(user1) expected false after Remove")
	}

	allAfterRemove, _ := db.All()
	if len(allAfterRemove) != 1 {
		t.Errorf("All after remove returned %d entries, expected 1", len(allAfterRemove))
	}
}
