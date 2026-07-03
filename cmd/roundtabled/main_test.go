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

func mapLookup(values map[string]string) func(string) string {
	return func(name string) string {
		return values[name]
	}
}
