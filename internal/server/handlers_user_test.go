package server_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"xraytool/internal/database"
)

func TestRegisterUser_Success(t *testing.T) {
	r := newTestRouter(t)
	w := doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":1001,"username":"Alice","telegram_username":"@alice"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d. body: %s", w.Code, w.Body.String())
	}
	res := jsonBody(t, w)
	if res["username"] != "Alice" {
		t.Errorf("expected Alice, got %v", res["username"])
	}
	if res["email"] != "bot_client_1001" {
		t.Errorf("expected bot_client_1001, got %v", res["email"])
	}
}

func TestRegisterUser_Idempotent(t *testing.T) {
	r := newTestRouter(t)
	w1 := doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":1002,"username":"Bob"}`)
	w2 := doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":1002,"username":"Bob"}`)

	if w1.Code != http.StatusCreated {
		t.Fatalf("first request failed: %d", w1.Code)
	}
	if w2.Code != http.StatusOK && w2.Code != http.StatusCreated {
		t.Fatalf("second request failed: %d", w2.Code)
	}

	id1 := jsonBody(t, w1)["id"]
	id2 := jsonBody(t, w2)["id"]
	if id1 != id2 {
		t.Errorf("expected same ID, got %v and %v", id1, id2)
	}
}

func TestGetUserByTelegram_Found(t *testing.T) {
	r := newTestRouter(t)
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":1003,"username":"Charlie"}`)
	w := doAuth(r, "GET", "/api/v1/users/telegram/1003", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if jsonBody(t, w)["username"] != "Charlie" {
		t.Errorf("wrong username")
	}
}

func TestAdjustBalance(t *testing.T) {
	r := newTestRouter(t)
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":1006,"username":"F"}`)

	w := doAuth(r, "POST", "/api/v1/users/telegram/1006/balance", `{"amount":200}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if int(jsonBody(t, w)["balance"].(float64)) != 200 {
		t.Errorf("expected 200")
	}

	w2 := doAuth(r, "POST", "/api/v1/users/telegram/1006/balance", `{"amount":-50}`)
	if w2.Code != http.StatusBadRequest {
		t.Errorf("expected 400")
	}
}

func TestSetMaxDevices(t *testing.T) {
	r := newTestRouter(t)
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":1007,"username":"G"}`)

	w := doAuth(r, "POST", "/api/v1/users/telegram/1007/max-devices", `{"max_devices":5}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	wg := doAuth(r, "GET", "/api/v1/users/telegram/1007", "")
	if int(jsonBody(t, wg)["max_devices"].(float64)) != 5 {
		t.Errorf("expected 5 devices")
	}
}

func TestAutoRenew_InsufficientBalance(t *testing.T) {
	r := newTestRouter(t)
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":1008,"username":"H"}`)

	w := doAuth(r, "POST", "/api/v1/users/telegram/1008/auto-renew", `{"plan_total_price":100,"new_ends_at":"2026-07-04T00:00:00Z"}`)
	if w.Code != http.StatusPaymentRequired {
		t.Fatalf("expected 402 Payment Required, got %d", w.Code)
	}
}

func TestAutoRenew_Success(t *testing.T) {
	r := newTestRouter(t)
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":1009,"username":"I"}`)
	doAuth(r, "POST", "/api/v1/users/telegram/1009/balance", `{"amount":200}`)

	w := doAuth(r, "POST", "/api/v1/users/telegram/1009/auto-renew", `{"plan_total_price":159,"new_ends_at":"2026-07-04T00:00:00Z"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	wg := doAuth(r, "GET", "/api/v1/users/telegram/1009", "")
	j := jsonBody(t, wg)
	if int(j["balance"].(float64)) != 41 {
		t.Errorf("expected 41 balance")
	}
	if j["sub_status"] != "active" {
		t.Errorf("expected active")
	}
}

func TestAdminBlock_Success(t *testing.T) {
	r := newTestRouter(t)
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":1014,"username":"N"}`)
	w := doAuth(r, "POST", "/api/v1/admin/users/bot_client_1014/block", "")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	wg := doAuth(r, "GET", "/api/v1/users/telegram/1014", "")
	if jsonBody(t, wg)["sub_status"] != "blocked" {
		t.Errorf("expected blocked")
	}
}

func TestAdminSetExpire_Success(t *testing.T) {
	r := newTestRouter(t)
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":1016,"username":"P"}`)
	w := doAuth(r, "POST", "/api/v1/admin/users/bot_client_1016/set-expire", `{"expire":"2028-01-01T00:00:00Z"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d. body: %s", w.Code, w.Body.String())
	}

	wg := doAuth(r, "GET", "/api/v1/users/telegram/1016", "")
	endsAt := jsonBody(t, wg)["ends_at"]
	if endsAt == nil || endsAt == "" {
		t.Errorf("expected ends_at to be set")
	}
}

func TestRegisterUser_DBError(t *testing.T) {
	t.Skip("skipping test that drops global tables")
}

func TestListUsers_DBError(t *testing.T) {
	t.Skip("skipping test that drops global tables")
}

func TestGetUserByTelegram_SubstringMatch(t *testing.T) {
	r := newTestRouter(t)
	// Register user with a longer ID — use 6-digit range to avoid conflicts with other tests
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":630034,"username":"CharlieLong"}`)

	// Querying for a shorter non-existent ID (63003) should not return CharlieLong
	w := doAuth(r, "GET", "/api/v1/users/telegram/63003", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for substring ID lookup, got %d. body: %s", w.Code, w.Body.String())
	}

	// Register user with the shorter ID (630001)
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":630001,"username":"CharlieShort"}`)

	// Querying for 630001 should now return CharlieShort
	w2 := doAuth(r, "GET", "/api/v1/users/telegram/630001", "")
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w2.Code)
	}
	if jsonBody(t, w2)["username"] != "CharlieShort" {
		t.Errorf("expected CharlieShort, got %v", jsonBody(t, w2)["username"])
	}

	// Querying for 630034 should return CharlieLong
	w3 := doAuth(r, "GET", "/api/v1/users/telegram/630034", "")
	if w3.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w3.Code)
	}
	if jsonBody(t, w3)["username"] != "CharlieLong" {
		t.Errorf("expected CharlieLong, got %v", jsonBody(t, w3)["username"])
	}
}

