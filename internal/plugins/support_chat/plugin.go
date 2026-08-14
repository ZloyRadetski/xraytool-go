package support_chat

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"xraytool/internal/pluginapi"

	"gorm.io/gorm"
)

// Provider implements the SupportChatProvider interface (publishes service).
type Provider struct {
	db     *gorm.DB
	crypto *Crypto
}

// UnreadCount returns the number of unread messages for a user.
func (p *Provider) UnreadCount(ctx context.Context, userID string) (int, error) {
	if p.db == nil {
		return 0, fmt.Errorf("support_chat: not initialised")
	}

	var count int64
	if p.crypto == nil {
		return 0, fmt.Errorf("support_chat: crypto not initialised")
	}
	userIDHash := p.crypto.BlindIndex("conversation-user-id", userID)
	err := p.db.WithContext(ctx).Model(&Message{}).
		Joins("JOIN conversations ON conversations.id = messages.conversation_id").
		Where("(conversations.user_id_hash = ? OR (conversations.encryption_version < ? AND conversations.user_id = ?)) AND messages.read_at IS NULL AND messages.sender_role = ?", userIDHash, currentEncryptionVersion, userID, "admin").
		Count(&count).Error

	return int(count), err
}

// IsBanned reports whether the user is actively banned in the support chat system.
func (p *Provider) IsBanned(ctx context.Context, userID string) (bool, error) {
	if p.db == nil {
		return false, fmt.Errorf("support_chat: not initialised")
	}
	if p.crypto == nil {
		return false, fmt.Errorf("support_chat: crypto not initialised")
	}
	userIDHash := p.crypto.BlindIndex("support-ban-user-id", userID)
	var count int64
	err := p.db.WithContext(ctx).Model(&SupportBan{}).
		Where("user_id_hash = ? AND (expires_at IS NULL OR expires_at > ?)", userIDHash, time.Now()).
		Count(&count).Error
	return count > 0, err
}


// Plugin implements pluginapi.Plugin, pluginapi.HTTPContributor, and pluginapi.ServiceProvider.
type Plugin struct {
	log      pluginapi.Logger
	cfg      pluginConfig
	db       *gorm.DB
	provider *Provider
	users    supportUserLookup
	// authMiddleware is supplied by api_server. Every HTTP endpoint in this
	// plugin is an internal API endpoint and must reject direct, unauthenticated
	// requests before considering caller-supplied identity headers.
	authMiddleware func(http.Handler) http.Handler
	store          *Store
	wg             sync.WaitGroup
	hub            *Hub
}

// supportUserLookup deliberately contains only the identity lookups the
// support boundary needs. Keeping it small makes the authorization rules easy
// to test without coupling handlers to the complete repository contract.
type supportUserLookup interface {
	FindByEmailOrUsername(ctx context.Context, email string) (*pluginapi.User, error)
	FindByTelegramID(ctx context.Context, telegramID int64) (*pluginapi.User, error)
	FindByPlatformID(ctx context.Context, platform, id string) (*pluginapi.User, error)
}

// New creates an uninitialised plugin. Call via BuiltinRegistry factory.
func New() *Plugin {
	return &Plugin{
		provider: &Provider{},
	}
}

// ── pluginapi.Plugin ──────────────────────────────────────────────────────────

func (p *Plugin) Metadata() pluginapi.Metadata {
	return pluginapi.Metadata{
		Name:        "support_chat",
		Kind:        "support",
		Version:     "1.0.0",
		APIVersion:  pluginapi.CurrentAPIVersion,
		Description: "Encrypted support chat between authenticated users and admins.",
		Mandatory:   false,
		Publishes:   []pluginapi.ServiceRef{{Name: "support_chat_provider"}},
		Requires: []pluginapi.ServiceRef{
			{Name: "user_repository", Optional: false},
			{Name: pluginapi.ServiceProtectedMiddleware},
		},
	}
}

func (p *Plugin) Init(ctx context.Context, rawCfg pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	p.log = reg.Logger()

	cfg, err := parseConfig(rawCfg)
	if err != nil {
		return fmt.Errorf("support_chat: config error: %w", err)
	}
	p.cfg = cfg

	crypto, err := NewCrypto(cfg.MasterKey)
	if err != nil {
		return fmt.Errorf("support_chat: crypto error: %w", err)
	}
	legacyCryptos := make([]*Crypto, 0, len(cfg.LegacyMasterKeys))
	for _, legacyMasterKey := range cfg.LegacyMasterKeys {
		legacyCrypto, err := NewCrypto(legacyMasterKey)
		if err != nil {
			return fmt.Errorf("support_chat: legacy key config error: %w", err)
		}
		legacyCryptos = append(legacyCryptos, legacyCrypto)
	}

	// Open isolated database only after all key material has been validated.
	db, err := openDB(cfg.Database)
	if err != nil {
		return fmt.Errorf("support_chat: db error: %w", err)
	}
	p.db = db
	p.provider.db = db
	p.provider.crypto = crypto
	p.store = NewStore(db, crypto, WithLegacyCryptos(legacyCryptos...), WithKeyVersion(cfg.KeyVersion))
	if cfg.MigrateLegacyData {
		stats, err := p.store.MigrateLegacyData(ctx, cfg.Media.StoragePath)
		if err != nil {
			return fmt.Errorf("support_chat: migrate legacy data: %w", err)
		}
		if stats.Conversations+stats.Messages+stats.Attachments > 0 {
			p.log.Info("support_chat: encrypted legacy data", "conversations", stats.Conversations, "messages", stats.Messages, "attachments", stats.Attachments)
		}
	}

	// Resolve UserRepository
	userRepo, err := reg.Resolve("user_repository")
	if err != nil {
		return fmt.Errorf("support_chat: failed to resolve user_repository: %w", err)
	}
	if ur, ok := userRepo.(pluginapi.UserRepository); ok {
		p.users = ur
	} else {
		return fmt.Errorf("support_chat: user_repository has wrong type")
	}

	authMw, err := reg.Resolve(pluginapi.ServiceProtectedMiddleware)
	if err != nil {
		return fmt.Errorf("support_chat: failed to resolve protected middleware: %w", err)
	}
	protected, ok := authMw.(func(http.Handler) http.Handler)
	if !ok || protected == nil {
		return fmt.Errorf("support_chat: protected middleware has wrong type")
	}
	p.authMiddleware = protected

	p.hub = newHub(p.log)

	p.log.Info("support_chat: initialised", "driver", cfg.Database.Driver)
	return nil
}

