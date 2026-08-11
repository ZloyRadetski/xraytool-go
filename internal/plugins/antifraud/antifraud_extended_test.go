package antifraud_plugin

// antifraud_extended_test.go — расширенные тесты для:
//   - deviceLimitCache (конкурентность, edge-cases, bulk refresh)
//   - propagator (батчинг, flush, пустой буфер)
//   - Module.IngestEvents (инъекция событий со slave)
//   - Динамический порог (dry-run + multi-device)
//   - Бенчмарки новых горячих путей

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"log/slog"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"xraytool/internal/database"
	"xraytool/internal/domain"
	"xraytool/internal/pluginapi"
	vpn "xraytool/internal/plugins/engine_xray"
)

// ─────────────────────────────────────────────────────────────────────────────
// deviceLimitCache — edge cases
// ─────────────────────────────────────────────────────────────────────────────

// TestGetDeviceLimit_ZeroMaxDevicesTreatedAsFallback проверяет, что если
// в кэше хранится 0 (результат некорректной записи в БД),
// getDeviceLimit возвращает fallback = 1.
func TestGetDeviceLimit_ZeroMaxDevicesTreatedAsFallback(t *testing.T) {
	db := setupTestDB(t)
	defer closeSQLite(t, db)

	// Напрямую записываем 0 в кэш (симуляция некорректного MaxDevices=0 в БД).
	an := newAnalyzer(&Config{}, newState("dummy-test-key"), newBanStore(), nil, time.Minute, 3, database.NewRegistry(db), &vpn.NoopEngine{}, nil, nil, slog.Default())
	an.deviceCache.mu.Lock()
	an.deviceCache.limits["zero@x.com"] = 0
	an.deviceCache.mu.Unlock()

	// Нулевое значение в кэше → возвращает 1 (fallback).
	// cache miss с 0 выбирает 1 как безопасный минимум.
	// Попадание в кэш с 0 → тоже должно вернуть 1.
	limit := an.getDeviceLimit("zero@x.com")
	assert.Equal(t, 1, limit, "cached MaxDevices=0 must fall back to 1")
}

// TestGetDeviceLimit_CachePersistsAfterRefresh проверяет что после полного
// обновления кэша через refreshDeviceCache() значение для старого юзера
// сохраняется, а новый юзер появляется.
func TestGetDeviceLimit_CachePersistsAfterRefresh(t *testing.T) {
	db := setupTestDB(t)
	defer closeSQLite(t, db)

	db.Create(&database.User{ID: "u-r1"})
	db.Create(&database.Subscription{
		ID: "sub-r1", UserID: "u-r1",
		Email: "old@x.com", Status: "active", MaxDevices: 3,
	})

	an := newAnalyzer(&Config{}, newState("dummy-test-key"), newBanStore(), nil, time.Minute, 3, database.NewRegistry(db), &vpn.NoopEngine{}, nil, nil, slog.Default())
	an.refreshDeviceCache()

	assert.Equal(t, 3, an.getDeviceLimit("old@x.com"))

	// Добавляем второго юзера напрямую в кэш (симулируем refresh для нового юзера).
	an.deviceCache.mu.Lock()
	an.deviceCache.limits["new@x.com"] = 7
	an.deviceCache.mu.Unlock()

	assert.Equal(t, 3, an.getDeviceLimit("old@x.com"), "old user persists after refresh")
	assert.Equal(t, 7, an.getDeviceLimit("new@x.com"), "new user appears after cache update")
}

// TestGetDeviceLimit_ConcurrentReads проверяет отсутствие гонок при
// одновременном чтении кэша из нескольких горутин (запускать с -race).
func TestGetDeviceLimit_ConcurrentReads(t *testing.T) {
	db := setupTestDB(t)
	defer closeSQLite(t, db)

	db.Create(&database.User{ID: "u-c"})
	db.Create(&database.Subscription{
		ID: "sub-c", UserID: "u-c",
		Email: "concurrent@x.com", Status: "active", MaxDevices: 4,
	})

	an := newAnalyzer(&Config{}, newState("dummy-test-key"), newBanStore(), nil, time.Minute, 3, database.NewRegistry(db), &vpn.NoopEngine{}, nil, nil, slog.Default())
	an.refreshDeviceCache()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			limit := an.getDeviceLimit("concurrent@x.com")
			assert.Equal(t, 4, limit)
		}()
	}
	wg.Wait()
}

