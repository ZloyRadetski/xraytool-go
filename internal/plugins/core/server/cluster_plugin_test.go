package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	json "github.com/goccy/go-json"
	"github.com/stretchr/testify/require"

	"xraytool/internal/pluginapi"
)

type clusterCommand struct {
	name   string
	params map[string]string
}

type clusterProviderStub struct {
	snapshot []pluginapi.VPNUserConfig
	state    pluginapi.SyncState
	commands chan clusterCommand
}

func (s *clusterProviderStub) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{Name: "cluster-test", Kind: "cluster_sync", APIVersion: pluginapi.CurrentAPIVersion}
}
func (*clusterProviderStub) Init(context.Context, pluginapi.RawConfig, pluginapi.ServiceResolver) error {
	return nil
}
func (*clusterProviderStub) Start(ctx context.Context) error { <-ctx.Done(); return nil }
func (*clusterProviderStub) Stop(context.Context) error      { return nil }
func (*clusterProviderStub) Health(context.Context) error    { return nil }
func (*clusterProviderStub) SyncAllSlaves(context.Context, bool, bool) ([]pluginapi.SyncResult, error) {
	return nil, nil
}
func (*clusterProviderStub) CollectSlaveTotals() ([]pluginapi.SlaveUserTotal, pluginapi.SlaveReport) {
	return nil, pluginapi.SlaveReport{}
}
func (s *clusterProviderStub) BuildSnapshot(context.Context) ([]pluginapi.VPNUserConfig, error) {
	return s.snapshot, nil
}
func (s *clusterProviderStub) MasterState(context.Context) (pluginapi.SyncState, error) {
	return s.state, nil
}
func (s *clusterProviderStub) PropagateCommand(_ context.Context, command string, params map[string]string) error {
	copyParams := make(map[string]string, len(params))
	for key, value := range params {
		copyParams[key] = value
	}
	s.commands <- clusterCommand{name: command, params: copyParams}
	return nil
}

var _ pluginapi.ClusterSyncHTTPProvider = (*clusterProviderStub)(nil)
var _ pluginapi.ClusterCommandPropagator = (*clusterProviderStub)(nil)

func TestWithClusterSyncProviderServesSnapshotAndState(t *testing.T) {
	provider := &clusterProviderStub{
		snapshot: []pluginapi.VPNUserConfig{{Email: "person@example.test", UUID: "uuid-1"}},
		state:    pluginapi.SyncState{LastEventID: 42, StateHash: "hash-42"},
		commands: make(chan clusterCommand, 1),
	}
	router := (&Router{log: slog.Default()}).WithClusterSyncProvider(provider)

	snapshotReq := httptest.NewRequest(http.MethodGet, "/api/v1/internal/xray/sync/snapshot?offset=0&limit=1000", nil)
	snapshotRec := httptest.NewRecorder()
	router.handleSyncSnapshot(snapshotRec, snapshotReq)
	require.Equal(t, http.StatusOK, snapshotRec.Code)

	var snapshot struct {
		Users   []pluginapi.VPNUserConfig `json:"users"`
		HasMore bool                      `json:"has_more"`
		Total   int                       `json:"total"`
	}
	require.NoError(t, json.Unmarshal(snapshotRec.Body.Bytes(), &snapshot))
	require.Equal(t, provider.snapshot, snapshot.Users)
	require.False(t, snapshot.HasMore)
	require.Equal(t, 1, snapshot.Total)

	stateReq := httptest.NewRequest(http.MethodGet, "/api/v1/internal/xray/sync/state", nil)
	stateRec := httptest.NewRecorder()
	router.handleSyncState(stateRec, stateReq)
	require.Equal(t, http.StatusOK, stateRec.Code)

	var state pluginapi.SyncState
	require.NoError(t, json.Unmarshal(stateRec.Body.Bytes(), &state))
	require.Equal(t, provider.state, state)
}

func TestClusterCommandPropagationUsesProvider(t *testing.T) {
	provider := &clusterProviderStub{commands: make(chan clusterCommand, 1)}
	router := (&Router{log: slog.Default()}).WithClusterSyncProvider(provider)

	router.propagateClusterCommand("rmuser", map[string]string{"email": "person@example.test"})

	select {
	case command := <-provider.commands:
		require.Equal(t, "rmuser", command.name)
		require.Equal(t, "person@example.test", command.params["email"])
	case <-time.After(time.Second):
		t.Fatal("cluster command was not propagated")
	}
	router.bgTasks.Wait()
}
