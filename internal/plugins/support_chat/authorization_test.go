package support_chat

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"xraytool/internal/pluginapi"
)

type supportUserLookupStub struct {
	byEmail map[string]*pluginapi.User
	byTG    map[int64]*pluginapi.User
	byBot   map[string]*pluginapi.User
}

func (s supportUserLookupStub) FindByEmailOrUsername(_ context.Context, email string) (*pluginapi.User, error) {
	if user := s.byEmail[email]; user != nil {
		return user, nil
	}
	return nil, errors.New("user not found")
}

func (s supportUserLookupStub) FindByTelegramID(_ context.Context, telegramID int64) (*pluginapi.User, error) {
	if user := s.byTG[telegramID]; user != nil {
		return user, nil
	}
	return nil, errors.New("user not found")
}

func (s supportUserLookupStub) FindByPlatformID(_ context.Context, platform, id string) (*pluginapi.User, error) {
	if platform == "bot" {
		if user := s.byBot[id]; user != nil {
			return user, nil
		}
	}
	return nil, errors.New("user not found")
}

func testProtectedMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "test-internal-api-key" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func TestAttachmentDownloadRequiresTrustedRequestAndServerSideAdminRole(t *testing.T) {
	ctx := context.Background()
	store := setupTestStore(t)
	mediaRoot := t.TempDir()
	const (
		ownerID  = "victim@example.test"
		content  = "private support attachment"
		storage  = "storage-key"
		attachID = "attachment-id"
	)

	conversation, err := store.CreateConversation(ctx, ownerID, "private ticket")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateAttachment(ctx, attachID, storage, ownerID, "private.txt", "text/plain", int64(len(content))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateMessage(ctx, conversation.ID, "client", "message", []string{attachID}); err != nil {
		t.Fatal(err)
	}

	file, err := os.OpenFile(attachmentStoragePath(mediaRoot, storage), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Crypto().EncryptAttachmentStream(storage, file, bytes.NewBufferString(content)); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	p := &Plugin{
		store: store,
		cfg:   pluginConfig{Media: MediaConfig{StoragePath: mediaRoot}},
		users: supportUserLookupStub{byEmail: map[string]*pluginapi.User{
			"attacker@example.test": {ID: "attacker", IsAdmin: false},
			"admin@example.test":    {ID: "admin", IsAdmin: true},
		}},
		authMiddleware: testProtectedMiddleware,
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	request := func(userID, apiKey string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/v1/support/attachments/"+attachID+"/download?admin=true", nil)
		req.Header.Set("X-User-ID", userID)
		req.Header.Set("X-API-Key", apiKey)
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)
		return res
	}

	if res := request("attacker@example.test", ""); res.Code != http.StatusUnauthorized {
		t.Fatalf("direct request status = %d, want %d", res.Code, http.StatusUnauthorized)
	}
	if res := request("attacker@example.test", "test-internal-api-key"); res.Code != http.StatusForbidden {
		t.Fatalf("non-admin ?admin=true status = %d, want %d", res.Code, http.StatusForbidden)
	}
	if res := request("admin@example.test", "test-internal-api-key"); res.Code != http.StatusOK || res.Body.String() != content {
		t.Fatalf("admin download = status %d body %q, want status %d body %q", res.Code, res.Body.String(), http.StatusOK, content)
	}
}

func TestAdminSupportRoutesRequireServerSideAdminRole(t *testing.T) {
	p := &Plugin{
		store: setupTestStore(t),
		users: supportUserLookupStub{byEmail: map[string]*pluginapi.User{
			"client@example.test": {ID: "client", IsAdmin: false},
			"admin@example.test":  {ID: "admin", IsAdmin: true},
		}},
		authMiddleware: testProtectedMiddleware,
	}
	mux := http.NewServeMux()
	p.RegisterRoutes(mux)

	for _, tc := range []struct {
		name   string
		userID string
		status int
	}{
		{name: "client", userID: "client@example.test", status: http.StatusForbidden},
		{name: "admin", userID: "admin@example.test", status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/support/conversations", nil)
			req.Header.Set("X-API-Key", "test-internal-api-key")
			req.Header.Set("X-User-ID", tc.userID)
			res := httptest.NewRecorder()
			mux.ServeHTTP(res, req)
			if res.Code != tc.status {
				t.Fatalf("status = %d, want %d", res.Code, tc.status)
			}
		})
	}
}