// TestGetDeviceLimit_ConcurrentRefreshAndRead проверяет отсутствие гонок при
// одновременном refreshDeviceCache() и чтении (запускать с -race).
func TestGetDeviceLimit_ConcurrentRefreshAndRead(t *testing.T) {
	db := setupTestDB(t)
	defer closeSQLite(t, db)

	db.Create(&database.User{ID: "u-cr"})
	db.Create(&database.Subscription{
		ID: "sub-cr", UserID: "u-cr",
		Email: "race@x.com", Status: "active", MaxDevices: 2,
	})

	an := newAnalyzer(&Config{}, newState("dummy-test-key"), newBanStore(), nil, time.Minute, 3, database.NewRegistry(db), &vpn.NoopEngine{}, nil, nil, slog.Default())

	var wg sync.WaitGroup
	// Несколько горутин читают
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = an.getDeviceLimit("race@x.com")
		}()
	}
	// Несколько горутин обновляют
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			an.refreshDeviceCache()
		}()
	}
	wg.Wait()
}

// ─────────────────────────────────────────────────────────────────────────────
// Динамический порог — handleEvent + dry-run
// ─────────────────────────────────────────────────────────────────────────────

// TestHandleEvent_DryRun_MultiDevice проверяет что в dry-run режиме
// динамический порог (maxIPs * MaxDevices) вычисляется корректно — бан не
// применяется, но в логе появляется предупреждение.
func TestHandleEvent_DryRun_MultiDevice(t *testing.T) {
	db := setupTestDB(t)
	defer closeSQLite(t, db)

	email := "dryrun@x.com"
	db.Create(&database.User{ID: "u-dry"})
	db.Create(&database.Subscription{
		ID: "sub-dry", UserID: "u-dry",
		Email: email, Status: "active", MaxDevices: 3,
	})

	cfg := &Config{
		DryRun: true, BanDuration: "1h",
	}

	bs := newBanStore()
	an := newAnalyzer(cfg, newState("dummy-test-key"), bs, nil, 5*time.Minute, 2, database.NewRegistry(db), &vpn.NoopEngine{}, nil, nil, slog.Default())
	// threshold = 2 * 3 = 6

	// 7 уникальных IP — в dry-run режиме бан не должен быть применён
	for i := 1; i <= 7; i++ {
		an.handleEvent(event{email: email, ip: generateIP(i)})
	}

	assert.False(t, bs.isBanned(email), "dry-run must not actually ban the user")

	// Убеждаемся что в БД тоже нет записи
	var count int64
	db.Model(&database.AntifraudBan{}).Where("email = ?", email).Count(&count)
	assert.Equal(t, int64(0), count, "dry-run must not write ban to DB")
}

// TestHandleEvent_AlreadyBanned_SkipsProcessing проверяет что для уже
// забаненного юзера события не обрабатываются (не увеличивают счётчик).
func TestHandleEvent_AlreadyBanned_SkipsProcessing(t *testing.T) {
	db := setupTestDB(t)
	defer closeSQLite(t, db)

	cfg := &Config{BanDuration: "1h"}
	state := newState("dummy-test-key")
	bs := newBanStore()
	bs.setBan("banned@x.com", time.Now().Add(time.Hour))

	an := newAnalyzer(cfg, state, bs, nil, 5*time.Minute, 1, database.NewRegistry(db), &vpn.NoopEngine{}, nil, nil, slog.Default())

	// Добавляем события для уже забаненного юзера
	for i := 0; i < 10; i++ {
		an.handleEvent(event{email: "banned@x.com", ip: generateIP(i)})
	}

	// State должен быть пуст — события скипнуты
	assert.Equal(t, 0, state.ActiveIPCount("banned@x.com"), "events for banned user must be skipped")
}

