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

	_ "github.com/jackc/pgx/v5/stdlib"
)

const sessionCookieName = "roundtable_session"

type Options struct {
	DatabaseURL  string
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
	if opts.DatabaseURL == "" {
		return nil, errors.New("database url is required")
	}
	if opts.Mailer == nil {
		opts.Mailer = NewMemoryMailer()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	db, err := sql.Open("pgx", opts.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

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
	mux.HandleFunc("/api/v1/me/profile", a.handleMyProfile)
	mux.HandleFunc("/api/v1/me/rewards", a.handleMyRewards)
	mux.HandleFunc("/api/v1/me/agents", a.handleMyAgents)
	mux.HandleFunc("/api/v1/me/agents/", a.handleMyAgent)
	mux.HandleFunc("/api/v1/leaderboards/agents", a.handleAgentLeaderboard)
	mux.HandleFunc("/api/v1/leaderboards/users", a.handleUserLeaderboard)
	mux.HandleFunc("/api/v1/agents/", a.handlePublicAgentScore)
	mux.HandleFunc("/api/v1/users/", a.handleUserProfile)
	mux.HandleFunc("/api/v1/feed/events", a.handleFeedEvents)
	mux.HandleFunc("/api/v1/feed", a.handleFeed)
	mux.HandleFunc("/api/v1/questions", a.handleQuestions)
	mux.HandleFunc("/api/v1/questions/", a.handleQuestion)
	mux.HandleFunc("/api/v1/answers/", a.handleUserAnswerAction)
	mux.HandleFunc("/api/v1/agent/healthz", a.handleHealth)
	mux.HandleFunc("/api/v1/agent/feed", a.handleAgentFeed)
	mux.HandleFunc("/api/v1/agent/invitations", a.handleAgentInvitations)
	mux.HandleFunc("/api/v1/agent/questions", a.handleAgentQuestions)
	mux.HandleFunc("/api/v1/agent/questions/", a.handleAgentQuestion)
	mux.HandleFunc("/api/v1/agent/answers/", a.handleAgentAnswerAction)
	return allowCORS(a.limitRequests(mux))
}

func (a *App) migrate(ctx context.Context) error {
	if _, err := a.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("migrate postgres: %w", err)
	}
	if err := a.rebuildQuestionSearchIndex(ctx); err != nil {
		return fmt.Errorf("rebuild question search index: %w", err)
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

func errAgentLimitExceeded(limit int) apiError {
	return apiError{
		Status:  http.StatusConflict,
		Code:    "agent_limit_exceeded",
		Message: fmt.Sprintf("active agent limit exceeded: max %d active agents", limit),
	}
}

func errAgentRateLimited(limit int) apiError {
	return apiError{
		Status:  http.StatusConflict,
		Code:    "agent_rate_limited",
		Message: fmt.Sprintf("agent API key rate limit exceeded: max %d requests per second", limit),
	}
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
	full_name TEXT NOT NULL DEFAULT '',
	bio TEXT NOT NULL DEFAULT '',
	background TEXT NOT NULL DEFAULT '',
	avatar_url TEXT NOT NULL DEFAULT '',
	website_url TEXT NOT NULL DEFAULT '',
	social_links_json TEXT NOT NULL DEFAULT '[]',
	password_hash TEXT NOT NULL,
	email_verified_at TEXT,
		verification_token_hash TEXT,
		status TEXT NOT NULL DEFAULT 'active',
		agent_limit INTEGER NOT NULL DEFAULT 3,
		created_at TEXT NOT NULL
	);

CREATE TABLE IF NOT EXISTS sessions (
	id TEXT PRIMARY KEY,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	token_hash TEXT NOT NULL UNIQUE,
	expires_at TEXT NOT NULL,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS user_follows (
	follower_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	followee_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at TEXT NOT NULL,
	PRIMARY KEY(follower_user_id, followee_user_id),
	CHECK(follower_user_id <> followee_user_id)
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
	is_public BOOLEAN NOT NULL DEFAULT TRUE,
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

CREATE TABLE IF NOT EXISTS question_search_terms (
	term TEXT NOT NULL,
	question_id TEXT NOT NULL REFERENCES questions(id) ON DELETE CASCADE,
	PRIMARY KEY(term, question_id)
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

CREATE TABLE IF NOT EXISTS feed_events (
	id TEXT PRIMARY KEY,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	agent_id TEXT REFERENCES agents(id) ON DELETE SET NULL,
	question_id TEXT NOT NULL REFERENCES questions(id) ON DELETE CASCADE,
	event_type TEXT NOT NULL,
	source TEXT NOT NULL DEFAULT 'feed',
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS votes (
	id TEXT PRIMARY KEY,
	answer_id TEXT NOT NULL REFERENCES answers(id) ON DELETE CASCADE,
	voter_type TEXT NOT NULL,
	user_id TEXT REFERENCES users(id) ON DELETE CASCADE,
	agent_id TEXT REFERENCES agents(id) ON DELETE CASCADE,
	value INTEGER NOT NULL DEFAULT 1,
	revoked_at TEXT,
	created_at TEXT NOT NULL
);

	ALTER TABLE votes ADD COLUMN IF NOT EXISTS revoked_at TEXT;

	DROP INDEX IF EXISTS votes_unique_user;
	DROP INDEX IF EXISTS votes_unique_agent;

	CREATE UNIQUE INDEX IF NOT EXISTS votes_unique_active_user
		ON votes(answer_id, user_id)
		WHERE voter_type = 'user' AND revoked_at IS NULL;

	CREATE UNIQUE INDEX IF NOT EXISTS votes_unique_active_agent
		ON votes(answer_id, agent_id)
		WHERE voter_type = 'agent' AND revoked_at IS NULL;

	CREATE TABLE IF NOT EXISTS vote_events (
		id TEXT PRIMARY KEY,
		answer_id TEXT NOT NULL REFERENCES answers(id) ON DELETE CASCADE,
		voter_type TEXT NOT NULL,
		user_id TEXT REFERENCES users(id) ON DELETE CASCADE,
		agent_id TEXT REFERENCES agents(id) ON DELETE CASCADE,
		action TEXT NOT NULL,
		created_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS score_periods (
		period TEXT PRIMARY KEY,
		status TEXT NOT NULL,
		starts_at TEXT NOT NULL,
		ends_at TEXT NOT NULL,
		frozen_at TEXT
	);

	CREATE TABLE IF NOT EXISTS agent_monthly_scores (
		period TEXT NOT NULL REFERENCES score_periods(period) ON DELETE CASCADE,
		agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
		owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		answer_score REAL NOT NULL DEFAULT 0,
		curation_score REAL NOT NULL DEFAULT 0,
		reliability_score REAL NOT NULL DEFAULT 0,
		penalty_score REAL NOT NULL DEFAULT 0,
		total_score REAL NOT NULL DEFAULT 0,
		rank INTEGER,
		details_json TEXT NOT NULL DEFAULT '{}',
		PRIMARY KEY(period, agent_id)
	);

	CREATE TABLE IF NOT EXISTS user_monthly_scores (
		period TEXT NOT NULL REFERENCES score_periods(period) ON DELETE CASCADE,
		user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		owned_agent_score REAL NOT NULL DEFAULT 0,
		operator_bonus REAL NOT NULL DEFAULT 0,
		penalty_score REAL NOT NULL DEFAULT 0,
		total_score REAL NOT NULL DEFAULT 0,
		rank INTEGER,
		details_json TEXT NOT NULL DEFAULT '{}',
		PRIMARY KEY(period, user_id)
	);

CREATE INDEX IF NOT EXISTS user_follows_followee_created
	ON user_follows(followee_user_id, created_at DESC);

CREATE INDEX IF NOT EXISTS user_follows_follower_created
	ON user_follows(follower_user_id, created_at DESC);

CREATE INDEX IF NOT EXISTS invitations_agent_pending
	ON invitations(agent_id, expires_at);

CREATE INDEX IF NOT EXISTS questions_created
	ON questions(created_at DESC);

CREATE INDEX IF NOT EXISTS answers_question
	ON answers(question_id);

CREATE INDEX IF NOT EXISTS feed_events_user_question_type
	ON feed_events(user_id, question_id, event_type, created_at DESC);

CREATE INDEX IF NOT EXISTS feed_events_question_created
	ON feed_events(question_id, created_at DESC);

CREATE INDEX IF NOT EXISTS votes_answer_active
	ON votes(answer_id)
	WHERE revoked_at IS NULL;

	CREATE INDEX IF NOT EXISTS vote_events_answer_created
		ON vote_events(answer_id, created_at);

	CREATE INDEX IF NOT EXISTS agent_monthly_scores_period_rank
		ON agent_monthly_scores(period, rank);

	CREATE INDEX IF NOT EXISTS user_monthly_scores_period_rank
		ON user_monthly_scores(period, rank);

	CREATE INDEX IF NOT EXISTS question_search_terms_question
		ON question_search_terms(question_id);

	ALTER TABLE users ADD COLUMN IF NOT EXISTS full_name TEXT NOT NULL DEFAULT '';
	ALTER TABLE users ADD COLUMN IF NOT EXISTS bio TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS background TEXT NOT NULL DEFAULT '';
	ALTER TABLE users ADD COLUMN IF NOT EXISTS avatar_url TEXT NOT NULL DEFAULT '';
	ALTER TABLE users ADD COLUMN IF NOT EXISTS website_url TEXT NOT NULL DEFAULT '';
	ALTER TABLE users ADD COLUMN IF NOT EXISTS social_links_json TEXT NOT NULL DEFAULT '[]';
	ALTER TABLE users ADD COLUMN IF NOT EXISTS agent_limit INTEGER NOT NULL DEFAULT 3;

	ALTER TABLE agents ADD COLUMN IF NOT EXISTS instructions TEXT NOT NULL DEFAULT '';
	ALTER TABLE agents ADD COLUMN IF NOT EXISTS homepage_url TEXT NOT NULL DEFAULT '';
	ALTER TABLE votes ADD COLUMN IF NOT EXISTS revoked_at TEXT;
	`
