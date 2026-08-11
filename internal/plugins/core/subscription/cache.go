package subscription

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"xraytool/internal/appconfig"
	"xraytool/internal/domain"
	"xraytool/internal/logger"
	"xraytool/internal/pluginapi"
)

// CacheManager holds subscription templates and the engine-neutral
// configuration projection used to serve subscriptions. Native engine files
// are intentionally never read from this package.
type CacheManager struct {
	mu        sync.RWMutex
	refreshMu sync.Mutex

	cfg *appconfig.Config

	clientConfigContributor any
	configProvider          pluginapi.SubscriptionConfigProvider
	trafficProvider         pluginapi.TrafficProvider
	trafficQuotaProvider    pluginapi.TrafficQuotaProvider
	formatProvider          pluginapi.SubscriptionFormatProvider
	templateProcessor       pluginapi.SubscriptionTemplateProcessor

	subscriptionConfig      pluginapi.SubscriptionConfigSnapshot
	subscriptionConfigReady bool
	activeUsers             map[string]*ActiveUser
	subTemplate             string
	routeGlobal             string
	routeRU                 string

	subTemplateModTime time.Time
	routeGlobalModTime time.Time
	routeRUModTime     time.Time

	done chan struct{}
}

// NewCacheManager initializes a cache. The engine is used only for optional
// engine-neutral capabilities; it is never type-asserted to an Xray adapter.
func NewCacheManager(cfg *appconfig.Config, engine domain.Engine) *CacheManager {
	cm := &CacheManager{
		cfg:         cfg,
		activeUsers: make(map[string]*ActiveUser),
		done:        make(chan struct{}),
	}
	cm.setEngineClientConfigContributor(engine)
	cm.setEngineSubscriptionConfigProvider(engine)
	return cm
}

// Refresh uses stale-while-revalidate semantics. The first successful load is
// synchronous; subsequent calls leave a previous complete snapshot available
// while another goroutine refreshes it.
func (c *CacheManager) Refresh() {
	c.mu.RLock()
	empty := !c.subscriptionConfigReady || c.subTemplate == ""
	c.mu.RUnlock()

	if empty {
		c.refreshMu.Lock()
		defer c.refreshMu.Unlock()
		c.refreshAll()
		return
	}
	if !c.refreshMu.TryLock() {
		return
	}
	defer c.refreshMu.Unlock()
	c.refreshAll()
}

func (c *CacheManager) refreshAll() {
	c.refreshSubscriptionConfig()
	c.refreshTemplates()
}

func (c *CacheManager) refreshSubscriptionConfig() {
	provider := c.SubscriptionConfigProvider()
	if provider == nil {
		return
	}
	snapshot, err := provider.SubscriptionConfigSnapshot(context.Background())
	if err != nil {
		logger.Warnf("[Cache] Engine subscription configuration is unavailable: %v", err)
		return
	}

	c.mu.RLock()
	unchanged := c.subscriptionConfigReady && snapshot.Revision != 0 && snapshot.Revision == c.subscriptionConfig.Revision
	c.mu.RUnlock()
	if unchanged {
		return
	}

	users := activeUsersFromSubscriptionClients(snapshot.ActiveClients, defaultExpireDate())
	c.mu.Lock()
	c.subscriptionConfig = cloneSubscriptionConfigSnapshot(snapshot)
	c.subscriptionConfigReady = true
	c.activeUsers = users
	c.mu.Unlock()
}

func activeUsersFromSubscriptionClients(clients []pluginapi.SubscriptionClient, defaultExpire string) map[string]*ActiveUser {
	users := make(map[string]*ActiveUser)
	for _, client := range clients {
		target := normalizeSubfileToID(client.Subfile)
		if target == "" || client.Email == "" {
			continue
		}
		limit := client.MaxDevices
		if limit <= 0 {
			limit = 3
		}
		row := &ActiveUser{
			Email: client.Email, ID: client.ID, Subfile: client.Subfile,
			Password: client.Password, Expire: client.Expire, Hy2Auth: client.Auth,
			Hy2Obfs: client.Obfs, Limit: limit,
		}
		if row.Expire == "" {
			row.Expire = defaultExpire
		}
		if best, exists := users[target]; exists {
			mergeActiveUser(best, row)
		} else {
			users[target] = row
		}
	}
	return users
}

func mergeActiveUser(best, row *ActiveUser) {
	if best.Hy2Auth == "" && row.Hy2Auth != "" {
		best.Hy2Auth = row.Hy2Auth
	}
	if best.Password == "" && row.Password != "" {
		best.Password = row.Password
	}
	if best.ID == "" && row.ID != "" {
		best.ID = row.ID
	}
	if best.Hy2Obfs == "" && row.Hy2Obfs != "" {
		best.Hy2Obfs = row.Hy2Obfs
	}
	if best.Expire == "" && row.Expire != "" {
		best.Expire = row.Expire
	}
}

func cloneSubscriptionConfigSnapshot(snapshot pluginapi.SubscriptionConfigSnapshot) pluginapi.SubscriptionConfigSnapshot {
	copySnapshot := snapshot
	copySnapshot.ActiveClients = append([]pluginapi.SubscriptionClient(nil), snapshot.ActiveClients...)
	copySnapshot.TemplateClients = append([]pluginapi.SubscriptionClient(nil), snapshot.TemplateClients...)
	copySnapshot.RealityShortIDs = append([]string(nil), snapshot.RealityShortIDs...)
	return copySnapshot
}

