# xraytool-go E2E Test Infrastructure

This document describes the design, execution, and verification architecture of the end-to-end (E2E) testing framework for `xraytool-go`.

## Testing Philosophy & Approach

The test suite is designed as an **opaque-box E2E test suite**, validating system behavior strictly through public CLI entry points and REST API endpoints. It treats the application as a black box and avoids direct imports of internal logic where possible, verifying:
- Exit codes, stdout, and stderr for CLI commands.
- HTTP status codes, headers, and response JSON payloads for REST API handlers.
- Database state transitions inside GORM-managed SQLite tables.

## Test Tiers

The test suite is structured into four testing tiers to verify different aspects of system security and reliability:

### Tier 1: Feature Coverage (Feature Verification)
Covers basic functional flows for all 6 core features:
1. **tg_lookup**: Verifies user registration and lookup by Telegram ID.
2. **balance_mgmt**: Verifies balance incrementing and deduction checks.
3. **sub_devices**: Verifies device subscription creation, formatting, and limits.
4. **webhook_pay**: Verifies payment creation, retrieval, and callback routing.
5. **cmd_exec_safety**: Verifies shell command dispatch and execution routes.
6. **crypto_leak_safety**: Verifies cryptographic helpers and safe resource disposal.

### Tier 2: Boundary & Corner Cases (Robustness & Security)
Verifies input boundaries, missing fields, format violations, and injection vectors:
- Non-existent IDs, invalid formats, and negative values.
- Webhook bypass behaviors and signature validations.
- Command injection attempts in user-supplied arguments.
- Concurrency limit tests and race conditions.

### Tier 3: Cross-Feature Pairwise Interactions
Checks how features interact together in pairs (e.g., registration leading to referral updates, payment trigger modifying balance, balance modifications enabling auto-renew subscription execution, device limits triggering events).

### Tier 4: Real-World User Scenarios
Executes multi-step user workflows simulating production behavior:
- Complete referral sign-up, top-up, subscription purchase, and device registration.
- Webhook-driven auto-renew cycle with multiple balance adjustments.
- Multi-device onboarding up to the Max Device limit.

---

## Execution Environment

The tests automate the setup of a clean test environment before running test cases:
1. **Compilation**: The `go test` environment compiles the latest source code to a test executable `build/xraytool.exe` (on Windows).
2. **Database Isolation**: The test runner spins up the API server with a dedicated configuration YAML pointing to a temporary SQLite database, completely isolating tests from production data.
3. **PATH Hijacking**: External dependencies (like the `xray` binary) are mocked using PATH hijacking: writing a temporary script `xray.bat` and prepending its folder to the subprocess's `PATH`.

## How to Run the Tests

To run the complete E2E test suite on Windows, execute the PowerShell script:

```powershell
.\tests\run_tests.ps1
```

Alternatively, run the Go tests directly:

```cmd
go test -v ./tests/...
```
