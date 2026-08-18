package antifraud_plugin

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockLoggerController struct {
	called  bool
	logPath string
}

func (m *mockLoggerController) RestartLogger(ctx context.Context) error {
	m.called = true
	if m.logPath != "" {
		_ = os.WriteFile(m.logPath, []byte{}, 0644)
	}
	return nil
}

func TestRotator_CleanupAtStartup(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	oldPath := logPath + ".old"

	// Create a dummy .old file before starting
	err := os.WriteFile(oldPath, []byte("old logs"), 0644)
	assert.NoError(t, err)

	notifyCh := make(chan struct{}, 1)
	mockCtrl := &mockLoggerController{}
	r := newRotator(logPath, 1, 10*time.Minute, mockCtrl, notifyCh, slog.Default())

	// Run startup cleanup using a cancelled context so it exits immediately
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r.run(ctx)

	// The .old file must have been deleted at startup
	_, err = os.Stat(oldPath)
	assert.True(t, os.IsNotExist(err), "leftover .old file must be deleted at startup")
}

func TestRotator_ForceRemoveStaleOldFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	oldPath := logPath + ".old"

	// Create main log file with size exceeding 1MB (we configure maxMB = 1)
	content := make([]byte, 2*1024*1024) // 2MB
	err := os.WriteFile(logPath, content, 0644)
	assert.NoError(t, err)

	// Create a stale .old file and backdate its ModTime by 1 minute
	err = os.WriteFile(oldPath, []byte("stale logs"), 0644)
	assert.NoError(t, err)
	staleTime := time.Now().Add(-1 * time.Minute)
	err = os.Chtimes(oldPath, staleTime, staleTime)
	assert.NoError(t, err)

	notifyCh := make(chan struct{}, 1)
	mockCtrl := &mockLoggerController{logPath: logPath}
	r := newRotator(logPath, 1, 10*time.Minute, mockCtrl, notifyCh, slog.Default())

	// This should trigger rotation, force-delete the stale .old file, and execute new rotation
	r.tryRotate(context.Background())

	assert.True(t, mockCtrl.called, "RestartLogger should have been called")

	// Verify that the new access.log is now truncated/empty
	info, err := os.Stat(logPath)
	require.NoError(t, err)
	assert.Equal(t, int64(0), info.Size(), "log file should be truncated after rotation")
}