func (p *Plugin) Start(ctx context.Context) error {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		p.hub.run(ctx.Done())
	}()

	// TODO: start cleanup worker
	<-ctx.Done()
	return nil
}

func (p *Plugin) Stop(_ context.Context) error {
	p.wg.Wait()

	if p.db != nil {
		if sqlDB, err := p.db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}
	return nil
}

func (p *Plugin) Health(_ context.Context) error {
	if p.db == nil || p.users == nil || p.authMiddleware == nil {
		return fmt.Errorf("support_chat: not initialised")
	}
	if p.cfg.MasterKey == "" {
		return fmt.Errorf("support_chat: master_key is empty")
	}
	if sqlDB, err := p.db.DB(); err == nil {
		if err := sqlDB.Ping(); err != nil {
			return fmt.Errorf("support_chat: db ping failed: %w", err)
		}
	}
	return nil
}

// ── pluginapi.ServiceProvider ─────────────────────────────────────────────────

func (p *Plugin) PublishedServices() map[string]any {
	return map[string]any{
		"support_chat_provider": p.provider,
	}
}

// ── pluginapi.HTTPContributor ────────────────────────────────────────────────

func (p *Plugin) RegisterRoutes(mux *http.ServeMux) {
	protected := func(handler http.HandlerFunc) http.Handler {
		return p.authMiddleware(handler)
	}
	adminOnly := func(handler http.HandlerFunc) http.Handler {
		return p.authMiddleware(p.requireAdmin(handler))
	}

	// Client routes
	mux.Handle("POST /api/v1/support/conversations", protected(p.handleClientCreateConversation()))
	mux.Handle("GET /api/v1/support/conversations", protected(p.handleClientListConversations()))
	mux.Handle("GET /api/v1/support/conversations/{id}", protected(p.handleClientGetConversation()))
	mux.Handle("DELETE /api/v1/support/conversations/{id}", protected(p.handleClientDeleteConversation()))
	mux.Handle("GET /api/v1/support/conversations/{id}/messages", protected(p.handleClientListMessages()))
	mux.Handle("POST /api/v1/support/conversations/{id}/messages", protected(p.handleClientCreateMessage()))
	mux.Handle("POST /api/v1/support/attachments", protected(p.handleUploadAttachment()))
	mux.Handle("GET /api/v1/support/attachments/{id}/download", protected(p.handleDownloadAttachment()))
	mux.Handle("GET /api/v1/support/ban-status", protected(p.handleClientGetBanStatus()))

	// Admin routes
	mux.Handle("GET /api/v1/admin/support/conversations", adminOnly(p.handleAdminListConversations()))
	mux.Handle("DELETE /api/v1/admin/support/conversations/{id}", adminOnly(p.handleAdminDeleteConversation()))
	mux.Handle("GET /api/v1/admin/support/conversations/{id}/messages", adminOnly(p.handleAdminListMessages()))
	mux.Handle("POST /api/v1/admin/support/conversations/{id}/messages", adminOnly(p.handleAdminCreateMessage()))
	mux.Handle("PATCH /api/v1/admin/support/conversations/{id}/status", adminOnly(p.handleAdminPatchStatus()))
	mux.Handle("GET /api/v1/admin/support/bans", adminOnly(p.handleAdminListBans()))
	mux.Handle("POST /api/v1/admin/support/bans", adminOnly(p.handleAdminCreateBan()))
	mux.Handle("GET /api/v1/admin/support/bans/{user_id}", adminOnly(p.handleAdminGetBan()))
	mux.Handle("DELETE /api/v1/admin/support/bans/{user_id}", adminOnly(p.handleAdminDeleteBan()))

	// WebSocket routes
	mux.Handle("GET /api/v1/support/conversations/{id}/ws", protected(p.serveWs("client")))
	mux.Handle("GET /api/v1/admin/support/ws", adminOnly(p.serveWs("admin")))
}

// Compile-time interface checks.
var _ pluginapi.Plugin = (*Plugin)(nil)
var _ pluginapi.HTTPContributor = (*Plugin)(nil)
var _ pluginapi.ServiceProvider = (*Plugin)(nil)
