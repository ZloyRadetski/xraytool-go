package convert

import (
	"fmt"
	"time"
)

// ParseExpiryDate parses an expiry date string into a time.Time.
// Supports: "DD.MM.YYYY HH:MM", "DD.MM.YYYY" (defaults to 15:00 MSK), and RFC3339.
func ParseExpiryDate(dateStr string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, dateStr); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02T15:04:05Z", dateStr); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", dateStr); err == nil {
		return t, nil
	}

	mskLoc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		mskLoc = time.FixedZone("MSK", 3*3600)
	}

	if t, err := time.ParseInLocation("02.01.2006 15:04", dateStr, mskLoc); err == nil {
		return t.UTC(), nil
	}

	if t, err := time.ParseInLocation("02.01.2006", dateStr, mskLoc); err == nil {
		t = time.Date(t.Year(), t.Month(), t.Day(), 15, 0, 0, 0, mskLoc)
		return t.UTC(), nil
	}

	return time.Time{}, fmt.Errorf("invalid date format")
}
