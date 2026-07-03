package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

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
	smtpAddr := os.Getenv("ROUNDTABLE_SMTP_ADDR")
	smtpFrom := os.Getenv("ROUNDTABLE_SMTP_FROM")
	if smtpAddr == "" || smtpFrom == "" {
		return roundtable.NewLogMailer(os.Stderr)
	}
	mailer, err := roundtable.NewSMTPMailer(roundtable.SMTPOptions{
		Addr:      smtpAddr,
		Username:  os.Getenv("ROUNDTABLE_SMTP_USERNAME"),
		Password:  os.Getenv("ROUNDTABLE_SMTP_PASSWORD"),
		From:      smtpFrom,
		PublicURL: os.Getenv("ROUNDTABLE_PUBLIC_URL"),
	})
	if err != nil {
		log.Fatal(err)
	}
	return mailer
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
