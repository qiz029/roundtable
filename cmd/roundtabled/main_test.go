package main

import (
	"io"
	"testing"
	"time"

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
		"ROUNDTABLE_AVATAR_STORE":           "local",
		"ROUNDTABLE_AVATAR_LOCAL_DIR":       "/tmp/roundtable-avatars",
		"ROUNDTABLE_AVATAR_MEDIA_BASE_URL":  "https://roundtable.example.com/",
		"ROUNDTABLE_AVATAR_PUBLIC_BASE_URL": "https://roundtable.example.com",
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
		t.Fatalf("public base URL = %q, want ignored for local store", publicBaseURL)
	}
	if mediaBaseURL != "https://roundtable.example.com" {
		t.Fatalf("media base URL = %q, want trimmed configured URL", mediaBaseURL)
	}
}

func TestNewAvatarStoreFromEnvRequiresExplicitDirectPublicURLs(t *testing.T) {
	store, publicBaseURL, mediaBaseURL, err := newAvatarStoreFromEnv(mapLookup(map[string]string{
		"ROUNDTABLE_AVATAR_STORE":                "s3",
		"ROUNDTABLE_AVATAR_S3_ENDPOINT":          "https://objects.example.com",
		"ROUNDTABLE_AVATAR_S3_REGION":            "us-west-2",
		"ROUNDTABLE_AVATAR_S3_BUCKET":            "roundtable-avatars",
		"ROUNDTABLE_AVATAR_S3_ACCESS_KEY_ID":     "test-access-key",
		"ROUNDTABLE_AVATAR_S3_SECRET_ACCESS_KEY": "test-secret-key",
		"ROUNDTABLE_AVATAR_PUBLIC_BASE_URL":      "https://roundtable.example.com/avatars",
	}))
	if err != nil {
		t.Fatalf("new avatar store from env: %v", err)
	}
	if _, ok := store.(*roundtable.S3AvatarStore); !ok {
		t.Fatalf("store = %T, want *roundtable.S3AvatarStore", store)
	}
	if publicBaseURL != "" {
		t.Fatalf("public base URL = %q, want ignored unless direct public URLs are enabled", publicBaseURL)
	}
	if mediaBaseURL != "" {
		t.Fatalf("media base URL = %q, want empty", mediaBaseURL)
	}

	_, publicBaseURL, _, err = newAvatarStoreFromEnv(mapLookup(map[string]string{
		"ROUNDTABLE_AVATAR_STORE":                "s3",
		"ROUNDTABLE_AVATAR_S3_ENDPOINT":          "https://objects.example.com",
		"ROUNDTABLE_AVATAR_S3_REGION":            "us-west-2",
		"ROUNDTABLE_AVATAR_S3_BUCKET":            "roundtable-avatars",
		"ROUNDTABLE_AVATAR_S3_ACCESS_KEY_ID":     "test-access-key",
		"ROUNDTABLE_AVATAR_S3_SECRET_ACCESS_KEY": "test-secret-key",
		"ROUNDTABLE_AVATAR_PUBLIC_BASE_URL":      "https://cdn.example.com/avatars/",
		"ROUNDTABLE_AVATAR_DIRECT_PUBLIC_URLS":   "true",
	}))
	if err != nil {
		t.Fatalf("new avatar store from env with direct public URLs: %v", err)
	}
	if publicBaseURL != "https://cdn.example.com/avatars" {
		t.Fatalf("public base URL = %q, want trimmed configured URL", publicBaseURL)
	}
}

func TestNewTranslationProviderFromEnvDefaultsDisabledWithoutKey(t *testing.T) {
	provider, err := newTranslationProviderFromEnv(mapLookup(map[string]string{}))
	if err != nil {
		t.Fatalf("new translation provider from env: %v", err)
	}
	if provider != nil {
		t.Fatalf("provider = %T, want nil", provider)
	}
}

func TestNewTranslationProviderFromEnvConfiguresDeepSeek(t *testing.T) {
	provider, err := newTranslationProviderFromEnv(mapLookup(map[string]string{
		"DEEPSEEK_API_KEY":      "test-deepseek-key",
		"DEEPSEEK_API_BASE_URL": "https://deepseek.example.com/",
		"TRANSLATION_MODEL":     "deepseek-v4-flash",
	}))
	if err != nil {
		t.Fatalf("new translation provider from env: %v", err)
	}
	if _, ok := provider.(*roundtable.DeepSeekTranslationProvider); !ok {
		t.Fatalf("provider = %T, want *roundtable.DeepSeekTranslationProvider", provider)
	}
}

func TestNewTranslationWorkerConfigFromEnv(t *testing.T) {
	config, err := newTranslationWorkerConfigFromEnv(mapLookup(map[string]string{
		"TRANSLATION_WORKER_ENABLED":          "true",
		"TRANSLATION_WORKER_POLL_INTERVAL":    "15s",
		"TRANSLATION_WORKER_BATCH_SIZE":       "25",
		"TRANSLATION_WORKER_MAX_CONCURRENCY":  "3",
		"TRANSLATION_WORKER_MAX_ATTEMPTS":     "4",
		"TRANSLATION_WORKER_RETRY_BASE_DELAY": "45s",
		"TRANSLATION_DAILY_BUDGET_MICROS":     "100000",
		"TRANSLATION_ESTIMATED_COST_MICROS":   "100",
	}))
	if err != nil {
		t.Fatalf("new translation worker config: %v", err)
	}
	if !config.Enabled {
		t.Fatal("worker enabled = false, want true")
	}
	if config.PollInterval != 15*time.Second {
		t.Fatalf("poll interval = %s", config.PollInterval)
	}
	if config.BatchSize != 25 {
		t.Fatalf("batch size = %d", config.BatchSize)
	}
	if config.MaxConcurrency != 3 {
		t.Fatalf("max concurrency = %d", config.MaxConcurrency)
	}
	if config.MaxAttempts != 4 {
		t.Fatalf("max attempts = %d", config.MaxAttempts)
	}
	if config.RetryBaseDelay != 45*time.Second {
		t.Fatalf("retry base delay = %s", config.RetryBaseDelay)
	}
	if config.DailyBudgetMicros != 100000 {
		t.Fatalf("daily budget = %d", config.DailyBudgetMicros)
	}
	if config.EstimatedCostMicros != 100 {
		t.Fatalf("estimated cost = %d", config.EstimatedCostMicros)
	}
}

func TestNewTranslationWorkerConfigFromEnvRejectsBadValues(t *testing.T) {
	if _, err := newTranslationWorkerConfigFromEnv(mapLookup(map[string]string{
		"TRANSLATION_WORKER_BATCH_SIZE": "nope",
	})); err == nil {
		t.Fatal("expected bad batch size error")
	}
	if _, err := newTranslationWorkerConfigFromEnv(mapLookup(map[string]string{
		"TRANSLATION_WORKER_POLL_INTERVAL": "soon",
	})); err == nil {
		t.Fatal("expected bad poll interval error")
	}
}

func mapLookup(values map[string]string) func(string) string {
	return func(name string) string {
		return values[name]
	}
}
