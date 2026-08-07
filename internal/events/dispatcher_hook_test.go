package events

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDispatcher_DispatchInvokesPluginHostHookWithoutWebhooks(t *testing.T) {
	var calls atomic.Int32
	d := NewDispatcher(&Config{
		OnDispatch: func(eventType string, data map[string]interface{}, metadata map[string]interface{}) {
			require.Equal(t, "payment.completed", eventType)
			require.Equal(t, "p_1", data["payment_id"])
			require.Equal(t, "u_1", metadata["user_id"])
			calls.Add(1)
		},
	})

	d.Dispatch("payment.completed", map[string]interface{}{"payment_id": "p_1"}, map[string]interface{}{"user_id": "u_1"})
	require.EqualValues(t, 1, calls.Load())
}
