package engine_xray

import (
	"testing"

	"github.com/stretchr/testify/require"

	"xraytool/internal/domain"
)

func TestSubscriptionToVPNUserConfigCarriesRoutingMetadata(t *testing.T) {
	sub := domain.Subscription{
		Email: "person@example.test",
		UUID:  "uuid-1",
		Metadata: domain.Metadata{
			"engine_ids":      []interface{}{" singbox ", "singbox", "xray", 5},
			"plan_engine_ids": "xray, singbox, xray",
		},
	}

	user := SubscriptionToVPNUserConfig(sub)
	require.Equal(t, []string{"singbox", "xray"}, user.SubscriptionEngineIDs)
	require.Equal(t, []string{"xray", "singbox"}, user.PlanEngineIDs)
}
