package support_chat

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"xraytool/internal/pluginapi"
)

func TestStore_SupportBanLifecycle(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	const userID = "telegram:123456"

	// Initial check: not banned
	banned, ban, err := s.IsUserBanned(ctx, userID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if banned || ban != nil {
		t.Fatalf("expected not banned, got banned=%v, ban=%+v", banned, ban)
	}

	// 1. Permanent Ban
	created, err := s.BanUser(ctx, userID, "Abusive language in support tickets", "admin-1", nil)
	if err != nil {
		t.Fatalf("failed to ban user: %v", err)
	}
	if created.UserID != userID || created.Reason != "Abusive language in support tickets" || created.BannedBy != "admin-1" {
		t.Fatalf("unexpected ban metadata: %+v", created)
	}

	// Verify encryption at rest in DB
	var rawBan SupportBan
	if err := s.db.Where("user_id_hash = ?", created.UserIDHash).First(&rawBan).Error; err != nil {
		t.Fatalf("failed to find raw ban in db: %v", err)
	}
	if bytes.Contains(rawBan.UserIDCiphertext, []byte(userID)) {
		t.Fatal("user_id plaintext found in ciphertext")
	}
	if bytes.Contains(rawBan.ReasonCiphertext, []byte("Abusive language")) {
		t.Fatal("reason plaintext found in ciphertext")
	}
	if bytes.Contains(rawBan.BannedByCiphertext, []byte("admin-1")) {
		t.Fatal("banned_by plaintext found in ciphertext")
	}

	// Check IsUserBanned returns hydrated info
	banned, ban, err = s.IsUserBanned(ctx, userID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !banned || ban == nil {
		t.Fatal("expected user to be banned")
	}
	if ban.UserID != userID || ban.Reason != "Abusive language in support tickets" || ban.BannedBy != "admin-1" {
		t.Fatalf("hydrated ban data mismatch: %+v", ban)
	}

	// Check GetBan
	retrieved, err := s.GetBan(ctx, userID)
	if err != nil {
		t.Fatalf("failed to get ban: %v", err)
	}
	if retrieved == nil || retrieved.UserID != userID {
		t.Fatalf("expected ban for %s, got %+v", userID, retrieved)
	}

	// List bans
	bans, err := s.ListBans(ctx)
	if err != nil {
		t.Fatalf("failed to list bans: %v", err)
	}
	if len(bans) != 1 || bans[0].UserID != userID {
		t.Fatalf("expected 1 ban for %s, got %d bans", userID, len(bans))
	}

	// 2. Unban
	if err := s.UnbanUser(ctx, userID); err != nil {
		t.Fatalf("failed to unban: %v", err)
	}

	banned, ban, err = s.IsUserBanned(ctx, userID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if banned || ban != nil {
		t.Fatalf("expected not banned after unban, got banned=%v", banned)
	}

	bans, err = s.ListBans(ctx)
	if err != nil {
		t.Fatalf("failed to list bans: %v", err)
	}
	if len(bans) != 0 {
		t.Fatalf("expected 0 bans, got %d", len(bans))
	}
}

func TestStore_SupportBanExpiration(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	const userID = "telegram:999999"

	// Ban with past expiration (already expired)
	past := time.Now().Add(-1 * time.Hour)
	_, err := s.BanUser(ctx, userID, "Temporary test ban", "admin", &past)
	if err != nil {
		t.Fatalf("failed to ban: %v", err)
	}

	banned, ban, err := s.IsUserBanned(ctx, userID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if banned || ban != nil {
		t.Fatalf("expected expired ban to be inactive, got banned=%v", banned)
	}

	// Ban with future expiration (active)
	future := time.Now().Add(2 * time.Hour)
	_, err = s.BanUser(ctx, userID, "Active temporary ban", "admin", &future)
	if err != nil {
		t.Fatalf("failed to update ban: %v", err)
	}

	banned, ban, err = s.IsUserBanned(ctx, userID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !banned || ban == nil {
		t.Fatal("expected future ban to be active")
	}
	if ban.Reason != "Active temporary ban" {
		t.Fatalf("expected updated reason, got %s", ban.Reason)
	}
}

func TestProvider_IsBanned(t *testing.T) {
	s := setupTestStore(t)
	provider := &Provider{db: s.db, crypto: s.crypto}
	ctx := context.Background()
	const userID = "telegram:777"

	isBanned, err := provider.IsBanned(ctx, userID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if isBanned {
		t.Fatal("expected not banned")
	}

	_, err = s.BanUser(ctx, userID, "spam", "admin", nil)
	if err != nil {
		t.Fatalf("failed to ban user: %v", err)
	}

	isBanned, err = provider.IsBanned(ctx, userID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isBanned {
		t.Fatal("expected provider to report user is banned")
	}
}

func TestHTTP_SupportBanClientEnforcement(t *testing.T) {
	ctx := context.Background()
	store := setupTestStore(t)
	p := &Plugin{
		store: store,
		provider: &Provider{db: store.db, crypto: store.crypto},
		cfg: pluginConfig{
			Media: MediaConfig{StoragePath: t.TempDir(), MaxFileSizeMB: 10},
		},
		users: supportUserLookupStub{byEmail: map[string]*pluginapi.User{
			"user@test.local":  {ID: "user-1", IsAdmin: false},
			"admin@test.local": {ID: "admin-1", IsAdmin: true},
		}},
		authMiddleware: testProtectedMiddleware,
		hub:            newHub(nil),
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	const clientUserID = "user@test.local"

	// 1. Not banned: check ban-status
	req := httptest.NewRequest(http.MethodGet, "/api/v1/support/ban-status", nil)
	req.Header.Set("X-API-Key", "test-internal-api-key")
	req.Header.Set("X-User-ID", clientUserID)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ban-status code = %d, want %d", rec.Code, http.StatusOK)
	}
	var statusResp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&statusResp); err != nil {
		t.Fatal(err)
	}
	if statusResp["is_banned"] != false {
		t.Fatalf("expected is_banned=false, got %+v", statusResp)
	}

	// 2. Create conversation while not banned -> OK
	convBody, _ := json.Marshal(map[string]any{
		"subject": "Help needed",
		"message": "Hello support",
	})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/support/conversations", bytes.NewReader(convBody))
	req.Header.Set("X-API-Key", "test-internal-api-key")
	req.Header.Set("X-User-ID", clientUserID)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create conv code = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var convResp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&convResp); err != nil {
		t.Fatal(err)
	}
	convID := convResp["conversation_id"].(string)

	// 3. Admin bans the user
	_, err := store.BanUser(ctx, clientUserID, "Spamming", "admin-1", nil)
	if err != nil {
		t.Fatal(err)
	}

	// 4. Check ban status now -> is_banned: true
	req = httptest.NewRequest(http.MethodGet, "/api/v1/support/ban-status", nil)
	req.Header.Set("X-API-Key", "test-internal-api-key")
	req.Header.Set("X-User-ID", clientUserID)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ban-status code = %d, want %d", rec.Code, http.StatusOK)
	}
	statusResp = nil
	if err := json.NewDecoder(rec.Body).Decode(&statusResp); err != nil {
		t.Fatal(err)
	}
	if statusResp["is_banned"] != true || statusResp["reason"] != "Spamming" {
		t.Fatalf("expected is_banned=true with reason Spamming, got %+v", statusResp)
	}

	// 5. Creating a new conversation when banned -> 403 Forbidden
	req = httptest.NewRequest(http.MethodPost, "/api/v1/support/conversations", bytes.NewReader(convBody))
	req.Header.Set("X-API-Key", "test-internal-api-key")
	req.Header.Set("X-User-ID", clientUserID)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("banned create conv code = %d, want %d", rec.Code, http.StatusForbidden)
	}

	// 6. Sending message to existing conversation when banned -> 403 Forbidden
	msgBody, _ := json.Marshal(map[string]any{
		"text": "Another message",
	})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/support/conversations/"+convID+"/messages", bytes.NewReader(msgBody))
	req.Header.Set("X-API-Key", "test-internal-api-key")
	req.Header.Set("X-User-ID", clientUserID)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("banned create msg code = %d, want %d", rec.Code, http.StatusForbidden)
	}

	// 7. Unban user -> creation works again
	if err := store.UnbanUser(ctx, clientUserID); err != nil {
		t.Fatal(err)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/support/conversations/"+convID+"/messages", bytes.NewReader(msgBody))
	req.Header.Set("X-API-Key", "test-internal-api-key")
	req.Header.Set("X-User-ID", clientUserID)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unbanned create msg code = %d, want %d, body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}

func TestHTTP_AdminBanEndpoints(t *testing.T) {
	store := setupTestStore(t)
	p := &Plugin{
		store: store,
		provider: &Provider{db: store.db, crypto: store.crypto},
		users: supportUserLookupStub{byEmail: map[string]*pluginapi.User{
			"user@test.local":  {ID: "user-1", IsAdmin: false},
			"admin@test.local": {ID: "admin-1", IsAdmin: true},
		}},
		authMiddleware: testProtectedMiddleware,
		hub:            newHub(nil),
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	const targetUser = "user@test.local"

	// 1. Non-admin cannot ban
	banReqBody, _ := json.Marshal(map[string]any{
		"user_id": targetUser,
		"reason":  "Violation of rules",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/support/bans", bytes.NewReader(banReqBody))
	req.Header.Set("X-API-Key", "test-internal-api-key")
	req.Header.Set("X-User-ID", "user@test.local")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin ban code = %d, want %d", rec.Code, http.StatusForbidden)
	}

	// 2. Admin creates ban
	req = httptest.NewRequest(http.MethodPost, "/api/v1/admin/support/bans", bytes.NewReader(banReqBody))
	req.Header.Set("X-API-Key", "test-internal-api-key")
	req.Header.Set("X-User-ID", "admin@test.local")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin ban code = %d, want %d, body: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var createdBan SupportBan
	if err := json.NewDecoder(rec.Body).Decode(&createdBan); err != nil {
		t.Fatal(err)
	}
	if createdBan.UserID != targetUser || createdBan.Reason != "Violation of rules" {
		t.Fatalf("created ban mismatch: %+v", createdBan)
	}

	// 3. Admin gets ban by user_id
	req = httptest.NewRequest(http.MethodGet, "/api/v1/admin/support/bans/"+targetUser, nil)
	req.Header.Set("X-API-Key", "test-internal-api-key")
	req.Header.Set("X-User-ID", "admin@test.local")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin get ban code = %d, want %d", rec.Code, http.StatusOK)
	}

	// 4. Admin lists bans
	req = httptest.NewRequest(http.MethodGet, "/api/v1/admin/support/bans", nil)
	req.Header.Set("X-API-Key", "test-internal-api-key")
	req.Header.Set("X-User-ID", "admin@test.local")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin list bans code = %d, want %d", rec.Code, http.StatusOK)
	}
	var listResp struct {
		Bans []SupportBan `json:"bans"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&listResp); err != nil {
		t.Fatal(err)
	}
	if len(listResp.Bans) != 1 || listResp.Bans[0].UserID != targetUser {
		t.Fatalf("expected 1 ban for %s, got %+v", targetUser, listResp.Bans)
	}

	// 5. Admin deletes ban
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/admin/support/bans/"+targetUser, nil)
	req.Header.Set("X-API-Key", "test-internal-api-key")
	req.Header.Set("X-User-ID", "admin@test.local")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin delete ban code = %d, want %d", rec.Code, http.StatusOK)
	}

	// 6. Admin gets ban after deletion -> 404
	req = httptest.NewRequest(http.MethodGet, "/api/v1/admin/support/bans/"+targetUser, nil)
	req.Header.Set("X-API-Key", "test-internal-api-key")
	req.Header.Set("X-User-ID", "admin@test.local")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("admin get ban after deletion code = %d, want %d", rec.Code, http.StatusNotFound)
	}
}
