package database

import (
	"fmt"
	"strconv"

	"gorm.io/gorm"

	"xraytool/internal/domain"
)

// FindUserByPlatformID locates a User by its ID depending on the platform (uuid, telegram, or web)
func FindUserByPlatformID(db *gorm.DB, platform, id string) (*domain.User, error) {
	if platform == "uuid" {
		var user User
		if err := db.Where("id = ?", id).First(&user).Error; err != nil {
			return nil, err
		}
		dUser := user.ToDomain()
		return &dUser, nil
	}

	var users []User
	var query *gorm.DB

	switch db.Dialector.Name() {
	case "sqlite":
		if platform == "telegram" {
			tgIDInt, _ := strconv.ParseInt(id, 10, 64)
			query = db.Where(
				"json_extract(metadata, '$.telegram_id') = ? OR json_extract(metadata, '$.telegram_id') = ?",
				tgIDInt, id,
			)
		} else if platform == "web" {
			query = db.Where("json_extract(metadata, '$.email') = ? OR username = ?", id, id)
		}
	case "postgres":
		if platform == "telegram" {
			query = db.Where("metadata::jsonb ->> 'telegram_id' = ?", id)
		} else if platform == "web" {
			query = db.Where("metadata::jsonb ->> 'email' = ? OR username = ?", id, id)
		}
	}

	if query == nil {
		if platform == "telegram" {
			tgIDInt, _ := strconv.ParseInt(id, 10, 64)
			query = db.Where(
				"json_extract(metadata, '$.telegram_id') = ? OR json_extract(metadata, '$.telegram_id') = ?",
				tgIDInt, id,
			)
		} else if platform == "web" {
			query = db.Where("json_extract(metadata, '$.email') = ? OR username = ?", id, id)
		} else {
			return nil, fmt.Errorf("unsupported platform: %s", platform)
		}
	}

	result := query.Limit(1).Find(&users)
	if result.Error != nil {
		return nil, result.Error
	}
	if len(users) == 0 {
		return nil, gorm.ErrRecordNotFound
	}
	dUser := users[0].ToDomain()
	return &dUser, nil
}
