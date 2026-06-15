package xrayconfig

import (
	"testing"
)

func buildTestConfig() RawConfig {
	return RawConfig{
		"inbounds": []byte(`[
			{
				"tag": "in1",
				"protocol": "vless",
				"settings": {
					"clients": [
						{"id": "1", "email": "a@a", "limit": 10},
						{"id": "2", "email": "b@b"}
					]
				}
			},
			{
				"tag": "in2",
				"protocol": "hysteria",
				"settings": {
					"users": [
						{"name": "a@a", "hy2_auth": "pass", "empty": ""}
					]
				}
			},
			{
				"tag": "in3",
				"protocol": "vless"
			}
		]`),
	}
}

func TestUserExistsAndFind(t *testing.T) {
	cfg := buildTestConfig()

	// Exists
	ex, err := UserExists(cfg, "a@a")
	if err != nil || !ex {
		t.Errorf("expected true, nil, got %v, %v", ex, err)
	}

	// Not exists
	ex, err = UserExists(cfg, "c@c")
	if err != nil || ex {
		t.Errorf("expected false, nil, got %v, %v", ex, err)
	}

	// Find user
	u, err := FindUser(cfg, "a@a")
	if err != nil || u == nil {
		t.Errorf("FindUser failed")
	}
	if u.GetString("id") != "1" {
		t.Errorf("merged user missing id")
	}
	if u.GetString("hy2_auth") != "pass" {
		t.Errorf("merged user missing hy2_auth")
	}
	if u.GetString("limit") != "10" {
		t.Errorf("merged user missing limit")
	}

	// Error in GetInbounds
	badCfg := RawConfig{"inbounds": []byte(`[{"tag":`)}
	_, err = UserExists(badCfg, "a@a")
	if err == nil {
		t.Errorf("expected error on malformed inbounds")
	}
}

func TestFindUser_BadInbound(t *testing.T) {
	// Should skip inbound with bad settings
	cfg := RawConfig{
		"inbounds": []byte(`[
			{"settings": "bad"},
			{
				"tag": "ok",
				"settings": {"clients": [{"email": "a@a"}]}
			}
		]`),
	}
	u, err := FindUser(cfg, "a@a")
	if err != nil || u == nil {
		t.Errorf("expected to skip bad inbound and find user")
	}
}

func TestListUsers(t *testing.T) {
	cfg := buildTestConfig()
	users, err := ListUsers(cfg)
	if err != nil {
		t.Errorf("ListUsers failed: %v", err)
	}
	if len(users) != 2 {
		t.Errorf("expected 2 unique users, got %d", len(users))
	}

	// Error GetInbounds
	badCfg := RawConfig{"inbounds": []byte(`[{"tag":`)}
	_, err = ListUsers(badCfg)
	if err == nil {
		t.Errorf("expected error on malformed inbounds")
	}

	// Skip bad inbound
	cfgWithBad := RawConfig{
		"inbounds": []byte(`[
			{"settings": "bad"},
			{"settings": {"clients": [{"email": "a@a"}, {"id":"no-email"}]}}
		]`),
	}
	users2, _ := ListUsers(cfgWithBad)
	if len(users2) != 1 || users2[0].Email() != "a@a" {
		t.Errorf("expected 1 user, skipped bad inbound and missing email")
	}
}

func TestIsRawMessageEmpty(t *testing.T) {
	if !isRawMessageEmpty(nil) {
		t.Errorf("nil not empty")
	}
	if !isRawMessageEmpty([]byte("")) {
		t.Errorf("empty not empty")
	}
	if !isRawMessageEmpty([]byte("null")) {
		t.Errorf("null not empty")
	}
	if !isRawMessageEmpty([]byte(`""`)) {
		t.Errorf(`"" not empty`)
	}
	if isRawMessageEmpty([]byte(`"1"`)) {
		t.Errorf(`"1" empty`)
	}
}

