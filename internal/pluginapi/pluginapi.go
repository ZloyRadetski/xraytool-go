// Package pluginapi defines the extension-point contracts (interfaces, types, metadata)
// shared by the Plugin Host (kernel) and all plugins — both internal (compiled-in) and
// external (separate process via go-plugin/gRPC).
//
// Design rules:
//   - This package MUST NOT import any other xraytool-internal package.
//     It may import standard library and third-party packages already in go.mod.
//   - All Go interfaces here are the single source of truth for plugin contracts.
//     Future external-plugin support will add .proto files that mirror these interfaces;
//     until then the Go interfaces are sufficient for internal (compiled-in) plugins.
//   - Plugins are initialised once: they resolve dependencies during Init() and store them
//     as struct fields. ServiceResolver is NOT in the hot path — it is only called at
//     startup, not on every request.
package pluginapi

import (
	"context"
	"io/fs"
	"net/http"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Core metadata and lifecycle
// ─────────────────────────────────────────────────────────────────────────────

// RawConfig is the plugin's configuration section from plugins.yaml,
// decoded as a generic map. Each plugin is responsible for unmarshalling
// it into its own typed config struct inside Init().
type RawConfig map[string]any

// Stable Service Registry names shared by kernel code and plugins. Keeping
// them in pluginapi prevents the composition root from importing an optional
// plugin package merely to obtain a string constant.
const (
	ServiceClusterSyncProvider = "cluster_sync_provider"
)

// ServiceRef declares a named service dependency or publication.
type ServiceRef struct {
	// Name is the stable identifier used in ServiceRegistry.Publish / Resolve.
	// Examples: "subscription_repository", "engine.softban", "pricing_engine".
	Name string

	// Optional, when true, means the plugin starts and degrades gracefully if
	// this service is not published by any loaded plugin. When false, Host.Load()
	// fails with a clear error before any plugin is started.
	Optional bool
}

// Metadata describes a plugin and its dependencies.
// Host.Load() reads this before calling Init() on any plugin.
type Metadata struct {
	// Name is the stable, lowercase identifier used in plugins.yaml and CLI commands.
	// Example: "antifraud", "payment_platega", "core".
	Name string

	// Kind is the extension-point category. One of:
	// "core" | "engine" | "antifraud" | "payment" | "pricing" |
	// "notification" | "event_sink" | "cluster_sync"
	Kind string

	// Version is the plugin's own semver.
	Version string

	// APIVersion is the pluginapi contract version this plugin was compiled against.
	// Host refuses to load a plugin whose APIVersion > host's supported APIVersion.
	APIVersion string

	// Description is a human-readable summary shown by `xraytool plugin list`.
	Description string

	// Mandatory, when true (only for the core plugin), means Host.Load() refuses to
	// start if this plugin is absent or has enabled:false in plugins.yaml.
	Mandatory bool

	// Publishes lists the service names this plugin registers in ServiceRegistry
	// after a successful Init(). Other plugins may then Resolve() these names.
	Publishes []ServiceRef

	// Requires lists the service names this plugin needs from ServiceRegistry
	// before it can be initialised. Host.Load() validates the entire dependency
	// graph before calling Init() on any plugin.
	Requires []ServiceRef
}

// Plugin is the universal lifecycle interface implemented by every plugin,
// regardless of kind. The host calls these methods in the order below.
//
//	Init → Start (goroutine) → [running] → Stop
type Plugin interface {
	// Metadata returns the plugin's static descriptor.
	// Called once by Host.Load() before Init().
	Metadata() Metadata

	// Init receives the plugin's validated configuration and a ServiceResolver
	// scoped to this plugin. The plugin resolves its Requires here and stores
	// the results as struct fields — it MUST NOT call Resolve() after Init returns.
	//
	// Init is called in dependency order (topological sort of the Requires graph).
	// It must return quickly; long-running work belongs in Start().
	Init(ctx context.Context, cfg RawConfig, reg ServiceResolver) error

	// Start runs the plugin's long-lived work (background goroutines, watchers, etc.).
	// It must block until ctx is cancelled and then return promptly.
	// The host runs Start in a separate goroutine for each plugin.
	Start(ctx context.Context) error

	// Stop signals the plugin to perform graceful shutdown within the deadline
	// imposed by the context (typically 30 s). Called by the host in reverse
	// load order; core plugin is stopped last.
	Stop(ctx context.Context) error

	// Health returns nil if the plugin is operating normally, or a descriptive
	// error if it is degraded or unavailable. Called periodically by the host's
	// health monitor and exposed via `xraytool plugin list`.
	Health(ctx context.Context) error
}

// ServiceProvider is implemented by plugins that publish named services for
// other plugins. The Host reads this map immediately after a successful Init
// and publishes its entries declared in Metadata.Publishes.
//
// Keeping publication separate from ServiceResolver is deliberate: a plugin
// may resolve only declared dependencies, while the Host remains the single
// authority that makes a service visible to other plugins.
//
// Every key returned here must be declared in Metadata.Publishes, and every
// declared publication must have a non-nil value in this map. The Host rejects
// a mismatch during Load before starting any plugin.
type ServiceProvider interface {
	Plugin

	PublishedServices() map[string]any
}

// ─────────────────────────────────────────────────────────────────────────────
// ServiceResolver — the narrow interface the host passes to each plugin's Init()
// ─────────────────────────────────────────────────────────────────────────────

// Logger is a minimal structured-logging interface that the host supplies to
// each plugin. Plugins MUST use this rather than creating their own root logger
// so that the host can attach plugin-name fields and control log levels uniformly.
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// PluginDBHandle is a database handle scoped to a single plugin.
// The handle exposes the raw *sql.DB so the plugin can run its own migrations
// and queries, but the naming convention (plugin-specific migration table,
// e.g. schema_migrations_antifraud) is enforced by the host, not by this type.
//
// NOTE: direct cross-plugin table access is forbidden by convention and will be
// enforced by code review / linting. Use ServiceRegistry instead.
type PluginDBHandle interface {
	// PluginName returns the plugin this handle belongs to.
	PluginName() string

	// RunMigrations applies all pending migrations from the given filesystem path.
	// The host calls this automatically for enabled plugins before Init().
	RunMigrations(ctx context.Context, migrationsPath string) error
}

// MigrationSet describes migration files compiled into an internal plugin.
//
// Internal plugins use an embedded filesystem rather than a path relative to
// the process working directory: production binaries are normally installed
// without the repository tree. The Dir must contain versioned *.up.sql files
// (for example, 000001_initial.up.sql). The host runs this set before Init for
// an enabled plugin when its database handle supports EmbeddedMigrationRunner.
//
// External plugins can keep using PluginDBHandle.RunMigrations with a local
// filesystem path under their own deployment directory.
type MigrationSet struct {
	FS  fs.FS
	Dir string
}

// MigrationProvider is an optional plugin capability. Built-in plugins that
// own persistent state expose their embedded migration directory through this
// interface; stateless plugins may expose a marker migration as well, which
// gives every enabled plugin an independent version namespace.
type MigrationProvider interface {
	PluginMigrations() MigrationSet
}

// EmbeddedMigrationRunner is implemented by database handles that can run an
// embedded MigrationSet. It remains separate from PluginDBHandle so existing
// external handles that only support filesystem paths stay source-compatible.
type EmbeddedMigrationRunner interface {
	RunEmbeddedMigrations(ctx context.Context, migrations MigrationSet) error
}

// ServiceResolver is the limited view of ServiceRegistry that the host exposes
// to a plugin during Init(). A plugin may only resolve services it declared in
// Metadata().Requires; any other Resolve() call returns an error.
type ServiceResolver interface {
	// Resolve returns the service published under name by another plugin.
	// Returns an error if the service was not declared in Metadata().Requires
	// or if the publishing plugin was not loaded.
	Resolve(name string) (any, error)

	// Logger returns a structured logger pre-tagged with the plugin's name.
	Logger() Logger

	// EmitEvent publishes a domain event to the kernel's event bus.
	// The bus fans it out to all registered EventSink plugins.
	EmitEvent(eventType string, data map[string]any, userMeta map[string]any)

	// DB returns a database handle scoped to this plugin.
	DB() PluginDBHandle
}

// ─────────────────────────────────────────────────────────────────────────────
// Extension Point 0 — CoreProvider (mandatory, always builtin)
// ─────────────────────────────────────────────────────────────────────────────

// UserRepository is the persistence port for User entities.
// Mirrors the current domain.UserRepository; declared here so plugins that need
// user data depend on this pluginapi type, not on domain directly.
type UserRepository interface {
	FindByID(ctx context.Context, id string) (*User, error)
	FindByEmailOrUsername(ctx context.Context, email string) (*User, error)
	FindByTelegramID(ctx context.Context, tgID int64) (*User, error)
	FindByRefCode(ctx context.Context, code string) (*User, error)
	Create(ctx context.Context, user *User) error
	Update(ctx context.Context, user *User) error
	Delete(ctx context.Context, id string) error

	FindAll(ctx context.Context) ([]User, error)
	ListUsers(ctx context.Context, page, limit int, search string) ([]User, int64, error)
	DeleteUserAndData(ctx context.Context, userID string) error
	FindByPlatformID(ctx context.Context, platform, id string) (*User, error)
	AddReferralReward(ctx context.Context, referrerID string, referredID string, paymentID int64, reward int) error
	CountReferrals(ctx context.Context, referrerID string) (int64, error)
	CountReferralRewards(ctx context.Context, referrerID string) (int64, error)
	SumReferralRewards(ctx context.Context, referrerID string) (int64, error)
	GetReferralStats(ctx context.Context, referrerIDs []string) ([]ReferralStats, error)
	CountByRefCode(ctx context.Context, code string) (int64, error)
	FindAdmins(ctx context.Context) ([]User, error)
	AdjustBalance(ctx context.Context, userID string, amount int) error
	UpdateIsBlocked(ctx context.Context, userID string, isBlocked bool) error
}

// SubscriptionRepository is the persistence port for Subscription entities.
type SubscriptionRepository interface {
	FindByID(ctx context.Context, id string) (*Subscription, error)
	FindByEmail(ctx context.Context, email string) (*Subscription, error)
	FindByUserID(ctx context.Context, userID string) ([]Subscription, error)
	Create(ctx context.Context, sub *Subscription) error
	Update(ctx context.Context, sub *Subscription) error
	Delete(ctx context.Context, id string) error

	FindAll(ctx context.Context) ([]Subscription, error)
	FindByStatus(ctx context.Context, status string) ([]Subscription, error)
	GetMasterSnapshot(ctx context.Context) ([]Subscription, error)
	FindByClientIdentifier(ctx context.Context, clientID string) (*Subscription, error)
	FindLatestByEmail(ctx context.Context, email string) (*Subscription, error)
	UpdateStatusIfActive(ctx context.Context, id string, newStatus string) (bool, error)
	UpdateFields(ctx context.Context, id string, updates map[string]interface{}) error
	FindLatestByUserID(ctx context.Context, userID string) (*Subscription, error)
	FindLatestByUserIDs(ctx context.Context, userIDs []string) ([]Subscription, error)
	UpdateMaxDevicesByUserID(ctx context.Context, userID string, maxDevices int) error
	UpdateAutoRenewByUserID(ctx context.Context, userID string, autoRenew bool) error
	AutoRenewSubscription(ctx context.Context, userID string, planID *int64, planTotalPrice int, newEndsAt *time.Time, maxDevices int) error
	GetAllEmailsAndMaxDevices(ctx context.Context) ([]EmailAndMaxDevice, error)
}

// DeviceRepository is the persistence port for Device entities.
type DeviceRepository interface {
	TrackDevice(ctx context.Context, subID string, hwid, deviceModel, deviceOs, userAgent string, deviceLimit int) (deviceLimitReached bool, err error)
	CountBySubscriptions(ctx context.Context, subIDs []string) (map[string]int64, error)
	FindOldestBySubscription(ctx context.Context, subID string, limit int) ([]Device, error)
	DeleteByIDs(ctx context.Context, ids []int64) error
	CountBySubscription(ctx context.Context, subID string) (int64, error)
	FindBySubscriptionID(ctx context.Context, subID string) ([]Device, error)
	FindByIDAndSubscription(ctx context.Context, deviceID int64, subID string) (*Device, error)
	Delete(ctx context.Context, id int64) error
}

// PlanRepository is the persistence port for Plan entities.
type PlanRepository interface {
	FindByID(ctx context.Context, id string) (*Plan, error)
	FindAll(ctx context.Context) ([]Plan, error)
	FindActive(ctx context.Context) ([]Plan, error)
	Create(ctx context.Context, plan *Plan) error
	Update(ctx context.Context, plan *Plan) error
	Delete(ctx context.Context, id string) error
}

// CoreProvider is the mandatory plugin that owns the user/subscription/device/plan
// domain. It publishes the repositories and business-orchestration methods that
// all other plugins depend on via ServiceRegistry.
//
// Published service names (Metadata.Publishes):
//
//	"user_repository", "subscription_repository", "device_repository",
//	"plan_repository", "payment_recorder", "subscription_lifecycle"
type CoreProvider interface {
	Plugin

	// Repository accessors — also published individually via ServiceRegistry.
	UserRepository() UserRepository
	SubscriptionRepository() SubscriptionRepository
	DeviceRepository() DeviceRepository
	PlanRepository() PlanRepository

	// Business-orchestration methods that must remain in core so that every
	// payment provider uses the same subscription-extension rules.
	ExtendSubscription(ctx context.Context, subID string, months int) error
	ApplyReferralReward(ctx context.Context, paymentID int64) error
}

// ─────────────────────────────────────────────────────────────────────────────
// Extension Point 1 — AntifraudProvider
// ─────────────────────────────────────────────────────────────────────────────

// FraudEvent is a single suspicious IP observation reported by a slave node or
// read from the engine's access log.
type FraudEvent struct {
	Email string
	IP    string
}

// BanUpdateSink is the reverse channel through which an AntifraudProvider
// pushes ban/unban decisions to the kernel's local cache (LocalBanCache).
// The kernel subscribes to this sink when the plugin starts; the plugin calls
// PushBanUpdate/PushUnban whenever it makes a decision.
//
// This is the "push, not pull" pattern described in section 3.4 of the plan:
// IsBanned() in the hot path always reads the LOCAL kernel cache — zero network
// calls — and the antifraud plugin is the only writer.
type BanUpdateSink interface {
	PushBanUpdate(email string, bannedUntil time.Time)
	PushUnban(email string)
}

// AntifraudProvider is the extension point for the anti-fraud subsystem.
//
// Required services (Metadata.Requires):
//
//	"subscription_repository" (from core, optional:false)
//	"engine.softban"          (from active engine plugin, optional:true)
//	"engine.logger_control"   (from active engine plugin, optional:true)
//	"event_propagator"        (optional:true)
type AntifraudProvider interface {
	Plugin

	// SetBanSink registers the kernel's local ban cache as the target for
	// push updates. Called by the host immediately after Init(), before Start().
	SetBanSink(sink BanUpdateSink)

	// IsBanned is called SYNCHRONOUSLY in the subscription hot-path.
	// Implementation MUST read from the local in-memory store set up by SetBanSink
	// and return immediately with no I/O. This contract is enforced by the
	// "push, not pull" architecture (plan section 3.4).
	IsBanned(email string) bool

	// ForceUnban lifts an active ban immediately (admin action).
	ForceUnban(ctx context.Context, email string) error

	// Snapshot returns the current state of the ban store for diagnostics.
	Snapshot(ctx context.Context) (map[string]any, error)

	// IngestEvents processes a batch of suspicious-IP events from slave nodes.
	// Only called on the master; the host sets ingestFn to nil on slaves.
	IngestEvents(ctx context.Context, sourceID string, events []FraudEvent) error
}

// ─────────────────────────────────────────────────────────────────────────────
// Extension Point 2 — PaymentProvider
// ─────────────────────────────────────────────────────────────────────────────

// PaymentIntentRequest carries the parameters for initiating a payment.
type PaymentIntentRequest struct {
	UserID      string
	Amount      int    // in minor units (kopecks, cents, etc.)
	Currency    string // ISO 4217
	Description string
	ExternalRef string
	CustomData  map[string]any
}

// PaymentIntentResult is returned by CreateIntent.
type PaymentIntentResult struct {
	// ExternalID is the payment gateway's transaction identifier.
	ExternalID string
	// PaymentURL is the URL the user should be redirected to for completion.
	PaymentURL string
	// RawResponse may carry provider-specific fields for debugging.
	RawResponse map[string]any
}

// PaymentCallbackResult is the normalised outcome of a verified webhook callback.
// The core plugin uses this to decide whether to extend the subscription.
type PaymentCallbackResult struct {
	ExternalID string
	Status     string // "completed" | "failed" | "refunded"
	Amount     int
	Currency   string
	CustomData map[string]any
}

// PaymentProvider is the extension point for a single payment gateway.
// Multiple PaymentProvider plugins may be loaded simultaneously (e.g. platega +
// yookassa); the core plugin dispatches CreatePayment to the correct provider by
// MethodID.
//
// IMPORTANT: subscription extension, balance adjustments and referral rewards
// are handled by CoreProvider.ExtendSubscription / ApplyReferralReward — NOT by
// the payment plugin. This prevents a rogue payment plugin from minting
// subscriptions in violation of core business rules.
//
// Required services (Metadata.Requires):
//
//	"payment_recorder" (from core, optional:false)
type PaymentProvider interface {
	Plugin

	// MethodID is the stable, lowercase provider identifier used in Payment.Method
	// and in the routing key inside plugins.yaml (e.g. "platega", "yookassa").
	MethodID() string

	// CreateIntent initiates a new payment with the gateway and returns the
	// redirect URL and gateway transaction ID.
	CreateIntent(ctx context.Context, req PaymentIntentRequest) (*PaymentIntentResult, error)

	// VerifyCallback authenticates and parses an inbound webhook from the gateway.
	// The host calls this when the gateway-specific callback route is hit.
	// The plugin MUST verify the request's signature/authenticity before returning.
	VerifyCallback(ctx context.Context, r *http.Request) (*PaymentCallbackResult, error)

	// Refund initiates a refund for the given gateway transaction.
	Refund(ctx context.Context, externalID string, amount int) error
}

// ─────────────────────────────────────────────────────────────────────────────
// Extension Point 3 — PricingEngine
// ─────────────────────────────────────────────────────────────────────────────

// PricingRequest carries the inputs for a price calculation.
//
// The value objects are snapshots supplied by the core before it calls a
// PricingEngine. Keeping the request self-contained deliberately prevents a
// dependency cycle: the mandatory core plugin can require a pricing engine,
// while a pricing plugin does not need to resolve core repositories just to
// calculate a price. It also makes the contract usable by an external plugin
// without giving it database access.
type PricingRequest struct {
	// UserID is the buyer whose current subscription is represented below.
	UserID string
	// PlanID and the fields below it are retained for callers built against the
	// first version of this contract. New implementations should use Plan,
	// CurrentSubscription, MaxDevices, Platform and Amount.
	PlanID        int64
	PromoCode     string
	ExtraDevices  int
	IsUpgrade     bool
	CurrentPlanID *int64

	// Amount is the requested amount for a payment without a subscription
	// plan (for example, a balance top-up).
	Amount int
	// Plan is the selected subscription plan. It is nil for a non-plan
	// payment. Pricing engines must not mutate it.
	Plan *Plan
	// CurrentSubscription is the buyer's latest subscription, if any. It is
	// used by the default engine to prorate additional device slots.
	CurrentSubscription *Subscription
	// MaxDevices is the requested device limit for the new/renewed plan.
	MaxDevices int
	// Platform is the normalized-or-raw purchase channel ("bot", "web", ...)
	// used to decide whether a promo is applicable.
	Platform string
	// Promo is the promo snapshot resolved by core for PromoCode. It is nil
	// when the code was not found. Engines must treat a nil promo as ineligible.
	Promo *PromoCode
	// Now gives a deterministic clock value for expiration and upgrade
	// calculations. A zero value means the engine may use time.Now().
	Now time.Time
}

// PricingResult is the output of CalculatePrice.
type PricingResult struct {
	FinalPrice      int    // in minor units
	DiscountPercent int    // 0–100
	AppliedPromo    string // promo code that was applied, if any
	// AppliedPromoID is the core persistence identifier for AppliedPromo. It
	// is nil when a global discount won or no promo applies.
	AppliedPromoID *int64
	Description    string
}

// PricingEngine is the extension point for subscription price calculation.
// This allows replacing the default pricing logic (global discounts, promo codes,
// device-upgrade costs) with a custom implementation (A/B tests, partner pricing)
// without modifying the core plugin.
//
// Published service names (Metadata.Publishes):
//
//	"pricing_engine"
//
// Required services (Metadata.Requires):
//
// Pricing engines receive immutable plan/promo/subscription snapshots in
// PricingRequest, so they normally have no repository dependency.
type PricingEngine interface {
	Plugin

	// CalculatePrice returns the final price for the given purchase request.
	CalculatePrice(ctx context.Context, req PricingRequest) (PricingResult, error)
}

// ─────────────────────────────────────────────────────────────────────────────
// Extension Point 4 — NotificationProvider
// ─────────────────────────────────────────────────────────────────────────────

// Notification is the channel-agnostic message sent by the core plugin to a
// NotificationProvider.
type Notification struct {
	// Channel is the delivery channel: "email", "telegram", "sms", etc.
	Channel string
	// To is the recipient address in the channel's native format
	// (email address, telegram chat ID, phone number, etc.).
	To string
	// Kind is the notification template identifier: "otp_code",
	// "subscription_expiring", "payment_received", etc.
	Kind string
	// Payload carries template variables. Keys and types depend on Kind.
	Payload map[string]any
}

// NotificationProvider is the extension point for user-facing notifications.
// Multiple providers may be loaded (email + telegram). The core plugin routes
// a Notification to all providers that declare the matching channel in Channels().
//
// Published service names (Metadata.Publishes):
//
//	"notification_provider"
type NotificationProvider interface {
	Plugin

	// Channels returns the list of delivery channels this provider handles.
	Channels() []string

	// Send delivers the notification. Returns an error only if the delivery
	// attempt itself failed; non-critical failures (e.g. user has no telegram)
	// should be logged at Warn level, not returned as errors.
	Send(ctx context.Context, n Notification) error
}

// ─────────────────────────────────────────────────────────────────────────────
// Extension Point 5 — EventSink
// ─────────────────────────────────────────────────────────────────────────────

// Event is a domain event emitted by the kernel or a plugin and fanned out
// to all registered EventSink plugins.
type Event struct {
	// Type is the event name, e.g. "subscription.created", "payment.completed",
	// "plugin.crashed", "antifraud.ban".
	Type string
	// OccurredAt is the wall-clock time the event was produced.
	OccurredAt time.Time
	// Data carries event-specific fields.
	Data map[string]any
	// UserMeta carries optional user context (userID, email) for tracing.
	UserMeta map[string]any
}

// EventSink is the extension point for consuming domain events.
// The kernel's event bus delivers every emitted Event to all loaded EventSink
// plugins asynchronously (fire-and-forget). The existing HTTP-webhook mechanism
// becomes the "eventsink_webhook" builtin plugin that implements this interface.
type EventSink interface {
	Plugin

	// Handle processes a single event. Implementations should be non-blocking
	// where possible; heavy processing belongs in a buffered internal queue.
	// Errors are logged by the host but do not stop delivery to other sinks.
	Handle(ctx context.Context, ev Event) error
}

// ─────────────────────────────────────────────────────────────────────────────
// Extension Point 6 — ClusterSyncProvider
// ─────────────────────────────────────────────────────────────────────────────

// SyncResult holds the outcome of a sync attempt for one slave.
type SyncResult struct {
	ServerName string
	Success    bool
	Error      error
}

// SlaveUserTotal is a user-level traffic aggregation from a slave node.
type SlaveUserTotal struct {
	Email string
	Slave int64
}

// SlaveReport summarises the overall health of the slave cluster.
type SlaveReport struct {
	Enabled       bool
	TotalServers  int
	OKServers     int
	FailedServers int
}

// ClusterSyncProvider is the extension point for master→slave state replication
// and cluster-level traffic statistics. Single-node installations disable this
// plugin entirely (enabled: false in plugins.yaml), which means no slave-related
// goroutines, connections or DB rows are created.
//
// Required services (Metadata.Requires):
//
//	"subscription_repository" (from core, optional:false)
type ClusterSyncProvider interface {
	Plugin

	// SyncAllSlaves replicates the current state to all configured slave nodes.
	SyncAllSlaves(ctx context.Context, dryRun bool, forceFull bool) ([]SyncResult, error)

	// CollectSlaveTotals aggregates per-user traffic from all reachable slaves.
	CollectSlaveTotals() ([]SlaveUserTotal, SlaveReport)
}

// SyncState is the transport-neutral position of a node in the cluster sync
// log. It mirrors the state persisted by the built-in cluster plugin without
// exposing a statesync implementation to HTTP consumers.
type SyncState struct {
	LastEventID int64
	StateHash   string
	UpdatedAt   time.Time
}

// ClusterSyncHTTPProvider is an optional capability of a ClusterSyncProvider.
// The kernel gives it to the HTTP router so slave nodes can request a snapshot
// and the current master position without importing internal/statesync.
//
// Keeping this separate from ClusterSyncProvider preserves compatibility with
// providers that only implement scheduled replication and traffic collection.
type ClusterSyncHTTPProvider interface {
	ClusterSyncProvider

	BuildSnapshot(ctx context.Context) ([]VPNUserConfig, error)
	MasterState(ctx context.Context) (SyncState, error)
}

// ClusterCommandPropagator is the optional cluster capability used by admin
// user handlers to broadcast legacy newuser/rmuser commands. The core HTTP
// package never constructs slave clients directly; a loaded cluster plugin
// owns the transport and may decline the operation when it is unavailable.
type ClusterCommandPropagator interface {
	ClusterSyncProvider

	PropagateCommand(ctx context.Context, command string, params map[string]string) error
}

// ─────────────────────────────────────────────────────────────────────────────
// Extension Point 7 — EngineProvider + ClientConfigContributor
// ─────────────────────────────────────────────────────────────────────────────

// VPNUserConfig is the engine-agnostic representation of a VPN user.
// Mirrors domain.VPNUserConfig so that engine plugins do not need to import
// the domain package directly.
type VPNUserConfig struct {
	Email      string
	UUID       string
	Auth       string
	Subfile    string
	Expire     string
	MaxDevices int
	Flow       string
	Cipher     string
	// PlanEngineIDs is the engine set selected by the subscription plan for
	// the by-plan routing mode. Empty means that no plan-specific routing was
	// supplied and the router's safe broadcast fallback applies.
	PlanEngineIDs []string
	// SubscriptionEngineIDs is the explicit per-subscription override. It is
	// normally sourced from Subscription.Metadata["engine_ids"].
	SubscriptionEngineIDs []string
}

// TrafficStat holds per-user bandwidth counters for one engine.
type TrafficStat struct {
	Email string
	Up    int64
	Down  int64
}

// EngineSyncResult reports what a SyncUsers call did.
type EngineSyncResult struct {
	Added   int
	Removed int
}

// ClientLink is a single VPN share-link produced by an engine for one user.
type ClientLink struct {
	// Protocol is the link scheme: "vless", "vmess", "trojan", "hysteria2", etc.
	Protocol string
	// URI is the full share link string (including the scheme).
	URI string
	// Label is the human-readable display name shown in client apps.
	Label string
}

// ClientConfigContributor is implemented by every EngineProvider to produce
// the protocol-specific share links for a user's subscription.
//
// This is the core of section 2.6.4 in the plan: subscription.go stops
// parsing vpn.RawConfig directly and instead calls BuildClientLinks on each
// active engine, collecting the results into one unified subscription response.
type ClientConfigContributor interface {
	// BuildClientLinks returns all share links (vless://, hysteria2://, ...)
	// for the given user, in the format specific to this engine.
	// The engine reads its own running config to extract keys and endpoints.
	BuildClientLinks(ctx context.Context, u VPNUserConfig) ([]ClientLink, error)
}

// EngineProvider is the extension point for a VPN engine (Xray, Singbox, …).
// It combines the full domain.Engine contract (user mutations, traffic stats,
// ban/unban, config sync) with Plugin lifecycle methods and ClientConfigContributor.
//
// The existing vpn.Adapter (Xray) becomes internal/plugins/engine_xray by
// adding Metadata()/Init()/Start()/Stop()/Health()/ID() wrappers — without
// changing any of the underlying engine logic.
//
// Published service names (Metadata.Publishes):
//
//	"engine.softban", "engine.logger_control"
//
// Required services: none (engines are loaded before other plugins).
type EngineProvider interface {
	Plugin
	ClientConfigContributor

	// ID returns the stable engine identifier used in routing decisions and logs.
	// Examples: "xray", "singbox", "mihomo".
	ID() string

	// --- UserMutator ---
	AddUser(ctx context.Context, user VPNUserConfig) error
	AddUsersBulk(ctx context.Context, users []VPNUserConfig) error
	RemoveUser(ctx context.Context, email string) error
	RemoveUsersBulk(ctx context.Context, emails []string) error
	SetExpire(ctx context.Context, email string, expire string) error
	SetLimit(ctx context.Context, email string, limit float64) error
	RebuildInbound(ctx context.Context, tag string) error

	// --- TrafficReader ---
	QueryStats(ctx context.Context) ([]TrafficStat, error)

	// --- SoftBanner ---
	BanUser(ctx context.Context, email string) error
	UnbanUser(ctx context.Context, email string) error

	// --- LoggerController ---
	RestartLogger(ctx context.Context) error

	// --- StateSyncer ---
	ListUsers(ctx context.Context) ([]VPNUserConfig, error)

	// --- ConfigRegenerator ---
	SyncUsers(ctx context.Context, dbUsers []VPNUserConfig, removeOrphans bool) (*EngineSyncResult, error)
}

// EngineRouter decides which engines a given VPN user should be registered on.
// This allows broadcast (all engines), by-plan, and override routing modes
// as described in plan section 2.6.3.
type EngineRouter interface {
	// EnginesFor returns the subset of loaded engines that should serve this user.
	EnginesFor(u VPNUserConfig) []EngineProvider
}

// ─────────────────────────────────────────────────────────────────────────────
// Shared domain types (mirrored from domain package to avoid cross-plugin imports)
// ─────────────────────────────────────────────────────────────────────────────

// User mirrors domain.User. The canonical definition lives in the core plugin;
// this copy exists so that other plugins can depend on pluginapi only.
type User struct {
	ID         string
	Username   string
	Balance    int
	IsAdmin    bool
	RefCode    string
	ReferredBy *string
	Metadata   map[string]any
	IsBlocked  bool
	CreatedAt  time.Time
}

// Subscription mirrors domain.Subscription.
// UUID is an engine-agnostic client identifier shared with domain.Subscription.
type Subscription struct {
	ID         string
	UserID     string
	Email      string
	UUID       string
	Status     string
	MaxDevices int
	StartsAt   *time.Time
	EndsAt     *time.Time
	AutoRenew  bool
	Metadata   map[string]any
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Device mirrors domain.Device.
type Device struct {
	ID             int64
	SubscriptionID string
	HWID           string
	DeviceModel    string
	DeviceOS       string
	UserAgent      string
}

// Plan mirrors domain.Plan.
type Plan struct {
	ID                    int64
	Months                int
	BasePrice             int
	GlobalDiscountPercent int
	// EngineIDs restricts this plan to these engine IDs in by-plan mode.
	// A nil/empty slice is deliberately backwards compatible and broadcasts.
	EngineIDs []string
	IsActive  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PromoCode is the immutable pricing view of a promo code. It mirrors the
// fields pricing rules are allowed to inspect, without exposing mutable
// persistence operations to a plugin.
type PromoCode struct {
	ID              int64
	Code            string
	DiscountPercent int
	MaxUses         int
	UsesCount       int
	TargetPlatform  string
	ExpiresAt       *time.Time
	IsActive        bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// ReferralStats holds referral aggregation data for one referrer.
type ReferralStats struct {
	ReferrerID string
	Count      int64
	Total      int64
}

// EmailAndMaxDevice is a projection used by the antifraud device-limit cache.
type EmailAndMaxDevice struct {
	Email      string
	MaxDevices int
}

// CurrentAPIVersion is the pluginapi contract version this build supports.
// Increment when making breaking changes to any interface in this package.
const CurrentAPIVersion = "1"
