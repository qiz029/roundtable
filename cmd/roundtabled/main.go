package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/qiz029/roundtable/internal/roundtable"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	addr := flag.String("addr", envDefault("ROUNDTABLE_ADDR", ":8080"), "HTTP listen address")
	databaseURL := flag.String("database-url", envDefault("ROUNDTABLE_DATABASE_URL", ""), "Postgres connection URL")
	secureCookie := flag.Bool("secure-cookie", envBool("ROUNDTABLE_SECURE_COOKIE"), "Set Secure on session cookies")
	flag.Parse()

	avatarStore, avatarPublicBaseURL, avatarMediaBaseURL, err := newAvatarStoreFromEnv(os.Getenv)
	if err != nil {
		fatal(logger, "configure_avatar_store_failed", err)
	}
	mailer, err := newMailerFromEnv(os.Getenv, os.Stderr)
	if err != nil {
		fatal(logger, "configure_mailer_failed", err)
	}
	translationProvider, err := newTranslationProviderFromEnv(os.Getenv)
	if err != nil {
		fatal(logger, "configure_translation_provider_failed", err)
	}
	translationWorker, err := newTranslationWorkerConfigFromEnv(os.Getenv)
	if err != nil {
		fatal(logger, "configure_translation_worker_failed", err)
	}
	app, err := roundtable.NewApp(roundtable.Options{
		DatabaseURL:         *databaseURL,
		Mailer:              mailer,
		CookieSecure:        *secureCookie,
		AvatarStore:         avatarStore,
		AvatarPublicBaseURL: avatarPublicBaseURL,
		AvatarMediaBaseURL:  avatarMediaBaseURL,
		TranslationProvider: translationProvider,
		TranslationWorker:   translationWorker,
		Logger:              logger,
	})
	if err != nil {
		fatal(logger, "new_app_failed", err)
	}
	defer app.Close()

	server := &http.Server{
		Addr:    *addr,
		Handler: app.Handler(),
	}
	logger.Info("roundtabled_listening", "addr", *addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fatal(logger, "http_server_failed", err)
	}
}

func newTranslationProviderFromEnv(getenv func(string) string) (roundtable.TranslationProvider, error) {
	opts, ok, err := newDeepSeekTranslationProviderOptionsFromEnv(getenv)
	if err != nil || !ok {
		return nil, err
	}
	return roundtable.NewDeepSeekTranslationProvider(opts)
}

func newDeepSeekTranslationProviderOptionsFromEnv(getenv func(string) string) (roundtable.DeepSeekTranslationProviderOptions, bool, error) {
	apiKey := strings.TrimSpace(getenv("DEEPSEEK_API_KEY"))
	if apiKey == "" {
		return roundtable.DeepSeekTranslationProviderOptions{}, false, nil
	}
	opts := roundtable.DeepSeekTranslationProviderOptions{
		APIKey:     apiKey,
		APIBaseURL: getenv("DEEPSEEK_API_BASE_URL"),
		Model:      getenv("TRANSLATION_MODEL"),
	}
	if err := setIntEnv(getenv, "TRANSLATION_INPUT_COST_MICROS_PER_MILLION", &opts.InputCostMicrosPerMillion); err != nil {
		return roundtable.DeepSeekTranslationProviderOptions{}, false, err
	}
	if err := setIntEnv(getenv, "TRANSLATION_OUTPUT_COST_MICROS_PER_MILLION", &opts.OutputCostMicrosPerMillion); err != nil {
		return roundtable.DeepSeekTranslationProviderOptions{}, false, err
	}
	return opts, true, nil
}