// TestHandleEvent_ExactlyAtThreshold_NoBan проверяет граничный случай:
// count == threshold не должен вызывать бан (бан при count > threshold).
func TestHandleEvent_ExactlyAtThreshold_NoBan(t *testing.T) {
	db := setupTestDB(t)
	defer closeSQLite(t, db)

	email := "boundary@x.com"
	db.Create(&database.User{ID: "u-b"})
	db.Create(&database.Subscription{
		ID: "sub-b", UserID: "u-b",
		Email: email, Status: "active", MaxDevices: 2,
	})

	cfg := &Config{BanDuration: "1h"}
	bs := newBanStore()
	an := newAnalyzer(cfg, newState("dummy-test-key"), bs, nil, 5*time.Minute, 3, database.NewRegistry(db), &vpn.NoopEngine{}, nil, nil, slog.Default())
	// threshold = 3 * 2 = 6
	an.refreshDeviceCache() // pre-warm so async fetch doesn't race with events

	// Ровно 6 IP — не должен забанить
	for i := 1; i <= 6; i++ {
		an.handleEvent(event{email: email, ip: generateIP(i)})
	}
	assert.False(t, bs.isBanned(email), "count == threshold must NOT trigger ban")
}

// TestHandleEvent_OneOverThreshold_Bans проверяет что count == threshold+1
// вызывает бан.
func TestHandleEvent_OneOverThreshold_Bans(t *testing.T) {
	db := setupTestDB(t)
	defer closeSQLite(t, db)

	email := "over@x.com"
	db.Create(&database.User{ID: "u-ov"})
	db.Create(&database.Subscription{
		ID: "sub-ov", UserID: "u-ov",
		Email: email, Status: "active", MaxDevices: 2,
	})

	cfg := &Config{BanDuration: "1h"}
	bs := newBanStore()
	an := newAnalyzer(cfg, newState("dummy-test-key"), bs, nil, 5*time.Minute, 3, database.NewRegistry(db), &vpn.NoopEngine{}, nil, nil, slog.Default())
	// threshold = 3 * 2 = 6
	an.refreshDeviceCache() // pre-warm

	// 7 IP — должен забанить
	for i := 1; i <= 7; i++ {
		an.handleEvent(event{email: email, ip: generateIP(i)})
	}
	assert.True(t, bs.isBanned(email), "count == threshold+1 must trigger ban")
}

// TestHandleEvent_SameIPRepeated_NoBan проверяет что один и тот же IP
// не увеличивает счётчик уникальных адресов — пользователь не будет забанен.
func TestHandleEvent_SameIPRepeated_NoBan(t *testing.T) {
	db := setupTestDB(t)
	defer closeSQLite(t, db)

	email := "oneloc@x.com"
	cfg := &Config{BanDuration: "1h"}
	bs := newBanStore()
	// maxIPs = 1, MaxDevices fallback = 1 → threshold = 1
	an := newAnalyzer(cfg, newState("dummy-test-key"), bs, nil, 5*time.Minute, 1, database.NewRegistry(db), &vpn.NoopEngine{}, nil, nil, slog.Default())

	// Один и тот же IP 100 раз
	for i := 0; i < 100; i++ {
		an.handleEvent(event{email: email, ip: "5.5.5.5"})
	}
	assert.False(t, bs.isBanned(email), "same IP repeated must not trigger ban")
}

// ─────────────────────────────────────────────────────────────────────────────
// Module.IngestEvents — unit tests
// ─────────────────────────────────────────────────────────────────────────────

