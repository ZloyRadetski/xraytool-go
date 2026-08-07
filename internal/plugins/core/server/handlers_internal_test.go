package server_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHandleInternalXraySync_SyncKeys(t *testing.T) {
	r := newTestRouter(t)

	// Configure a temp keys file path in the router config
	tmpDir := t.TempDir()
	keysPath := filepath.Join(tmpDir, "reality.keys")
	r.Config().Reality.KeysFilepath = keysPath

	// 1. Sending sync-keys request
	payload := `{"action":"sync-keys", "payload":"{\"private_key\":\"test-priv\",\"public_key\":\"test-pub\",\"short_ids\":[\"test-sid\"]}"}`
	w := doAuth(r, "POST", "/api/v1/internal/xray/sync", payload)

	assert.Equal(t, http.StatusOK, w.Code)

	// Verify the keys file was written correctly
	assert.FileExists(t, keysPath)
	data, err := os.ReadFile(keysPath)
	assert.NoError(t, err)
	assert.Contains(t, string(data), "test-priv")
	assert.Contains(t, string(data), "test-pub")
	assert.Contains(t, string(data), "test-sid")
}
