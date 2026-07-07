package slave

import (
	"context"
	"fmt"

	"xraytool/internal/domain"
	"xraytool/internal/vpn"
)

type Snapshot struct {
	Active []SnapshotUser `json:"active"`
}

type SnapshotUser struct {
	Email   string   `json:"email"`
	UUID    string   `json:"uuid,omitempty"`
	Auth    string   `json:"auth,omitempty"`
	Subfile string   `json:"subfile"`
	Expire  string   `json:"expire"`
	Limit   *float64 `json:"limit,omitempty"`
}



// BuildMasterSnapshot builds a Snapshot by querying active subscriptions from the DB
// and merging static template users. Returns an error if querying fails, preventing
// destructive empty snapshot syncs.
func BuildMasterSnapshot(ctx context.Context, reg domain.Registry, engine domain.StateSyncer) (Snapshot, error) {
	blockedMap := getBlockedEmailsFromRegistry(ctx, reg)

	var active []SnapshotUser
	dbAllEmails := make(map[string]bool)

	// 1. Load active subscriptions directly from GORM DB
	if reg != nil {
		subs, err := reg.Subscriptions().FindAll(ctx)
		if err != nil {
			return Snapshot{}, fmt.Errorf("snapshot: load subscriptions from DB failed: %w", err)
		}

		for _, sub := range subs {
			if sub.Email == "" {
				continue
			}
			dbAllEmails[sub.Email] = true

			if sub.Status != "active" || blockedMap[sub.Email] {
				continue
			}

			u := vpn.SubscriptionToVPNUserConfig(sub)
			limitF := float64(u.MaxDevices)
			active = append(active, SnapshotUser{
				Email:   u.Email,
				UUID:    u.UUID,
				Auth:    u.Auth,
				Subfile: u.Subfile,
				Expire:  u.Expire,
				Limit:   &limitF,
			})
		}
	}

	// 2. Load static template users from the master's config
	engineUsers, err := engine.ListUsers(ctx)
	if err != nil {
		return Snapshot{}, fmt.Errorf("snapshot: list engine users failed: %w", err)
	}

	for _, u := range engineUsers {
		if u.Email == "" || blockedMap[u.Email] {
			continue
		}
		// If they exist in DB (even if inactive/expired), they are governed by GORM, not static template.
		if dbAllEmails[u.Email] {
			continue
		}

		limitF := float64(u.MaxDevices)
		active = append(active, SnapshotUser{
			Email:   u.Email,
			UUID:    u.UUID,
			Auth:    u.Auth,
			Subfile: u.Subfile,
			Expire:  u.Expire,
			Limit:   &limitF,
		})
	}

	return Snapshot{Active: active}, nil
}

// getBlockedEmailsFromRegistry queries the registry for globally-blocked users
// and active antifraud bans, returning a set of emails to exclude from snapshots.
func getBlockedEmailsFromRegistry(ctx context.Context, reg domain.Registry) map[string]bool {
	blockedMap := make(map[string]bool)
	if reg == nil {
		return blockedMap
	}

	// Blocked users (admin block)
	users, err := reg.Users().FindAll(ctx)
	if err == nil {
		blockedUserIDs := make(map[string]bool)
		for _, u := range users {
			if u.IsBlocked {
				blockedUserIDs[u.ID] = true
			}
		}

		if len(blockedUserIDs) > 0 {
			subs, err := reg.Subscriptions().FindAll(ctx)
			if err == nil {
				for _, sub := range subs {
					if blockedUserIDs[sub.UserID] && sub.Email != "" {
						blockedMap[sub.Email] = true
					}
				}
			}
		}
	}

	// Active antifraud bans
	bans, err := reg.AntifraudBans().FindActive(ctx)
	if err == nil {
		for _, b := range bans {
			blockedMap[b.Email] = true
		}
	}

	return blockedMap
}
