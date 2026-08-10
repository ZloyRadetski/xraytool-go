// Package pluginhost — registry_builtin.go
//
// Единственное место регистрации всех compiled-in ("builtin") плагинов.
// Новый плагин = одна строка здесь; остальное система обнаруживает сама.
//
// Build tags на пакетах плагинов позволяют исключать их из бинарника:
//
//	//go:build !plugin_antifraud
//	package antifraud_plugin
//go:build !minimal

package pluginhost

import (
	"xraytool/internal/appconfig"
	"xraytool/internal/pluginapi"
	antifraudPlugin "xraytool/internal/plugins/antifraud"
	apiServerPlugin "xraytool/internal/plugins/api_server"
	billingPlugin "xraytool/internal/plugins/billing"
	clusterReplicationPlugin "xraytool/internal/plugins/cluster_replication"
	storagePlugin "xraytool/internal/plugins/config_storage"
	corePlugin "xraytool/internal/plugins/core"
	enginePlugin "xraytool/internal/plugins/engine_xray"
	eventsinkPlugin "xraytool/internal/plugins/eventsink_webhook"
	identityPlugin "xraytool/internal/plugins/identity_memory"
	mailerPlugin "xraytool/internal/plugins/mailer_resend"
	plategaPlugin "xraytool/internal/plugins/payment_platega"
	pricingPlugin "xraytool/internal/plugins/pricing_default"
	promoPlugin "xraytool/internal/plugins/promo"
	referralPlugin "xraytool/internal/plugins/referral"
	autoBalancerPlugin "xraytool/internal/plugins/subscription_autobalancer"
	formatPlugin "xraytool/internal/plugins/subscription_format_legacy"
	lifecyclePlugin "xraytool/internal/plugins/subscription_lifecycle"
	subscriptionRuntimePlugin "xraytool/internal/plugins/subscription_runtime"
	supportChatPlugin "xraytool/internal/plugins/support_chat"
	trafficPlugin "xraytool/internal/plugins/traffic_file"
	userManagementPlugin "xraytool/internal/plugins/user_management"
)

// BuiltinRegistry возвращает карту name → factory для всех compiled-in плагинов.
//
// Передаётся в Host при создании:
//
//	host := pluginhost.New(cfg, log, pluginhost.BuiltinRegistry(cfg), emitFn)
//
// В тестах вместо неё передаётся вручную собранная карта (см. host_test.go).
func BuiltinRegistry(cfg *appconfig.Config) map[string]func() pluginapi.Plugin {
	return map[string]func() pluginapi.Plugin{
		// ── Phase 1: Mandatory core plugin ──────────────────────────────────
		"core":                      func() pluginapi.Plugin { return corePlugin.New(cfg) },
		"user_management":           func() pluginapi.Plugin { return userManagementPlugin.New(cfg) },
		"subscription_runtime":      func() pluginapi.Plugin { return subscriptionRuntimePlugin.New(cfg) },
		"subscription_autobalancer": func() pluginapi.Plugin { return autoBalancerPlugin.New() },
		"api_server":                func() pluginapi.Plugin { return apiServerPlugin.New(cfg) },
		"config_storage":            func() pluginapi.Plugin { return storagePlugin.New(cfg) },

		// ── Phase 1.1: Optional built-in plugins ────────────────────────────
		// Antifraud: multi-IP soft-ban, log tailing.
		// Config keys under plugins.antifraud.config: enabled, log_path, max_ips, ...
		"antifraud": func() pluginapi.Plugin { return antifraudPlugin.New() },
		"cluster_replication": func() pluginapi.Plugin { return clusterReplicationPlugin.NewCLI(cfg) },

		// ── Phase 1.5: Xray Engine plugin ─────────────────────────────────────
		// engine_xray is disabled=false but its factory is passed via NewFromAdapter
		// (kernel builds adapter above). Enabled by default for now.
		"engine_xray":     func() pluginapi.Plugin { return enginePlugin.New() },
		"identity_memory": func() pluginapi.Plugin { return identityPlugin.New() },

		// Mailer: email delivery via Resend.com.
		// Config keys: resend_api_key, from_email.
		"mailer_resend": func() pluginapi.Plugin { return mailerPlugin.New() },

		// EventSink: HTTP webhook delivery with HMAC signing and retry.
		// Config keys: webhooks ([]string), webhook_secret.
		"eventsink_webhook": func() pluginapi.Plugin { return eventsinkPlugin.New() },

		// Platega payment gateway
		"payment_platega": func() pluginapi.Plugin { return plategaPlugin.New() },

		// Pricing logic
		"pricing_default": func() pluginapi.Plugin { return pricingPlugin.New() },

		// Support chat
		"support_chat":               func() pluginapi.Plugin { return supportChatPlugin.New() },
		"billing":                    func() pluginapi.Plugin { return billingPlugin.NewPlugin(cfg) },
		"promo":                      func() pluginapi.Plugin { return promoPlugin.NewPlugin() },
		"referral":                   func() pluginapi.Plugin { return referralPlugin.NewPlugin() },
		"subscription_lifecycle":     func() pluginapi.Plugin { return lifecyclePlugin.New(cfg) },
		"subscription_format_legacy": func() pluginapi.Plugin { return formatPlugin.New() },
		"traffic_file":               func() pluginapi.Plugin { return trafficPlugin.New(cfg) },
		// ── Phase 1.5+: Engine plugins (TBD) ────────────────────────────────
		// "engine_xray": func() pluginapi.Plugin { return engine_xray.New(cfg) },
	}
}