func TestRegisterUser_SubstringMatchPrevention(t *testing.T) {
	r := newTestRouter(t)
	// Register user with ID 12345 (Alice)
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":12345,"username":"Alice"}`)

	// Try to register a new user with ID 1234 (Bob)
	// In the vulnerable code, this would return Alice with 200 OK
	w := doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":1234,"username":"Bob"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 Created for new user 1234, got %d", w.Code)
	}
	if jsonBody(t, w)["username"] != "Bob" {
		t.Errorf("expected Bob, got %v (Account Takeover / substring collision occurred)", jsonBody(t, w)["username"])
	}
}

func TestGetUserByTelegram_SQLiteRepresentationsAndEdgeCases(t *testing.T) {
	r := newTestRouter(t)
	db := database.DB()

	// 1. Insert user with numeric telegram_id in metadata
	userNum := database.User{
		ID:       "uuid-num-123",
		Username: "UserNum",
		RefCode:  "ref_num_123",
		Metadata: database.Metadata{
			"telegram_id": 99999, // stored as number
		},
	}
	if err := db.Create(&userNum).Error; err != nil {
		t.Fatalf("failed to create userNum: %v", err)
	}

	// 2. Insert user with string telegram_id in metadata
	userStr := database.User{
		ID:       "uuid-str-456",
		Username: "UserStr",
		RefCode:  "ref_str_456",
		Metadata: database.Metadata{
			"telegram_id": "88888", // stored as string
		},
	}
	if err := db.Create(&userStr).Error; err != nil {
		t.Fatalf("failed to create userStr: %v", err)
	}

	// Test fetching numeric representation
	wNum := doAuth(r, "GET", "/api/v1/users/telegram/99999", "")
	if wNum.Code != http.StatusOK {
		t.Errorf("expected 200 for numeric tg ID, got %d", wNum.Code)
	} else if jsonBody(t, wNum)["username"] != "UserNum" {
		t.Errorf("expected UserNum, got %v", jsonBody(t, wNum)["username"])
	}

	// Test fetching string representation
	wStr := doAuth(r, "GET", "/api/v1/users/telegram/88888", "")
	if wStr.Code != http.StatusOK {
		t.Errorf("expected 200 for string tg ID, got %d", wStr.Code)
	} else if jsonBody(t, wStr)["username"] != "UserStr" {
		t.Errorf("expected UserStr, got %v", jsonBody(t, wStr)["username"])
	}

	// Test zero ID lookup
	wZero := doAuth(r, "GET", "/api/v1/users/telegram/0", "")
	if wZero.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for zero tg ID, got %d", wZero.Code)
	}

	// Test extremely large ID lookup
	wLarge := doAuth(r, "GET", "/api/v1/users/telegram/999999999999999999999999999999", "")
	if wLarge.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for extremely large tg ID, got %d", wLarge.Code)
	}

	// Test non-numeric ID lookup
	wAlpha := doAuth(r, "GET", "/api/v1/users/telegram/abc", "")
	if wAlpha.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for alpha tg ID, got %d", wAlpha.Code)
	}

	// Test register with extremely large ID (causes json unmarshal error)
	wLargeReg := doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":999999999999999999999999999999,"username":"TooLarge"}`)
	if wLargeReg.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for JSON unmarshal failure, got %d", wLargeReg.Code)
	}
}
func TestGetUserByTelegram_EdgeCases(t *testing.T) {
	r := newTestRouter(t)

	// 1. Extremely large ID (MaxInt64: 9223372036854775807)
	largeIDStr := "9223372036854775807"
	wLargeReg := doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":9223372036854775807,"username":"LargeIDUser"}`)
	if wLargeReg.Code != http.StatusCreated {
		t.Fatalf("failed to register large ID user: %d, body: %s", wLargeReg.Code, wLargeReg.Body.String())
	}
	wLargeGet := doAuth(r, "GET", "/api/v1/users/telegram/"+largeIDStr, "")
	if wLargeGet.Code != http.StatusOK {
		t.Errorf("failed to retrieve large ID user: %d", wLargeGet.Code)
	}
	if jsonBody(t, wLargeGet)["username"] != "LargeIDUser" {
		t.Errorf("retrieved wrong user for large ID")
	}

	// 2. Numeric vs String representation in metadata
	db := database.DB()
	stringUserID := "str-user-id-uuid"
	stringUser := database.User{
		ID:       stringUserID,
		Username: "StringIDUser",
		RefCode:  "ref_string_user",
		Metadata: database.Metadata{
			// Use 77777 to avoid conflict with TestGetUserByTelegram_SQLiteRepresentations which uses 88888
			"telegram_id": "77777",
		},
	}
	if err := db.Create(&stringUser).Error; err != nil {
		t.Fatalf("failed to create user with string telegram_id: %v", err)
	}
	// Verify it can be retrieved by telegram ID 77777
	wStrGet := doAuth(r, "GET", "/api/v1/users/telegram/77777", "")
	if wStrGet.Code != http.StatusOK {
		t.Errorf("failed to retrieve user with string telegram_id: %d", wStrGet.Code)
	} else if jsonBody(t, wStrGet)["username"] != "StringIDUser" {
		t.Errorf("retrieved wrong user for string telegram_id")
	}

	// 3. Zero ID
	wZeroGet := doAuth(r, "GET", "/api/v1/users/telegram/0", "")
	if wZeroGet.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for zero ID, got %d", wZeroGet.Code)
	}

	// 4. Invalid non-numeric ID
	wInvalidGet := doAuth(r, "GET", "/api/v1/users/telegram/abc", "")
	if wInvalidGet.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request for non-numeric ID, got %d", wInvalidGet.Code)
	}
}

