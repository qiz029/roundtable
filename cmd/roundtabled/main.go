package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/qiz029/roundtable/internal/roundtable"
)

func main() {
	addr := flag.String("addr", envDefault("ROUNDTABLE_ADDR", ":8080"), "HTTP listen address")
	dbPath := flag.String("db", envDefault("ROUNDTABLE_DB_PATH", "./roundtable.db"), "SQLite database path")
	secureCookie := flag.Bool("secure-cookie", envBool("ROUNDTABLE_SECURE_COOKIE"), "Set Secure on session cookies")
	flag.Parse()

	app, err := roundtable.NewApp(roundtable.Options{
		DBPath:       *dbPath,
		Mailer:       configuredMailer(),
		CookieSecure: *secureCookie,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer app.Close()

	server := &http.Server{
		Addr:    *addr,
		Handler: app.Handler(),
	}
	fmt.Fprintf(os.Stderr, "roundtabled listening on %s\n", *addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func configuredMailer() roundtable.Mailer {
	mailer, err := newMailerFromEnv(os.Getenv, os.Stderr)
	if err != nil {
		log.Fatal(err)
	}
	return mailer
}

func newMailerFromEnv(getenv func(string) string, logWriter io.Writer) (roundtable.Mailer, error) {
	switch strings.ToLower(strings.TrimSpace(getenv("ROUNDTABLE_MAILER"))) {
	case "", "auto":
		if hasMailgunConfig(getenv) {
			return newMailgunMailerFromEnv(getenv)
		}
		if hasSMTPConfig(getenv) {
			return newSMTPMailerFromEnv(getenv)
		}
		return roundtable.NewLogMailer(logWriter), nil
	case "log":
		return roundtable.NewLogMailer(logWriter), nil
	case "mailgun":
		return newMailgunMailerFromEnv(getenv)
	case "smtp":
		return newSMTPMailerFromEnv(getenv)
	default:
		return nil, fmt.Errorf("unsupported mailer provider %q", getenv("ROUNDTABLE_MAILER"))
	}
}

func newSMTPMailerFromEnv(getenv func(string) string) (roundtable.Mailer, error) {
	mailer, err := roundtable.NewSMTPMailer(roundtable.SMTPOptions{
		Addr:      getenv("ROUNDTABLE_SMTP_ADDR"),
		Username:  getenv("ROUNDTABLE_SMTP_USERNAME"),
		Password:  getenv("ROUNDTABLE_SMTP_PASSWORD"),
		From:      getenv("ROUNDTABLE_SMTP_FROM"),
		PublicURL: getenv("ROUNDTABLE_PUBLIC_URL"),
	})
	if err != nil {
		return nil, err
	}
	return mailer, nil
}

func newMailgunMailerFromEnv(getenv func(string) string) (roundtable.Mailer, error) {
	mailer, err := roundtable.NewMailgunMailer(roundtable.MailgunOptions{
		APIBaseURL: getenv("ROUNDTABLE_MAILGUN_API_BASE"),
		Domain:     getenv("ROUNDTABLE_MAILGUN_DOMAIN"),
		APIKey:     getenv("ROUNDTABLE_MAILGUN_API_KEY"),
		From:       getenv("ROUNDTABLE_MAILGUN_FROM"),
		PublicURL:  getenv("ROUNDTABLE_PUBLIC_URL"),
	})
	if err != nil {
		return nil, err
	}
	return mailer, nil
}

func hasMailgunConfig(getenv func(string) string) bool {
	return getenv("ROUNDTABLE_MAILGUN_DOMAIN") != "" ||
		getenv("ROUNDTABLE_MAILGUN_API_KEY") != "" ||
		getenv("ROUNDTABLE_MAILGUN_FROM") != ""
}

func hasSMTPConfig(getenv func(string) string) bool {
	return getenv("ROUNDTABLE_SMTP_ADDR") != "" ||
		getenv("ROUNDTABLE_SMTP_FROM") != ""
}

func envDefault(name string, fallback string) string {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	return value
}

func envBool(name string) bool {
	switch os.Getenv(name) {
	case "1", "true", "TRUE", "yes", "YES":
		return true
	default:
		return false
	}
}