// TestModule_IngestEvents_ValidEvents проверяет что валидные события попадают
// в eventCh и обрабатываются анализатором.
func startModuleAnalyzer(t *testing.T, m *Module, cfg *Config, maxIPs int) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	m.readyMu.Do(func() { close(m.ready) })
	analyzer := newAnalyzer(cfg, m.state, m.banStore, m.eventCh, 3*time.Minute, maxIPs, m.registry, m.banner, m.propagator, m.reporter, slog.Default())
	done := make(chan struct{})
	go func() {
		defer close(done)
		analyzer.run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return ctx
}

func TestModule_IngestEvents_ValidEvents(t *testing.T) {
	db := setupTestDB(t)
	defer closeSQLite(t, db)

	cfg := &Config{
		Enabled:     true,
		BanDuration: "1h",
		IPLimitTTL:  "3m",
	}
	m := NewModule(cfg, database.NewRegistry(db), &vpn.NoopEngine{}, &vpn.NoopEngine{}, nil, nil, slog.Default())
	ctx := startModuleAnalyzer(t, m, cfg, 3)

	events := []domain.FraudEvent{
		{Email: "slave@x.com", IP: m.state.HashIP("10.0.0.1"), OccurredAt: time.Now().UTC()},
		{Email: "slave@x.com", IP: m.state.HashIP("10.0.0.2"), OccurredAt: time.Now().UTC()},
	}

	require.NoError(t, m.IngestEvents(ctx, "slave-a", events))

	// События должны были попасть в канал
	require.Equal(t, 2, m.state.ActiveIPCount("slave@x.com"))
	require.Equal(t, 1, m.GetSnapshot().ActiveSlaves)
}

// TestModule_IngestEvents_SkipsInvalid проверяет что события с пустыми
// полями email или ip отфильтровываются и не попадают в канал.
func TestModule_IngestEvents_SkipsInvalid(t *testing.T) {
	db := setupTestDB(t)
	defer closeSQLite(t, db)

	cfg := &Config{IPLimitTTL: "3m"}
	m := NewModule(cfg, database.NewRegistry(db), &vpn.NoopEngine{}, &vpn.NoopEngine{}, nil, nil, slog.Default())
	ctx := startModuleAnalyzer(t, m, cfg, 3)
	initialLen := len(m.eventCh)

	m.IngestEvents(ctx, "1.1.1.1", []domain.FraudEvent{
		{Email: "", IP: "1.1.1.1"}, // пустой email
		{Email: "u@x.com", IP: ""}, // пустой IP
		{Email: "", IP: ""},        // оба пустых
	})

	assert.Equal(t, initialLen, len(m.eventCh), "invalid events must not be enqueued")
}

func TestPluginIngestEventsRejectsDifferentClusterHashKey(t *testing.T) {
	db := setupTestDB(t)
	defer closeSQLite(t, db)

	cfg := &Config{IPLimitTTL: "3m", APIKey: "master-cluster-secret"}
	module := NewModule(cfg, database.NewRegistry(db), &vpn.NoopEngine{}, &vpn.NoopEngine{}, nil, nil, slog.Default())
	ctx := startModuleAnalyzer(t, module, cfg, 3)
	plugin := &Plugin{module: module}

	err := plugin.IngestEvents(ctx, "slave-a", []pluginapi.FraudEvent{{
		Email:      "user@example.test",
		IP:         newState("slave-cluster-secret").HashIP("203.0.113.7"),
		OccurredAt: time.Now().UTC(),
		HashKeyID:  newState("slave-cluster-secret").HashKeyID(),
	}})
	require.ErrorContains(t, err, "IP hash key mismatch")
	require.Zero(t, module.state.ActiveIPCount("user@example.test"))
}