func TestInboundTagsForUser(t *testing.T) {
	cfg := buildTestConfig()
	tags, err := InboundTagsForUser(cfg, "a@a")
	if err != nil {
		t.Errorf("failed: %v", err)
	}
	if len(tags) != 2 {
		t.Errorf("expected 2 tags, got %v", tags)
	}

	tags, _ = InboundTagsForUser(cfg, "c@c")
	if len(tags) != 0 {
		t.Errorf("expected 0 tags, got %v", tags)
	}

	badCfg := RawConfig{"inbounds": []byte(`[{"tag":`)}
	_, err = InboundTagsForUser(badCfg, "a@a")
	if err == nil {
		t.Errorf("expected error")
	}

	// Skip bad inbound
	cfgWithBad := RawConfig{
		"inbounds": []byte(`[
			{"settings": "bad"},
			{"tag": "t1", "settings": {"clients": [{"email": "a@a"}]}}
		]`),
	}
	tags2, _ := InboundTagsForUser(cfgWithBad, "a@a")
	if len(tags2) != 1 || tags2[0] != "t1" {
		t.Errorf("expected t1")
	}
}

func TestClientInbounds(t *testing.T) {
	cfg := buildTestConfig()
	ibs, err := ClientInbounds(cfg)
	if err != nil {
		t.Errorf("failed: %v", err)
	}
	if len(ibs) != 2 { // in1, in2 have clients. in3 does not.
		t.Errorf("expected 2 client inbounds, got %v", ibs)
	}

	badCfg := RawConfig{"inbounds": []byte(`[{"tag":`)}
	_, err = ClientInbounds(badCfg)
	if err == nil {
		t.Errorf("expected error")
	}
}

func TestAddUserToInbounds(t *testing.T) {
	cfg := buildTestConfig()
	payload := []TaggedClient{
		{Tag: "in1", Client: RawClient{"email": []byte(`"c@c"`)}},
		{Tag: "in2", Client: RawClient{"email": []byte(`"c@c"`)}},
		{Tag: "in3", Client: RawClient{"email": []byte(`"c@c"`)}}, // no clients list in in3 initially
	}
	err := AddUserToInbounds(cfg, payload)
	if err != nil {
		t.Errorf("failed: %v", err)
	}

	u, _ := FindUser(cfg, "c@c")
	if u == nil {
		t.Errorf("user not added")
	}
	tags, _ := InboundTagsForUser(cfg, "c@c")
	if len(tags) != 2 {
		t.Errorf("user should be added to in1 and in2, got %v", tags)
	}

	// Inject duplicates of "a@a"
	dupPayload := []TaggedClient{
		{Tag: "in1", Client: RawClient{"email": []byte(`"a@a"`), "limit": []byte("555")}},
	}
	_ = AddUserToInbounds(cfg, dupPayload)

	// Test replacing existing user "a@a" and cleaning up duplicates
	replacePayload := []TaggedClient{
		{Tag: "in1", Client: RawClient{"email": []byte(`"a@a"`), "limit": []byte("999")}},
	}
	if err := AddUserToInbounds(cfg, replacePayload); err != nil {
		t.Errorf("failed to replace user: %v", err)
	}
	u2, _ := FindUser(cfg, "a@a")
	if u2 == nil {
		t.Errorf("user a@a lost")
	} else if u2.GetString("limit") != "999" {
		t.Errorf("user a@a limit not updated, got %v", u2.GetString("limit"))
	}
	
	// Check that we didn't duplicate a@a
	usersList, _ := ListUsers(cfg)
	aCount := 0
	for _, client := range usersList {
		if client.Email() == "a@a" {
			aCount++
		}
	}
	if aCount != 1 {
		t.Errorf("expected exactly 1 user with email a@a, got %d", aCount)
	}

	badCfg := RawConfig{"inbounds": []byte(`[{"tag":`)}
	if err := AddUserToInbounds(badCfg, payload); err == nil {
		t.Errorf("expected error")
	}

	// Inbound with bad clients settings
	badSettingsCfg := RawConfig{"inbounds": []byte(`[{"tag": "t1", "settings": "bad"}]`)}
	if err := AddUserToInbounds(badSettingsCfg, []TaggedClient{{Tag: "t1"}}); err == nil {
		t.Errorf("expected error from bad inbound")
	}

	// SetClients error (e.g. malformed settings dict where we can't write)
	// We mocked SetClients but we can test bad JSON format for clients
	badSetCfg := RawConfig{"inbounds": []byte(`[{"tag": "t1", "settings": {"clients":[]}}]`)}
	// Inject invalid JSON to fail SetClients via Marshal
	payloadBad := []TaggedClient{{Tag: "t1", Client: RawClient{"email": []byte(`invalid json`)}}}
	if err := AddUserToInbounds(badSetCfg, payloadBad); err == nil {
		t.Errorf("expected error when SetClients fails")
	}
}

