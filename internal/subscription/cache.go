package subscription

import (
	"bufio"
	"os"
	"strings"
	"sync"
	"time"

	"xraytool/internal/appconfig"
	"xraytool/internal/logger"
	"xraytool/internal/xrayconfig"
)

// CacheManager handles in-memory caching of frequently read files
// (config.json, templates, limited_users.db) to eliminate disk I/O
// and parsing overhead during subscription delivery.
type CacheManager struct {
	mu sync.RWMutex
	refreshMu sync.Mutex

	cfg *appconfig.Config

	// Cached Data
	xrayConfig   xrayconfig.RawConfig
	activeUsers  map[string]*ActiveUser
	limitedUsers map[string]*LimitedUser
	subTemplate  string
	routeGlobal  string
	routeRU      string

	// ModTimes to detect file changes on disk
	xrayConfigModTime  time.Time
	limitedDBModTime   time.Time
	subTemplateModTime time.Time
	routeGlobalModTime time.Time
	routeRUModTime     time.Time

	// Device State
	deviceStateMu      sync.Mutex
	deviceState        DeviceState
	deviceStateDirty   bool
	deviceStateLoaded  bool
	deviceStateModTime time.Time

	// done is closed by Stop() to signal the flush worker to exit.
	done chan struct{}
}

// NewCacheManager initializes a new cache manager.
func NewCacheManager(cfg *appconfig.Config) *CacheManager {
	cm := &CacheManager{
		cfg:          cfg,
		activeUsers:  make(map[string]*ActiveUser),
		limitedUsers: make(map[string]*LimitedUser),
		done:         make(chan struct{}),
	}
	// Start async flusher for devices state
	go cm.flushDeviceStateWorker()
	return cm
}

// Refresh checks all cached files and reloads them if their Modification Time changed.
// This allows hot-reloading without restarting the server.
func (c *CacheManager) Refresh() {
	// Prevent concurrent refresh operations
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	c.refreshXrayConfig()
	c.refreshLimitedDB()
	c.refreshTemplates()
	c.refreshDeviceState()
}

func (c *CacheManager) refreshXrayConfig() {
	path := firstReadablePath(
		c.cfg.Paths.XrayConfig,
		"/usr/local/etc/xray/config.json",
		"/usr/local/etc/xray/config/config.json",
	)
	if path == "" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	c.mu.RLock()
	modTime := c.xrayConfigModTime
	c.mu.RUnlock()

	if info.ModTime().Equal(modTime) {
		return // No changes
	}

	logger.Infof("[Cache] Обнаружено изменение %s. Обновление индекса пользователей...", path)
	cfg, err := xrayconfig.Read(path)
	if err != nil {
		logger.Errorf("[Cache] Ошибка чтения Xray config: %v", err)
		return
	}

	// Rebuild active users index O(N) -> O(1)
	newActiveUsers := make(map[string]*ActiveUser)
	defaultExpire := defaultExpireDate()

	inbounds, err := cfg.GetInbounds()
	if err == nil {
		for _, ib := range inbounds {
			clients, err := ib.GetClients()
			if err != nil {
				continue
			}
			for _, client := range clients {
				sub := client.GetString("subfile")
				if sub == "" {
					continue
				}
				targetNorm := normalizeSubfileToID(sub)
				email := client.Email()
				if email == "" {
					continue
				}

				limitVal := 3
				if lv, ok := client.GetNumber("limit"); ok && lv > 0 {
					limitVal = int(lv)
				}

				hy2Auth := client.GetString("auth")

				row := &ActiveUser{
					Email:    email,
					ID:       client.GetString("id"),
					Subfile:  sub,
					Password: client.GetString("password"),
					Expire:   client.GetString("expire"),
					Hy2Auth:  hy2Auth,
					Hy2Obfs:  client.GetString("hy2_obfs"),
					Limit:    limitVal,
				}
				if row.Expire == "" {
					row.Expire = defaultExpire
				}

				// Handle merging multiple inbounds per user (e.g. VLESS + Hysteria2)
				if best, exists := newActiveUsers[targetNorm]; exists {
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
				} else {
					newActiveUsers[targetNorm] = row
				}
			}
		}
	}

	c.mu.Lock()
	c.xrayConfig = cfg
	c.activeUsers = newActiveUsers
	c.xrayConfigModTime = info.ModTime()
	c.mu.Unlock()
	
	logger.Infof("[Cache] Индекс Xray обновлен. Загружено %d пользователей.", len(newActiveUsers))
}

