// Package database provides the GORM-based infrastructure adapters that
// implement the repository interfaces defined in internal/domain/ports.go.
//
// Nothing outside this package should import internal/database for the purpose
// of using repository interfaces — use domain.Registry directly in
// business-logic packages (payment, antifraud, user, worker, etc.).
//
// Type aliases below are provided for backward compatibility so that existing
// code referencing database.ReferralStats, database.EmailAndMaxDevice, etc.
// continues to compile while the canonical definitions live in domain.
package database

import "xraytool/internal/domain"

// Registry is a type alias for domain.Registry.
// Prefer using domain.Registry directly in business-logic packages.
type Registry = domain.Registry

// UserRepository is a type alias for domain.UserRepository.
type UserRepository = domain.UserRepository

// SubscriptionRepository is a type alias for domain.SubscriptionRepository.
type SubscriptionRepository = domain.SubscriptionRepository

// PaymentRepository is a type alias for domain.PaymentRepository.
type PaymentRepository = domain.PaymentRepository

// PlanRepository is a type alias for domain.PlanRepository.
type PlanRepository = domain.PlanRepository

// PromoRepository is a type alias for domain.PromoRepository.
type PromoRepository = domain.PromoRepository

// DeviceRepository is a type alias for domain.DeviceRepository.
type DeviceRepository = domain.DeviceRepository

// AntifraudBanRepository is a type alias for domain.AntifraudBanRepository.
type AntifraudBanRepository = domain.AntifraudBanRepository

// SubscriptionNotificationRepository is a type alias for domain.SubscriptionNotificationRepository.
type SubscriptionNotificationRepository = domain.SubscriptionNotificationRepository

// ReferralStats is a type alias for domain.ReferralStats.
type ReferralStats = domain.ReferralStats

// EmailAndMaxDevice is a type alias for domain.EmailAndMaxDevice.
type EmailAndMaxDevice = domain.EmailAndMaxDevice
