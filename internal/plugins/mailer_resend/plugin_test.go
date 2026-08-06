package mailer_resend_test

import (
	"context"
	"testing"

	mailerPlugin "xraytool/internal/plugins/mailer_resend"
	"xraytool/internal/pluginapi"
)

func TestMailerResend_Init_MissingAPIKey(t *testing.T) {
	t.Parallel()
	p := mailerPlugin.New()
	err := p.Init(context.Background(), pluginapi.RawConfig{
		"from_email": "noreply@example.com",
		// resend_api_key отсутствует
	}, nil)
	if err == nil {
		t.Fatal("expected error when resend_api_key is missing, got nil")
	}
}

func TestMailerResend_Init_MissingFromEmail(t *testing.T) {
	t.Parallel()
	p := mailerPlugin.New()
	err := p.Init(context.Background(), pluginapi.RawConfig{
		"resend_api_key": "re_test_key",
		// from_email отсутствует
	}, nil)
	if err == nil {
		t.Fatal("expected error when from_email is missing, got nil")
	}
}

func TestMailerResend_Init_Success(t *testing.T) {
	t.Parallel()
	p := mailerPlugin.New()
	err := p.Init(context.Background(), pluginapi.RawConfig{
		"resend_api_key": "re_test_key",
		"from_email":     "noreply@example.com",
	}, nil)
	if err != nil {
		t.Fatalf("expected nil error on valid config, got: %v", err)
	}
}

func TestMailerResend_Health_BeforeInit(t *testing.T) {
	t.Parallel()
	p := mailerPlugin.New()
	if err := p.Health(context.Background()); err == nil {
		t.Fatal("Health() before Init() should return error")
	}
}

func TestMailerResend_Health_AfterInit(t *testing.T) {
	t.Parallel()
	p := mailerPlugin.New()
	_ = p.Init(context.Background(), pluginapi.RawConfig{
		"resend_api_key": "re_test_key",
		"from_email":     "noreply@example.com",
	}, nil)
	if err := p.Health(context.Background()); err != nil {
		t.Fatalf("Health() after Init() should be nil, got: %v", err)
	}
}

func TestMailerResend_Send_UnsupportedChannel(t *testing.T) {
	t.Parallel()
	p := mailerPlugin.New()
	_ = p.Init(context.Background(), pluginapi.RawConfig{
		"resend_api_key": "re_test_key",
		"from_email":     "noreply@example.com",
	}, nil)
	err := p.Send(context.Background(), pluginapi.Notification{
		Channel: "sms",
		Kind:    "otp_code",
		Payload: map[string]any{"code": "123456"},
	})
	if err == nil {
		t.Fatal("Send() with unsupported channel should return error")
	}
}

func TestMailerResend_Send_UnsupportedKind(t *testing.T) {
	t.Parallel()
	p := mailerPlugin.New()
	_ = p.Init(context.Background(), pluginapi.RawConfig{
		"resend_api_key": "re_test_key",
		"from_email":     "noreply@example.com",
	}, nil)
	err := p.Send(context.Background(), pluginapi.Notification{
		Channel: "email",
		Kind:    "unknown_kind",
		To:      "user@example.com",
		Payload: map[string]any{},
	})
	if err == nil {
		t.Fatal("Send() with unknown kind should return error")
	}
}

func TestMailerResend_Send_OTPMissingCode(t *testing.T) {
	t.Parallel()
	p := mailerPlugin.New()
	_ = p.Init(context.Background(), pluginapi.RawConfig{
		"resend_api_key": "re_test_key",
		"from_email":     "noreply@example.com",
	}, nil)
	err := p.Send(context.Background(), pluginapi.Notification{
		Channel: "email",
		Kind:    "otp_code",
		To:      "user@example.com",
		Payload: map[string]any{}, // code отсутствует
	})
	if err == nil {
		t.Fatal("Send() otp_code without 'code' payload should return error")
	}
}

func TestMailerResend_Channels(t *testing.T) {
	t.Parallel()
	p := mailerPlugin.New()
	channels := p.Channels()
	if len(channels) != 1 || channels[0] != "email" {
		t.Fatalf("Channels() should return [\"email\"], got %v", channels)
	}
}

func TestMailerResend_Metadata(t *testing.T) {
	t.Parallel()
	p := mailerPlugin.New()
	m := p.Metadata()
	if m.Name != "mailer_resend" {
		t.Errorf("Name = %q, want %q", m.Name, "mailer_resend")
	}
	if m.Kind != "notification" {
		t.Errorf("Kind = %q, want %q", m.Kind, "notification")
	}
	if m.Mandatory {
		t.Error("mailer_resend must not be mandatory")
	}
}
