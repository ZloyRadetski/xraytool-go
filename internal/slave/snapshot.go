package slave

import (
	"time"

	"xraytool/internal/database"
	"xraytool/internal/xrayconfig"
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

// BatchPayload represents the requested operations for apply-batch
type BatchPayload struct {
	Add    []SnapshotUser `json:"add"`
	Remove []string       `json:"remove"`
}

func GetBlockedEmails() map[string]bool {
	var blockedSubs []database.Subscription
	// Only run this query if the database connection exists.
	if database.IsReady() {
		db := database.DB()
		db.Joins("JOIN users ON users.id = subscriptions.user_id").
			Where("users.is_blocked = ?", true).
			Find(&blockedSubs)
	}

	blockedMap := make(map[string]bool)
	for _, sub := range blockedSubs {
		blockedMap[sub.Email] = true
	}
	return blockedMap
}

func BuildMasterSnapshot(xrayCfg xrayconfig.RawConfig) Snapshot {
	blockedMap := GetBlockedEmails()

	// Anti-Fraud: exclude soft-banned users so they aren't sent to slaves as Active.
	if database.IsReady() {
		var banned []database.AntifraudBan
		if err := database.DB().Where("expires_at > ?", time.Now()).Find(&banned).Error; err == nil {
			for _, b := range banned {
				blockedMap[b.Email] = true
			}
		}
	}

	users, _ := xrayconfig.ListUsers(xrayCfg)
	active := make([]SnapshotUser, 0, len(users))
	for _, u := range users {
		if blockedMap[u.Email()] {
			continue
		}
		authVal := u.GetString("auth")
		if authVal == "" {
			authVal = u.GetString("password")
		}
		su := SnapshotUser{
			Email:   u.Email(),
			UUID:    u.GetString("id"),
			Auth:    authVal,
			Subfile: u.GetString("subfile"),
			Expire:  u.GetString("expire"),
		}
		if lv, ok := u.GetNumber("limit"); ok {
			su.Limit = &lv
		}
		active = append(active, su)
	}

	return Snapshot{Active: active}
}
