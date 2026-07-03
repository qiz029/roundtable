package roundtable

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const sessionCookieName = "roundtable_session"

type Options struct {
	DBPath       string
	Mailer       Mailer
	Now          func() time.Time
	CookieSecure bool
	RateLimit    RateLimitConfig
}

type App struct {
	db           *sql.DB
	mailer       Mailer
	now          func() time.Time
	cookieSecure bool
	limiter      *rateLimiter
}

func NewApp(opts Options) (*App, error) {
	if opts.DBPath == "" {
		return nil, errors.New("db path is required")
	}
	if opts.Mailer == nil {
		opts.Mailer = NewMemoryMailer()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	db, err := sql.Open("sqlite3", sqliteDSN(opts.DBPath))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)

	app := &App{
		db:           db,
		mailer:       opts.Mailer,
		now:          opts.Now,
		cookieSecure: opts.CookieSecure,
		limiter:      newRateLimiter(opts.RateLimit),
	}
	if err := app.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return app, nil
}

func (a *App) Close() error {
	return a.db.Close()
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", a.handleHealth)
	mux.HandleFunc("/api/v1/auth/register", a.handleRegister)
	mux.HandleFunc("/api/v1/auth/verify", a.handleVerify)
	mux.HandleFunc("/api/v1/auth/login", a.handleLogin)
	mux.HandleFunc("/api/v1/auth/logout", a.handleLogout)
	mux.HandleFunc("/api/v1/auth/me", a.handleMe)
	mux.HandleFunc("/api/v1/me/agents", a.handleMyAgents)
	mux.HandleFunc("/api/v1/me/agents/", a.handleMyAgent)
	mux.HandleFunc("/api/v1/questions", a.handleQuestions)
	mux.HandleFunc("/api/v1/questions/", a.handleQuestion)
	mux.HandleFunc("/api/v1/answers/", a.handleUserAnswerAction)
	mux.HandleFunc("/api/v1/agent/invitations", a.handleAgentInvitations)
	mux.HandleFunc("/api/v1/agent/questions", a.handleAgentQuestions)
	mux.HandleFunc("/api/v1/agent/questions/", a.handleAgentQuestion)
	mux.HandleFunc("/api/v1/agent/answers/", a.handleAgentAnswerAction)
	return allowCORS(a.limitRequests(mux))
}

func sqliteDSN(path string) string {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return path + separator + "_foreign_keys=on&_busy_timeout=5000"
}

func (a *App) migrate(ctx context.Context) error {
	if _, err := a.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("migrate sqlite: %w", err)
	}
	return nil
}

type apiError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e apiError) Error() string {
	return e.Message
}

func errInvalidInput(message string) apiError {
	return apiError{Status: http.StatusBadRequest, Code: "invalid_input", Message: message}
}

func errUnauthorized() apiError {
	return apiError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "authentication required"}
}

func errLoginRequired(action string) apiError {
	message := "login required"
	if action != "" {
		message = "login required to " + action
	}
	return apiError{Status: http.StatusUnauthorized, Code: "login_required", Message: message}
}

func errForbidden(message string) apiError {
	return apiError{Status: http.StatusForbidden, Code: "forbidden", Message: message}
}

func errNotFound(message string) apiError {
	return apiError{Status: http.StatusNotFound, Code: "not_found", Message: message}
}

func errConflict(message string) apiError {
	return apiError{Status: http.StatusConflict, Code: "conflict", Message: message}
}

func errMethodNotAllowed() apiError {
	return apiError{Status: http.StatusMethodNotAllowed, Code: "method_not_allowed", Message: "method not allowed"}
}

func errRateLimited() apiError {
	return apiError{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: "too many requests"}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func writeError(w http.ResponseWriter, err error) {
	var apiErr apiError
	if errors.As(err, &apiErr) {
		writeJSON(w, apiErr.Status, apiErr)
		return
	}
	writeJSON(w, http.StatusInternalServerError, apiError{
		Status:  http.StatusInternalServerError,
		Code:    "internal_error",
		Message: "internal server error",
	})
}

func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return errInvalidInput("invalid json body")
	}
	return nil
}

func encodeStringList(values []string) (string, error) {
	if values == nil {
		values = []string{}
	}
	for i := range values {
		values[i] = strings.TrimSpace(values[i])
	}
	raw, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("marshal string list: %w", err)
	}
	return string(raw), nil
}

func decodeStringList(raw string) []string {
	var values []string
	if raw == "" {
		return []string{}
	}
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return []string{}
	}
	return values
}

func pathTail(path string, prefix string) string {
	return strings.Trim(strings.TrimPrefix(path, prefix), "/")
}

func singlePathID(path string, prefix string) (string, bool) {
	tail := pathTail(path, prefix)
	if tail == "" || strings.Contains(tail, "/") {
		return "", false
	}
	return tail, true
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS users (
	id TEXT PRIMARY KEY,
	email TEXT NOT NULL UNIQUE,
	display_name TEXT NOT NULL,
	password_hash TEXT NOT NULL,
	email_verified_at TEXT,
	verification_token_hash TEXT,
	status TEXT NOT NULL DEFAULT 'active',
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
	id TEXT PRIMARY KEY,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	token_hash TEXT NOT NULL UNIQUE,
	expires_at TEXT NOT NULL,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS agents (
	id TEXT PRIMARY KEY,
	owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	tags_json TEXT NOT NULL DEFAULT '[]',
	capabilities_json TEXT NOT NULL DEFAULT '[]',
	instructions TEXT NOT NULL DEFAULT '',
	homepage_url TEXT NOT NULL DEFAULT '',
	is_public INTEGER NOT NULL DEFAULT 1,
	status TEXT NOT NULL DEFAULT 'active',
	token_hash TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS questions (
	id TEXT PRIMARY KEY,
	author_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	title TEXT NOT NULL,
	body TEXT NOT NULL,
	tags_json TEXT NOT NULL DEFAULT '[]',
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS invitations (
	id TEXT PRIMARY KEY,
	question_id TEXT NOT NULL REFERENCES questions(id) ON DELETE CASCADE,
	agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
	expires_at TEXT NOT NULL,
	answered_at TEXT,
	created_at TEXT NOT NULL,
	UNIQUE(question_id, agent_id)
);

CREATE TABLE IF NOT EXISTS answers (
	id TEXT PRIMARY KEY,
	question_id TEXT NOT NULL REFERENCES questions(id) ON DELETE CASCADE,
	agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
	invitation_id TEXT REFERENCES invitations(id) ON DELETE SET NULL,
	body TEXT NOT NULL,
	created_at TEXT NOT NULL,
	UNIQUE(question_id, agent_id)
);

CREATE TABLE IF NOT EXISTS votes (
	id TEXT PRIMARY KEY,
	answer_id TEXT NOT NULL REFERENCES answers(id) ON DELETE CASCADE,
	voter_type TEXT NOT NULL,
	user_id TEXT REFERENCES users(id) ON DELETE CASCADE,
	agent_id TEXT REFERENCES agents(id) ON DELETE CASCADE,
	value INTEGER NOT NULL DEFAULT 1,
	created_at TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS votes_unique_user
	ON votes(answer_id, user_id)
	WHERE voter_type = 'user';

CREATE UNIQUE INDEX IF NOT EXISTS votes_unique_agent
	ON votes(answer_id, agent_id)
	WHERE voter_type = 'agent';

CREATE INDEX IF NOT EXISTS invitations_agent_pending
	ON invitations(agent_id, expires_at);

CREATE INDEX IF NOT EXISTS answers_question
	ON answers(question_id);
`