func (c *CacheManager) refreshTemplates() {
	subTmplPath := firstReadablePath(
		c.cfg.Paths.JSONSubscriptionTemplate,
		strings.ReplaceAll(c.cfg.Paths.JSONSubscriptionTemplate, "/helpful_bots/Dev/", "/helpful_bots/dev/"),
		"/var/www/TorvaldsVPN/helpful_bots/dev/configs.txt",
		"/var/www/TorvaldsVPN/helpful_bots/configs.txt",
		"/var/www/TorvaldsVPN/xraytool/configs.txt",
		"./configs.txt",
	)
	c.mu.RLock()
	tmplModTime := c.subTemplateModTime
	c.mu.RUnlock()
	if info, err := os.Stat(subTmplPath); err == nil && !info.ModTime().Equal(tmplModTime) {
		if data, readErr := os.ReadFile(subTmplPath); readErr == nil {
			c.mu.Lock()
			c.subTemplate = string(data)
			c.subTemplateModTime = info.ModTime()
			c.mu.Unlock()
			logger.Infof("[Cache] Subscription template refreshed.")
		}
	}

	routingPath := firstReadablePath(
		c.cfg.Paths.RoutingTemplate,
		strings.ReplaceAll(c.cfg.Paths.RoutingTemplate, "/helpful_bots/Dev/", "/helpful_bots/dev/"),
		"/var/www/TorvaldsVPN/helpful_bots/dev/routing.json",
		"/var/www/TorvaldsVPN/helpful_bots/routing.json",
		"/var/www/TorvaldsVPN/xraytool/routing.json",
		"./routing.json",
	)
	c.mu.RLock()
	routeModTime := c.routeGlobalModTime
	c.mu.RUnlock()
	if info, err := os.Stat(routingPath); err == nil && !info.ModTime().Equal(routeModTime) {
		if data, readErr := os.ReadFile(routingPath); readErr == nil {
			c.mu.Lock()
			c.routeGlobal = strings.TrimSpace(string(data))
			c.routeGlobalModTime = info.ModTime()
			c.mu.Unlock()
		}
	}

	routingRUPath := firstReadablePath(
		c.cfg.Paths.RoutingRUTemplate,
		strings.ReplaceAll(c.cfg.Paths.RoutingRUTemplate, "/helpful_bots/Dev/", "/helpful_bots/dev/"),
		"/var/www/TorvaldsVPN/helpful_bots/dev/routing_ALL_RU.json",
		"/var/www/TorvaldsVPN/helpful_bots/routing_ALL_RU.json",
		"/var/www/TorvaldsVPN/xraytool/routing_ALL_RU.json",
		"./routing_ALL_RU.json",
	)
	c.mu.RLock()
	routeRUModTime := c.routeRUModTime
	c.mu.RUnlock()
	if info, err := os.Stat(routingRUPath); err == nil && !info.ModTime().Equal(routeRUModTime) {
		if data, readErr := os.ReadFile(routingRUPath); readErr == nil {
			c.mu.Lock()
			c.routeRU = strings.TrimSpace(string(data))
			c.routeRUModTime = info.ModTime()
			c.mu.Unlock()
		}
	}
}

// GetUserBySubfile returns the active user in O(1) time.
func (c *CacheManager) GetUserBySubfile(filename string) *ActiveUser {
	c.mu.RLock()
	defer c.mu.RUnlock()
	target := normalizeSubfileToID(filename)
	user := c.activeUsers[target]
	if user == nil {
		return nil
	}
	copyUser := *user
	return &copyUser
}

func (c *CacheManager) GetTemplates() (sub string, routeGlobal string, routeRU string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	routeGlobal = c.routeGlobal
	if routeGlobal == "" {
		routeGlobal = `{"rules":[]}`
	}
	routeRU = c.routeRU
	if routeRU == "" {
		routeRU = `{"rules":[]}`
	}
	return c.subTemplate, routeGlobal, routeRU
}

func (c *CacheManager) SetTrafficProviders(traffic pluginapi.TrafficProvider, quota pluginapi.TrafficQuotaProvider) {
	c.mu.Lock()
	c.trafficProvider = traffic
	c.trafficQuotaProvider = quota
	c.mu.Unlock()
}

func (c *CacheManager) TrafficProviders() (pluginapi.TrafficProvider, pluginapi.TrafficQuotaProvider) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.trafficProvider, c.trafficQuotaProvider
}

func (c *CacheManager) SetSubscriptionFormatProvider(provider pluginapi.SubscriptionFormatProvider) {
	c.mu.Lock()
	c.formatProvider = provider
	c.mu.Unlock()
}

func (c *CacheManager) SubscriptionFormatProvider() pluginapi.SubscriptionFormatProvider {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.formatProvider
}

func (c *CacheManager) SetSubscriptionTemplateProcessor(processor pluginapi.SubscriptionTemplateProcessor) {
	c.mu.Lock()
	c.templateProcessor = processor
	c.mu.Unlock()
}

func (c *CacheManager) SubscriptionTemplateProcessor() pluginapi.SubscriptionTemplateProcessor {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.templateProcessor
}
