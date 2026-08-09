package subscription_format_legacy

import (
	"context"
	"testing"

	"xraytool/internal/pluginapi"
)

func TestRenderVLESSUsesNativeLinks(t *testing.T) {
	p := New()
	result, err := p.RenderSubscription(context.Background(), pluginapi.SubscriptionFormatRequest{
		Format: "vless",
		Links:  []pluginapi.ClientLink{{URI: "vless://first"}, {URI: "hysteria2://second"}},
	})
	if err != nil {
		t.Fatalf("RenderSubscription: %v", err)
	}
	if !result.Handled || result.Body != "vless://first\nhysteria2://second" {
		t.Fatalf("unexpected result: %#v", result)
	}
	if result.ContentDisposition != `attachment; filename="configs.txt"` {
		t.Fatalf("unexpected content disposition: %q", result.ContentDisposition)
	}
}
