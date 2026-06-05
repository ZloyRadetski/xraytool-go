# Project: xraytool-go Security & Integrity Refactoring

## Architecture
- **Database Layer**: GORM v2 based models (`internal/database/models.go`) with GORM sqlite/postgres driver setup (`internal/database/db.go`).
- **REST API server**: Handlers and Router (`internal/server/`).
- **Xray Controller**: Inbound configuration updates and process interface (`internal/xrayapi/`).
- **Cryptography**: Secure secret generation (`internal/generate/`).
- **Logger**: Service/Request logger (`internal/logger/`).

## Code Layout
- `internal/database/models.go` — DB structs: User, Subscription, Device, Payment, ReferralReward
- `internal/database/db.go` — DB connection & initialization singleton
- `internal/server/router.go` — API entry routing and middleware
- `internal/server/handlers_user.go` — User endpoints: registration, lookup, balance modifications
- `internal/server/handlers_payment.go` — Payment & referral reward callback processing
- `internal/generate/secret.go` — Crypto-secure token utilities
- `internal/xrayapi/` — External Xray API calls and command execution

## Milestones
| # | Name | Scope | Dependencies | Status | Conversation ID |
|---|------|-------|-------------|--------|-----------------|
| 1 | R1: SQL Injection & Account Takeover | Safe Exact-match JSON query for telegram_id | None | IN_PROGRESS | 9cbc1401-ac8f-4fe9-9557-32b57006a5a6 |
| 2 | R2: Race Conditions & Balance Updates | DB-level atomic increments and transactional device checks | Milestone 1 | PLANNED | |
| 3 | R3: RCE & OS Incompatibility | Command argument sanitation and temp files for Xray stream | None | PLANNED | |
| 4 | R4: Webhook & Referral Logic | RowsAffected verification & API-Key bypass for webhook | Milestone 2 | PLANNED | |
| 5 | R5: Resource Leaks & Cryptography | Logger file descriptor leak fix & remove predictable crypto | None | PLANNED | |

## Interface Contracts
### GORM Database ↔ Server Handlers
- `findUserByTelegramID(db *gorm.DB, tgID int64) (*database.User, error)`: Must locate user by exact match of `telegram_id` in the `metadata` JSON field.
- Balance updates must perform atomic operations on `user.Balance` at the database level.
- Device limit checks and subscription updates must run inside GORM transaction blocks.
- `generate.Secret(length int) (string, error)`: Must return cryptographically secure random string, or error if generation fails.