func (c *CacheManager) refreshLimitedDB() {
	path := c.cfg.Paths.LimitedDB
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	c.mu.RLock()
	modTime := c.limitedDBModTime
	c.mu.RUnlock()

	if info.ModTime().Equal(modTime) {
		return
	}

	logger.Infof("[Cache] Обнаружено изменение %s. Обновление базы лимитов...", path)
	f, err := os.Open(path)
	if err != nil {
		logger.Errorf("[Cache] Ошибка чтения limited_db: %v", err)
		return
	}
	defer f.Close()

	newLimitedUsers := make(map[string]*LimitedUser)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 2 {
			continue
		}
		email := strings.TrimSpace(parts[0])
		sub := strings.TrimSpace(parts[1])

		targetNorm := normalizeSubfileToID(sub)
		newLimitedUsers[targetNorm] = &LimitedUser{
			Email:   email,
			Subfile: sub,
		}
	}

	c.mu.Lock()
	c.limitedUsers = newLimitedUsers
	c.limitedDBModTime = info.ModTime()
	c.mu.Unlock()
}

func (c *CacheManager) refreshTemplates() {
	// Sub Template
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
		if data, err := os.ReadFile(subTmplPath); err == nil {
			c.mu.Lock()
			c.subTemplate = string(data)
			c.subTemplateModTime = info.ModTime()
			c.mu.Unlock()
			logger.Infof("[Cache] Шаблон подписок обновлен.")
		}
	}

	// Routing Global
	routingPath := firstReadablePath(
		c.cfg.Paths.RoutingTemplate,
		strings.ReplaceAll(c.cfg.Paths.RoutingTemplate, "/helpful_bots/Dev/", "/helpful_bots/dev/"),
		"/var/www/TorvaldsVPN/helpful_bots/dev/routing.json",
		"/var/www/TorvaldsVPN/helpful_bots/routing.json",
		"/var/www/TorvaldsVPN/xraytool/routing.json",
		"./routing.json",
	)
	c.mu.RLock()
	rgModTime := c.routeGlobalModTime
	c.mu.RUnlock()

	if info, err := os.Stat(routingPath); err == nil && !info.ModTime().Equal(rgModTime) {
		if data, err := os.ReadFile(routingPath); err == nil {
			c.mu.Lock()
			c.routeGlobal = strings.TrimSpace(string(data))
			c.routeGlobalModTime = info.ModTime()
			c.mu.Unlock()
			logger.Infof("[Cache] Глобальный роутинг обновлен.")
		}
	}

	// Routing RU
	routingRUPath := firstReadablePath(
		c.cfg.Paths.RoutingRUTemplate,
		strings.ReplaceAll(c.cfg.Paths.RoutingRUTemplate, "/helpful_bots/Dev/", "/helpful_bots/dev/"),
		"/var/www/TorvaldsVPN/helpful_bots/dev/routing_ALL_RU.json",
		"/var/www/TorvaldsVPN/helpful_bots/routing_ALL_RU.json",
		"/var/www/TorvaldsVPN/xraytool/routing_ALL_RU.json",
		"./routing_ALL_RU.json",
	)
	c.mu.RLock()
	ruModTime := c.routeRUModTime
	c.mu.RUnlock()

	if info, err := os.Stat(routingRUPath); err == nil && !info.ModTime().Equal(ruModTime) {
		if data, err := os.ReadFile(routingRUPath); err == nil {
			c.mu.Lock()
			c.routeRU = strings.TrimSpace(string(data))
			c.routeRUModTime = info.ModTime()
			c.mu.Unlock()
			logger.Infof("[Cache] RU роутинг обновлен.")
		}
	}
}

// GetUserBySubfile returns the active user or limited user in O(1) time.
func (c *CacheManager) GetUserBySubfile(filename string) (*ActiveUser, *LimitedUser) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	targetNorm := normalizeSubfileToID(filename)
	if targetNorm == "" {
		return nil, nil
	}
	return c.activeUsers[targetNorm], c.limitedUsers[targetNorm]
}

// GetTemplates returns the cached templates.
func (c *CacheManager) GetTemplates() (sub string, routeGlobal string, routeRU string) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.routeGlobal == "" {
		routeGlobal = `{"rules":[]}`
	} else {
		routeGlobal = c.routeGlobal
	}

	if c.routeRU == "" {
		routeRU = `{"rules":[]}`
	} else {
		routeRU = c.routeRU
	}

	return c.subTemplate, routeGlobal, routeRU
}

// GetRawConfig returns a copy of the xray config if needed for reading reality keys etc.
func (c *CacheManager) GetRawConfig() xrayconfig.RawConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.xrayConfig
}
