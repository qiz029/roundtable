package main

import (
	"io"
	"testing"

	"github.com/qiz029/roundtable/internal/roundtable"
)

func TestNewMailerFromEnvSelectsMailgun(t *testing.T) {
	mailer, err := newMailerFromEnv(mapLookup(map[string]string{
		"ROUNDTABLE_MAILER":           "mailgun",
		"ROUNDTABLE_MAILGUN_DOMAIN":   "mg.example.com",
		"ROUNDTABLE_MAILGUN_API_KEY":  "test-key",
		"ROUNDTABLE_MAILGUN_FROM":     "Roundtable <noreply@mg.example.com>",
		"ROUNDTABLE_MAILGUN_API_BASE": "https://api.eu.mailgun.net",
		"ROUNDTABLE_SMTP_ADDR":        "smtp.example.com:587",
		"ROUNDTABLE_SMTP_FROM":        "smtp@example.com",
		"ROUNDTABLE_SMTP_USERNAME":    "smtp-user",
		"ROUNDTABLE_SMTP_PASSWORD":    "smtp-password",
		"ROUNDTABLE_PUBLIC_URL":       "https://roundtable.example.com",
	}), io.Discard)
	if err != nil {
		t.Fatalf("new mailer from env: %v", err)
	}
	if _, ok := mailer.(*roundtable.MailgunMailer); !ok {
		t.Fatalf("mailer = %T, want *roundtable.MailgunMailer", mailer)
	}
}

func TestNewMailerFromEnvRequiresExplicitMailgunConfig(t *testing.T) {
	_, err := newMailerFromEnv(mapLookup(map[string]string{
		"ROUNDTABLE_MAILER":          "mailgun",
		"ROUNDTABLE_MAILGUN_DOMAIN":  "mg.example.com",
		"ROUNDTABLE_MAILGUN_FROM":    "Roundtable <noreply@mg.example.com>",
		"ROUNDTABLE_MAILGUN_API_KEY": "",
	}), io.Discard)
	if err == nil {
		t.Fatal("expected missing mailgun api key error")
	}
}

func TestNewAvatarStoreFromEnvDefaultsToDisabled(t *testing.T) {
	store, publicBaseURL, mediaBaseURL, err := newAvatarStoreFromEnv(mapLookup(map[string]string{}))
	if err != nil {
		t.Fatalf("new avatar store from env: %v", err)
	}
	if store != nil {
		t.Fatalf("store = %T, want nil", store)
	}
	if publicBaseURL != "" {
		t.Fatalf("public base URL = %q, want empty", publicBaseURL)
	}
	if mediaBaseURL != "" {
		t.Fatalf("media base URL = %q, want empty", mediaBaseURL)
	}
}

func TestNewAvatarStoreFromEnvConfiguresLocalStore(t *testing.T) {
	store, publicBaseURL, mediaBaseURL, err := newAvatarStoreFromEnv(mapLookup(map[string]string{
		"ROUNDTABLE_AVATAR_STORE":          "local",
		"ROUNDTABLE_AVATAR_LOCAL_DIR":      "/tmp/roundtable-avatars",
		"ROUNDTABLE_AVATAR_MEDIA_BASE_URL": "https://roundtable.example.com/",
	}))
	if err != nil {
		t.Fatalf("new avatar store from env: %v", err)
	}
	local, ok := store.(*roundtable.LocalAvatarStore)
	if !ok {
		t.Fatalf("store = %T, want *roundtable.LocalAvatarStore", store)
	}
	if local.Dir != "/tmp/roundtable-avatars" {
		t.Fatalf("local dir = %q, want configured dir", local.Dir)
	}
	if publicBaseURL != "" {
		t.Fatalf("public base URL = %q, want empty", publicBaseURL)
	}
	if mediaBaseURL != "https://roundtable.example.com" {
		t.Fatalf("media base URL = %q, want trimmed configured URL", mediaBaseURL)
	}
}

func mapLookup(values map[string]string) func(string) string {
	return func(name string) string {
		return values[name]
	}
}
