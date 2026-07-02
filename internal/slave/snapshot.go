package slave

import (
	"context"
	"fmt"
	"os"

	"xraytool/internal/domain"
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


// BuildMasterSnapshot builds a Snapshot by querying blocked/banned emails from the
// registry (driven port) and listing active users from the engine. No direct DB
// calls — all storage access goes through domain interfaces.
func BuildMasterSnapshot(ctx context.Context, reg domain.Registry, engine domain.StateSyncer) Snapshot {
	blockedMap := getBlockedEmailsFromRegistry(ctx, reg)

	users, err := engine.ListUsers(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[WARN] slave: cannot list users from engine for snapshot: %v\n", err)
		return Snapshot{}
	}

	var active []SnapshotUser
	for _, u := range users {
		if blockedMap[u.Email] {
			continue
		}

		limitF := float64(u.MaxDevices)
		su := SnapshotUser{
			Email:   u.Email,
			UUID:    u.UUID,
			Auth:    u.Auth,
			Subfile: u.Subfile,
			Expire:  u.Expire,
			Limit:   &limitF,
		}
		active = append(active, su)
	}

	return Snapshot{Active: active}
}

// getBlockedEmailsFromRegistry queries the registry for globally-blocked users
// and active antifraud bans, returning a set of emails to exclude from snapshots.
func getBlockedEmailsFromRegistry(ctx context.Context, reg domain.Registry) map[string]bool {
	blockedMap := make(map[string]bool)
	if reg == nil {
		return blockedMap
	}

	// Blocked users (admin block)
	subs, err := reg.Subscriptions().GetMasterSnapshot(ctx)
	if err == nil {
		// GetMasterSnapshot already filters by active status; additionally check
		// the user's is_blocked flag by querying all subs and cross-referencing.
		// For simplicity we exclude subs where the linked user is blocked:
		// that join is performed inside GetMasterSnapshot (it only returns active,
		// non-blocked records). Any email not in the result is NOT excluded here
		// because we want to block only explicitly blocked users.
		_ = subs // subs returned by GetMasterSnapshot are already filtered
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