// TestModule_IngestEvents_FullChannel_DoesNotBlock проверяет что при
// переполненном канале IngestEvents не блокирует вызывающую горутину.
func TestModule_IngestEvents_FullChannel_DoesNotBlock(t *testing.T) {
	db := setupTestDB(t)
	defer closeSQLite(t, db)

	m := NewModule(&Config{}, database.NewRegistry(db), &vpn.NoopEngine{}, &vpn.NoopEngine{}, nil, nil, slog.Default())
	m.readyMu.Do(func() { close(m.ready) })

	// Заполняем канал до предела
	for i := 0; i < cap(m.eventCh); i++ {
		m.eventCh <- event{email: "filler@x.com", ip: generateIP(i)}
	}
	assert.Equal(t, cap(m.eventCh), len(m.eventCh), "pre-condition: channel must be full")

	// IngestEvents должен вернуться мгновенно, не блокируясь
	errs := make(chan error, 1)
	go func() {
		errs <- m.IngestEvents(context.Background(), "1.1.1.1", []domain.FraudEvent{{Email: "new@x.com", IP: "5.5.5.5"}})
	}()

	select {
	case err := <-errs:
		require.Error(t, err)
		// ok — вернулся без блокировки
	case <-time.After(500 * time.Millisecond):
		t.Fatal("IngestEvents blocked on full channel")
	}
}

// TestModule_IngestEvents_ConcurrentCalls проверяет thread-safety IngestEvents
// при параллельных вызовах из нескольких горутин (запускать с -race).
func TestModule_IngestEvents_ConcurrentCalls(t *testing.T) {
	db := setupTestDB(t)
	defer closeSQLite(t, db)

	cfg := &Config{IPLimitTTL: "3m"}
	m := NewModule(cfg, database.NewRegistry(db), &vpn.NoopEngine{}, &vpn.NoopEngine{}, nil, nil, slog.Default())
	ctx := startModuleAnalyzer(t, m, cfg, 3)
	errs := make(chan error, 20)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			errs <- m.IngestEvents(ctx, "slave-a", []domain.FraudEvent{{
				Email:      fmt.Sprintf("u%d@x.com", n),
				IP:         m.state.HashIP(generateIP(n)),
				OccurredAt: time.Now().UTC(),
			}})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// fraudReason — форматирование
// ─────────────────────────────────────────────────────────────────────────────

func TestFraudReason_SingleDevice(t *testing.T) {
	r := fraudReason("u@x.com", 4, 3, 3, 1, 3*time.Minute)
	assert.Contains(t, r, "4 unique IPs")
	assert.Contains(t, r, "limit 3 = 3 base × 1 devices")
	assert.Contains(t, r, "u@x.com")
}

func TestFraudReason_MultiDevice(t *testing.T) {
	r := fraudReason("u@x.com", 7, 6, 3, 2, 5*time.Minute)
	assert.Contains(t, r, "7 unique IPs")
	assert.Contains(t, r, "limit 6 = 3 base × 2 devices")
}

func TestFraudReason_LargeValues(t *testing.T) {
	r := fraudReason("vip@x.com", 25, 20, 4, 5, 10*time.Minute)
	assert.Contains(t, r, "25 unique IPs")
	assert.Contains(t, r, "limit 20 = 4 base × 5 devices")
	assert.Contains(t, r, "vip@x.com")
}

// ─────────────────────────────────────────────────────────────────────────────
// Интеграционный сценарий: slave-события вызывают бан на master
// ─────────────────────────────────────────────────────────────────────────────

// TestIntegration_SlaveEvents_TriggerBan — сквозной тест:
// master получает IP-события через IngestEvents (симуляция slave → master),
// они обрабатываются анализатором, и пользователь получает бан.
func TestIntegration_SlaveEvents_TriggerBan(t *testing.T) {
	db := setupTestDB(t)
	defer closeSQLite(t, db)

	email := "slaveuser@x.com"
	db.Create(&database.User{ID: "u-si"})
	db.Create(&database.Subscription{
		ID:         "sub-si",
		UserID:     "u-si",
		Email:      email,
		Status:     "active",
		MaxDevices: 1, // threshold = maxIPs(2) * 1 = 2
	})

	cfg := &Config{
		Enabled:               true,
		BanDuration:           "1h",
		IPLimitTTL:            "3m",
		SuspiciousIPThreshold: 2,
	}

	m := NewModule(cfg, database.NewRegistry(db), &vpn.NoopEngine{}, &vpn.NoopEngine{}, nil, nil, slog.Default())

	// Стартуем только анализатор (без tailer и rotator)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.readyMu.Do(func() { close(m.ready) })

	an := newAnalyzer(cfg, m.state, m.banStore, m.eventCh, 3*time.Minute, 2, database.NewRegistry(db), &vpn.NoopEngine{}, nil, nil, slog.Default())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		an.run(ctx)
	}()
	defer wg.Wait()
	defer cancel()

	// Инжектируем события как если бы они пришли со slave
	require.NoError(t, m.IngestEvents(ctx, "slave-a", []domain.FraudEvent{
		{Email: email, IP: m.state.HashIP("10.1.1.1"), OccurredAt: time.Now().UTC()},
		{Email: email, IP: m.state.HashIP("10.1.1.2"), OccurredAt: time.Now().UTC()},
	}))
	// Пока не бан — порог не превышен

	require.Eventually(t, func() bool {
		return m.state.ActiveIPCount(email) == 2
	}, 1*time.Second, 10*time.Millisecond, "Events must be processed")

	assert.False(t, m.IsBanned(email), "2 IPs at threshold 2 must not ban")

	// Третий IP — должен вызвать бан
	require.NoError(t, m.IngestEvents(ctx, "slave-a", []domain.FraudEvent{{
		Email: email, IP: m.state.HashIP("10.1.1.3"), OccurredAt: time.Now().UTC(),
	}}))

	// Даём анализатору время среагировать
	require.Eventually(t, func() bool {
		return m.IsBanned(email)
	}, 2*time.Second, 50*time.Millisecond, "user must be banned after slave events pushed 3rd IP")
}

