package cmd

import (
	"time"

	"xraytool/internal/database"
	"xraytool/internal/generate"
)

func sqlSetStatus(email, status string) {
	if database.IsReady() {
		db := database.DB()
		var sub database.Subscription
		if err := db.Where("email = ?", email).Order("created_at desc").First(&sub).Error; err == nil {
			db.Model(&sub).Updates(map[string]interface{}{
				"status":     status,
				"updated_at": time.Now(),
			})
		}
	}
}

func sqlSetExpire(email string, expireVal string) {
	if database.IsReady() {
		db := database.DB()
		var sub database.Subscription
		if err := db.Where("email = ?", email).Order("created_at desc").First(&sub).Error; err == nil {
			updates := map[string]interface{}{
				"updated_at": time.Now(),
			}
			if t, err := time.Parse("02-01-2006", expireVal); err == nil {
				updates["ends_at"] = t
			}
			db.Model(&sub).Updates(updates)
		}
	}
}

func sqlSetLimit(email string, limit int) {
	if database.IsReady() {
		db := database.DB()
		var sub database.Subscription
		if err := db.Where("email = ?", email).Order("created_at desc").First(&sub).Error; err == nil {
			db.Model(&sub).Updates(map[string]interface{}{
				"max_devices": limit,
				"updated_at":  time.Now(),
			})
		}
	}
}

func sqlCreateUserIfNotExist(email, xrayUUID, expire string, limit int, subfile string) {
	if database.IsReady() {
		db := database.DB()
		var sub database.Subscription
		if err := db.Where("email = ?", email).First(&sub).Error; err != nil {
			// user not found, create
			var endsAt *time.Time
			if t, err := time.Parse("02-01-2006", expire); err == nil {
				endsAt = &t
			}
			userID, _ := generate.UUID()
			if userID == "" {
				userID = xrayUUID
			}

			subID, _ := generate.UUID()
			if subID == "" {
				subID = xrayUUID + "-sub"
			}

			db.Create(&database.User{
				ID:        userID,
				Username:  email,
				RefCode:   "ref_" + generate.Secret(8),
				CreatedAt: time.Now(),
			})
			db.Create(&database.Subscription{
				ID:         subID,
				UserID:     userID,
				Email:      email,
				XrayUUID:   xrayUUID,
				Status:     "active",
				MaxDevices: limit,
				EndsAt:     endsAt,
				Metadata:   map[string]interface{}{"subfile": subfile},
				CreatedAt:  time.Now(),
				UpdatedAt:  time.Now(),
			})
		} else {
			// User exists (probably from migrate_db). 
			// We must merge the REAL XrayUUID and subfile from config.json.
			updated := false
			if sub.Metadata == nil {
				sub.Metadata = make(map[string]interface{})
			}
			if sub.XrayUUID != xrayUUID {
				sub.XrayUUID = xrayUUID
				updated = true
			}
			if sub.Metadata["subfile"] != subfile {
				sub.Metadata["subfile"] = subfile
				updated = true
			}
			if updated {
				db.Save(&sub)
			}
		}
	}
}
