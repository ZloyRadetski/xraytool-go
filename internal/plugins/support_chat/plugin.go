package support_chat

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"xraytool/internal/pluginapi"

	"gorm.io/gorm"
)

// Provider implements the SupportChatProvider interface (publishes service).
type Provider struct {
	db *gorm.DB
}

// UnreadCount returns the number of unread messages for a user.
func (p *Provider) UnreadCount(ctx context.Context, userID string) (int, error) {
	if p.db == nil {
		return 0, fmt.Errorf("support_chat: not initialised")
	}

	var count int64
	err := p.db.WithContext(ctx).Model(&Message{}).
		Joins("JOIN conversations ON conversations.id = messages.conversation_id").
		Where("conversations.user_id = ? AND messages.read_at IS NULL AND messages.sender_role = ?", userID, "admin").
		Count(&count).Error

	return int(count), err
}

// Plugin implements pluginapi.Plugin, pluginapi.HTTPContributor, and pluginapi.ServiceProvider.
type Plugin struct {
	log      pluginapi.Logger
	cfg      pluginConfig
	db       *gorm.DB
	provider *Provider
	users    pluginapi.UserRepository
	store    *Store
	wg       sync.WaitGroup
	hub      *Hub
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
		Requires:    []pluginapi.ServiceRef{{Name: "user_repository", Optional: false}},
	}
}

func (p *Plugin) Init(ctx context.Context, rawCfg pluginapi.RawConfig, reg pluginapi.ServiceResolver) error {
	p.log = reg.Logger()

	cfg, err := parseConfig(rawCfg)
	if err != nil {
		return fmt.Errorf("support_chat: config error: %w", err)
	}
	p.cfg = cfg

	// Open isolated database
	db, err := openDB(cfg.Database)
	if err != nil {
		return fmt.Errorf("support_chat: db error: %w", err)
	}
	p.db = db
	p.provider.db = db // Inject DB to provider

	// Init Crypto & Store
	crypto, err := NewCrypto(cfg.MasterKey)
	if err != nil {
		return fmt.Errorf("support_chat: crypto error: %w", err)
	}
	p.store = NewStore(db, crypto)

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
	if p.db == nil {
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
	// Client routes
	mux.HandleFunc("POST /api/v1/support/conversations", p.handleClientCreateConversation())
	mux.HandleFunc("GET /api/v1/support/conversations", p.handleClientListConversations())
	mux.HandleFunc("GET /api/v1/support/conversations/{id}", p.handleClientGetConversation())
	mux.HandleFunc("GET /api/v1/support/conversations/{id}/messages", p.handleClientListMessages())
	mux.HandleFunc("POST /api/v1/support/conversations/{id}/messages", p.handleClientCreateMessage())
	mux.HandleFunc("POST /api/v1/support/attachments", p.handleUploadAttachment())
	mux.HandleFunc("GET /api/v1/support/attachments/{id}/download", p.handleDownloadAttachment())

	// Admin routes
	mux.HandleFunc("GET /api/v1/admin/support/conversations", p.handleAdminListConversations())
	mux.HandleFunc("GET /api/v1/admin/support/conversations/{id}/messages", p.handleAdminListMessages())
	mux.HandleFunc("POST /api/v1/admin/support/conversations/{id}/messages", p.handleAdminCreateMessage())
	mux.HandleFunc("PATCH /api/v1/admin/support/conversations/{id}/status", p.handleAdminPatchStatus())

	// WebSocket routes
	mux.HandleFunc("GET /api/v1/support/conversations/{id}/ws", p.serveWs("client"))
	mux.HandleFunc("GET /api/v1/admin/support/ws", p.serveWs("admin"))
}

// Compile-time interface checks.
var _ pluginapi.Plugin = (*Plugin)(nil)
var _ pluginapi.HTTPContributor = (*Plugin)(nil)
var _ pluginapi.ServiceProvider = (*Plugin)(nil)