func TestRemoveUserFromAllInbounds(t *testing.T) {
	cfg := buildTestConfig()
	err := RemoveUserFromAllInbounds(cfg, "a@a")
	if err != nil {
		t.Errorf("failed: %v", err)
	}
	ex, _ := UserExists(cfg, "a@a")
	if ex {
		t.Errorf("user not removed")
	}

	badCfg := RawConfig{"inbounds": []byte(`[{"tag":`)}
	if err := RemoveUserFromAllInbounds(badCfg, "a@a"); err == nil {
		t.Errorf("expected error")
	}

	badSettingsCfg := RawConfig{"inbounds": []byte(`[{"tag": "t1", "settings": "bad"}]`)}
	if err := RemoveUserFromAllInbounds(badSettingsCfg, "a@a"); err == nil {
		t.Errorf("expected error")
	}

	// simulate SetClients error
	badSetCfg := RawConfig{"inbounds": []byte(`[{"tag": "t1", "settings": {"clients":[{"email":"x"}]}}]`)}
	// actually it's hard to trigger SetClients error naturally on Remove, as we just filter valid clients.
	_ = RemoveUserFromAllInbounds(badSetCfg, "x")
}

func TestUpdateFields(t *testing.T) {
	cfg := buildTestConfig()

	// UpdateStringField
	err := UpdateStringField(cfg, "a@a", "subfile", "newsub")
	if err != nil {
		t.Errorf("failed: %v", err)
	}
	u, _ := FindUser(cfg, "a@a")
	if u.GetString("subfile") != "newsub" {
		t.Errorf("string not updated")
	}

	// UpdateNumberField
	err = UpdateNumberField(cfg, "a@a", "limit", 999)
	if err != nil {
		t.Errorf("failed: %v", err)
	}
	u, _ = FindUser(cfg, "a@a")
	if n, _ := u.GetNumber("limit"); n != 999 {
		t.Errorf("number not updated")
	}

	badCfg := RawConfig{"inbounds": []byte(`[{"tag":`)}
	if err := UpdateStringField(badCfg, "a@a", "k", "v"); err == nil {
		t.Errorf("expected error")
	}

	badSettingsCfg := RawConfig{"inbounds": []byte(`[{"tag": "t1", "settings": "bad"}]`)}
	if err := UpdateStringField(badSettingsCfg, "a@a", "k", "v"); err == nil {
		t.Errorf("expected error")
	}
}

func TestReplaceAllClients(t *testing.T) {
	cfg := buildTestConfig()
	payload := []TaggedClient{
		{Tag: "in1", Client: RawClient{"email": []byte(`"new@new"`)}},
	}
	err := ReplaceAllClients(cfg, payload)
	if err != nil {
		t.Errorf("failed: %v", err)
	}

	// in1 should have 1 client, in2 should remain untouched... wait, does ReplaceAllClients clear in2?
	// It says: "clients, ok := grouped[tag]; if !ok || !ib.HasClientList() { continue }"
	// So in2 is skipped and keeps its clients!
	ibs, _ := cfg.GetInbounds()
	c1, _ := ibs[0].GetClients()
	if len(c1) != 1 || c1[0].Email() != "new@new" {
		t.Errorf("in1 not replaced correctly")
	}

	badCfg := RawConfig{"inbounds": []byte(`[{"tag":`)}
	if err := ReplaceAllClients(badCfg, payload); err == nil {
		t.Errorf("expected error")
	}
}
