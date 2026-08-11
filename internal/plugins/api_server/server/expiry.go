package server

import (
	"fmt"
	"time"
)

// parseExpiryDate parses the administrator-facing expiry input. It belongs to
// the API plugin rather than a subscription-format renderer because it changes
// subscription lifecycle state; it does not render a client configuration.
func parseExpiryDate(dateStr string) (time.Time, error) {
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
