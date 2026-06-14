package server

import (
	"testing"
	"time"
)

func TestParseExpiryDate(t *testing.T) {
	mskLoc, err := time.LoadLocation("Europe/Moscow")
	if err != nil {
		mskLoc = time.FixedZone("MSK", 3*3600)
	}

	tests := []struct {
		name    string
		input   string
		want    time.Time
		wantErr bool
	}{
		{
			name:    "RFC3339 format",
			input:   "2026-07-04T00:00:00Z",
			want:    time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC),
			wantErr: false,
		},
		{
			name:    "Alternative RFC3339 without timezone",
			input:   "2026-07-04T15:00:00Z",
			want:    time.Date(2026, 7, 4, 15, 0, 0, 0, time.UTC),
			wantErr: false,
		},
		{
			name:    "Short date format",
			input:   "2026-07-04",
			want:    time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC),
			wantErr: false,
		},
		{
			name:    "Russian dot format with time",
			input:   "12.02.2026 14:00",
			want:    time.Date(2026, 2, 12, 14, 0, 0, 0, mskLoc).UTC(),
			wantErr: false,
		},
		{
			name:    "Russian dot format without time",
			input:   "12.02.2026",
			want:    time.Date(2026, 2, 12, 15, 0, 0, 0, mskLoc).UTC(),
			wantErr: false,
		},
		{
			name:    "Invalid format",
			input:   "12/02/2026",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseExpiryDate(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseExpiryDate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && !got.Equal(tt.want) {
				t.Errorf("parseExpiryDate() = %v, want %v", got, tt.want)
			}
		})
	}
}