func TestDeviceManagement(t *testing.T) {
	r := newTestRouter(t)

	// 1. Register a user
	doAuth(r, "POST", "/api/v1/users/register", `{"telegram_id":9999123,"username":"DeviceTester"}`)

	// 2. Insert a device manually into the test database
	db := database.DB()
	var user database.User
	if err := db.Where("json_extract(metadata, '$.telegram_id') = ?", 9999123).First(&user).Error; err != nil {
		t.Fatalf("failed to find user: %v", err)
	}

	var sub database.Subscription
	if err := db.Where("user_id = ?", user.ID).First(&sub).Error; err != nil {
		t.Fatalf("failed to find subscription: %v", err)
	}

	device1 := database.Device{
		SubscriptionID: sub.ID,
		HWID:           "test-hwid-1",
		DeviceModel:    "Test Phone",
	}
	db.Create(&device1)

	// 3. Test GET devices
	wGet := doAuth(r, "GET", "/api/v1/users/telegram/9999123/devices", "")
	if wGet.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wGet.Code)
	}
	// Parse JSON array
	var devices []map[string]interface{}
	if err := json.Unmarshal(wGet.Body.Bytes(), &devices); err != nil {
		t.Fatalf("failed to parse devices array: %v", err)
	}
	if len(devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devices))
	}
	if devices[0]["DeviceModel"] != "Test Phone" {
		t.Errorf("expected Test Phone, got %v", devices[0]["DeviceModel"])
	}

	// 4. Test DELETE device
	deviceID := fmt.Sprintf("%v", devices[0]["ID"])
	wDel := doAuth(r, "DELETE", "/api/v1/users/telegram/9999123/devices/"+deviceID, "")
	if wDel.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", wDel.Code)
	}

	// 5. Verify device is deleted
	var count int64
	db.Model(&database.Device{}).Where("subscription_id = ?", sub.ID).Count(&count)
	if count != 0 {
		t.Fatalf("expected 0 devices, got %d", count)
	}
}