// ─────────────────────────────────────────────────────────────────────────────
// Benchmarks
// ─────────────────────────────────────────────────────────────────────────────

// BenchmarkGetDeviceLimit_CacheHit измеряет скорость горячего пути (чтение из кэша).
func BenchmarkGetDeviceLimit_CacheHit(b *testing.B) {
	db := setupTestDB(b)
	defer func() { sqlDB, _ := db.DB(); sqlDB.Close() }()

	db.Create(&database.User{ID: "u-bench"})
	db.Create(&database.Subscription{
		ID:         "sub-bench",
		UserID:     "u-bench",
		Email:      "bench@x.com",
		Status:     "active",
		MaxDevices: 5,
	})

	an := newAnalyzer(&Config{}, newState("dummy-test-key"), newBanStore(), nil, time.Minute, 3, database.NewRegistry(db), &vpn.NoopEngine{}, nil, nil, slog.Default())
	an.refreshDeviceCache()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = an.getDeviceLimit("bench@x.com")
	}
}

// BenchmarkHandleEvent_WithDeviceCache измеряет handleEvent с динамическим
// порогом (кэш тёплый).
func BenchmarkHandleEvent_WithDeviceCache(b *testing.B) {
	db := setupTestDB(b)
	defer func() { sqlDB, _ := db.DB(); sqlDB.Close() }()

	db.Create(&database.User{ID: "u-bh"})
	db.Create(&database.Subscription{
		ID: "sub-bh", UserID: "u-bh",
		Email: "benchhandle@x.com", Status: "active", MaxDevices: 3,
	})

	cfg := &Config{BanDuration: "1h"}
	an := newAnalyzer(cfg, newState("dummy-test-key"), newBanStore(), nil, 3*time.Minute, 3, database.NewRegistry(db), &vpn.NoopEngine{}, nil, nil, slog.Default())
	an.refreshDeviceCache()

	ips := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		an.handleEvent(event{email: "benchhandle@x.com", ip: ips[i%len(ips)]})
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// closeSQLite закрывает соединение с in-memory SQLite в тестах.
func closeSQLite(tb testing.TB, db *gorm.DB) {
	tb.Helper()
	sqlDB, err := db.DB()
	if err == nil {
		_ = sqlDB.Close()
	}
}
