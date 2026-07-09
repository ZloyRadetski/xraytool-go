package server_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestDevices(t *testing.T) {
	r := newTestRouter(t)

	// User
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":7001,"username":"DeviceUser"}`)

	// Set Max Devices
	wSet := doAuth(r, "POST", "/api/v1/users/telegram/7001/max-devices", `{"max_devices":5}`)
	if wSet.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wSet.Code)
	}

	wg := doAuth(r, "GET", "/api/v1/users/telegram/7001", "")
	if int(jsonBody(t, wg)["max_devices"].(float64)) != 5 {
		t.Errorf("max_devices mismatch")
	}

	// Get Devices (empty initially)
	wGetD := doAuth(r, "GET", "/api/v1/users/telegram/7001/devices", "")
	if wGetD.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wGetD.Code)
	}
	var devs []map[string]interface{}
	json.Unmarshal(wGetD.Body.Bytes(), &devs) //nolint:errcheck
	if len(devs) != 0 {
		t.Errorf("expected 0 devices")
	}

	// Delete Device (not found)
	wDelD := doAuth(r, "DELETE", "/api/v1/users/telegram/7001/devices/99999", "")
	if wDelD.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", wDelD.Code)
	}
}