func newTranslationWorkerConfigFromEnv(getenv func(string) string) (roundtable.TranslationWorkerConfig, error) {
	config := roundtable.TranslationWorkerConfig{
		Enabled: envBoolValue(getenv("TRANSLATION_WORKER_ENABLED")),
	}
	if err := setDurationEnv(getenv, "TRANSLATION_WORKER_POLL_INTERVAL", &config.PollInterval); err != nil {
		return roundtable.TranslationWorkerConfig{}, err
	}
	if err := setIntEnv(getenv, "TRANSLATION_WORKER_BATCH_SIZE", &config.BatchSize); err != nil {
		return roundtable.TranslationWorkerConfig{}, err
	}
	if err := setIntEnv(getenv, "TRANSLATION_WORKER_MAX_CONCURRENCY", &config.MaxConcurrency); err != nil {
		return roundtable.TranslationWorkerConfig{}, err
	}
	if err := setIntEnv(getenv, "TRANSLATION_WORKER_MAX_ATTEMPTS", &config.MaxAttempts); err != nil {
		return roundtable.TranslationWorkerConfig{}, err
	}
	if err := setDurationEnv(getenv, "TRANSLATION_WORKER_RETRY_BASE_DELAY", &config.RetryBaseDelay); err != nil {
		return roundtable.TranslationWorkerConfig{}, err
	}
	if err := setIntEnv(getenv, "TRANSLATION_DAILY_BUDGET_MICROS", &config.DailyBudgetMicros); err != nil {
		return roundtable.TranslationWorkerConfig{}, err
	}
	if err := setIntEnv(getenv, "TRANSLATION_ESTIMATED_COST_MICROS", &config.EstimatedCostMicros); err != nil {
		return roundtable.TranslationWorkerConfig{}, err
	}
	return config, nil
}

func fatal(logger *slog.Logger, message string, err error) {
	logger.Error(message, "error", err)
	os.Exit(1)
}

func newAvatarStoreFromEnv(getenv func(string) string) (roundtable.AvatarStore, string, string, error) {
	publicBaseURL := ""
	if envBoolValue(getenv("ROUNDTABLE_AVATAR_DIRECT_PUBLIC_URLS")) {
		publicBaseURL = strings.TrimRight(strings.TrimSpace(getenv("ROUNDTABLE_AVATAR_PUBLIC_BASE_URL")), "/")
	}
	mediaBaseURL := strings.TrimRight(strings.TrimSpace(getenv("ROUNDTABLE_AVATAR_MEDIA_BASE_URL")), "/")
	switch strings.ToLower(strings.TrimSpace(getenv("ROUNDTABLE_AVATAR_STORE"))) {
	case "", "disabled":
		return nil, publicBaseURL, mediaBaseURL, nil
	case "local":
		dir := strings.TrimSpace(getenv("ROUNDTABLE_AVATAR_LOCAL_DIR"))
		if dir == "" {
			dir = "data/avatars"
		}
		store, err := roundtable.NewLocalAvatarStore(dir)
		return store, "", mediaBaseURL, err
	case "s3":
		store, err := roundtable.NewS3AvatarStore(roundtable.S3AvatarStore{
			Endpoint:        getenv("ROUNDTABLE_AVATAR_S3_ENDPOINT"),
			Region:          getenv("ROUNDTABLE_AVATAR_S3_REGION"),
			Bucket:          getenv("ROUNDTABLE_AVATAR_S3_BUCKET"),
			AccessKeyID:     getenv("ROUNDTABLE_AVATAR_S3_ACCESS_KEY_ID"),
			SecretAccessKey: getenv("ROUNDTABLE_AVATAR_S3_SECRET_ACCESS_KEY"),
			ForcePathStyle:  envBoolValue(getenv("ROUNDTABLE_AVATAR_S3_FORCE_PATH_STYLE")),
		})
		return store, publicBaseURL, mediaBaseURL, err
	default:
		return nil, publicBaseURL, mediaBaseURL, fmt.Errorf("unsupported avatar store %q", getenv("ROUNDTABLE_AVATAR_STORE"))
	}
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
	return envBoolValue(os.Getenv(name))
}

func envBoolValue(value string) bool {
	switch value {
	case "1", "true", "TRUE", "yes", "YES":
		return true
	default:
		return false
	}
}

func setIntEnv(getenv func(string) string, name string, target *int) error {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("%s must be an integer", name)
	}
	if value < 0 {
		return fmt.Errorf("%s must be non-negative", name)
	}
	*target = value
	return nil
}

func setDurationEnv(getenv func(string) string, name string, target *time.Duration) error {
	raw := strings.TrimSpace(getenv(name))
	if raw == "" {
		return nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("%s must be a Go duration such as 30s or 1m", name)
	}
	if value < 0 {
		return fmt.Errorf("%s must be non-negative", name)
	}
	*target = value
	return nil
}
