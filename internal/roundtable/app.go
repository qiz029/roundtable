package roundtable

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const sessionCookieName = "roundtable_session"

type Options struct {
	DatabaseURL         string
	Mailer              Mailer
	Now                 func() time.Time
	CookieSecure        bool
	RateLimit           RateLimitConfig
	AvatarStore         AvatarStore
	AvatarPublicBaseURL string
	AvatarMediaBaseURL  string
	TranslationProvider TranslationProvider
	TranslationWorker   TranslationWorkerConfig
	Logger              *slog.Logger
}

type App struct {
	db                  *sql.DB
	mailer              Mailer
	now                 func() time.Time
	cookieSecure        bool
	limiter             *rateLimiter
	avatarStore         AvatarStore
	avatarPublicBaseURL string
	avatarMediaBaseURL  string
	translationProvider TranslationProvider
	translationWorker   TranslationWorkerConfig
	translationCancel   context.CancelFunc
	translationDone     chan struct{}
	logger              *slog.Logger
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
		db:                  db,
		mailer:              opts.Mailer,
		now:                 opts.Now,
		cookieSecure:        opts.CookieSecure,
		limiter:             newRateLimiter(opts.RateLimit),
		avatarStore:         opts.AvatarStore,
		avatarPublicBaseURL: strings.TrimRight(strings.TrimSpace(opts.AvatarPublicBaseURL), "/"),
		avatarMediaBaseURL:  strings.TrimRight(strings.TrimSpace(opts.AvatarMediaBaseURL), "/"),
		translationProvider: opts.TranslationProvider,
		translationWorker:   normalizeTranslationWorkerConfig(opts.TranslationWorker),
		logger:              opts.Logger,
	}
	if err := app.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	app.startTranslationWorker()
	return app, nil
}

func (a *App) Close() error {
	if a.translationCancel != nil {
		a.translationCancel()
		if a.translationDone != nil {
			<-a.translationDone
		}
	}
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
	mux.HandleFunc("/api/v1/media/avatars/", a.handleAvatarMedia)
	mux.HandleFunc("/api/v1/me/avatar", a.handleMyAvatar)
	mux.HandleFunc("/api/v1/me/profile", a.handleMyProfile)
	mux.HandleFunc("/api/v1/me/rewards", a.handleMyRewards)
	mux.HandleFunc("/api/v1/me/agents", a.handleMyAgents)
	mux.HandleFunc("/api/v1/me/agents/", a.handleMyAgent)
	mux.HandleFunc("/api/v1/leaderboards/agents", a.handleAgentLeaderboard)
	mux.HandleFunc("/api/v1/leaderboards/users", a.handleUserLeaderboard)
	mux.HandleFunc("/api/v1/agents/", a.handlePublicAgent)
	mux.HandleFunc("/api/v1/users/", a.handleUserProfile)
	mux.HandleFunc("/api/v1/feed/answers", a.handleAnswerFeed)
	mux.HandleFunc("/api/v1/feed/events", a.handleFeedEvents)
	mux.HandleFunc("/api/v1/feed", a.handleFeed)
	mux.HandleFunc("/api/v1/translations", a.handleTranslations)
	mux.HandleFunc("/api/v1/questions", a.handleQuestions)
	mux.HandleFunc("/api/v1/questions/", a.handleQuestion)
	mux.HandleFunc("/api/v1/answers/", a.handleUserAnswerAction)
	mux.HandleFunc("/api/v1/comments/", a.handleCommentAction)
	mux.HandleFunc("/api/v1/agent/healthz", a.handleHealth)
	mux.HandleFunc("/api/v1/agent/avatar", a.handleAgentAvatar)
	mux.HandleFunc("/api/v1/agent/profile", a.handleAgentProfile)
	mux.HandleFunc("/api/v1/agent/feed", a.handleAgentFeed)
	mux.HandleFunc("/api/v1/agent/invitations", a.handleAgentInvitations)
	mux.HandleFunc("/api/v1/agent/questions", a.handleAgentQuestions)
	mux.HandleFunc("/api/v1/agent/questions/", a.handleAgentQuestion)
	mux.HandleFunc("/api/v1/agent/answers/", a.handleAgentAnswerAction)
	mux.HandleFunc("/api/v1/agent/responses/", a.handleAgentResponseAction)
	return a.observeRequests(allowCORS(a.limitRequests(mux)))
}

func (a *App) migrate(ctx context.Context) error {
	if _, err := a.db.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("migrate postgres: %w", err)
	}
	if err := a.rebuildQuestionTagIndex(ctx); err != nil {
		return fmt.Errorf("rebuild question tag index: %w", err)
	}
	if err := a.rebuildQuestionSearchIndex(ctx); err != nil {
		return fmt.Errorf("rebuild question search index: %w", err)
	}
	if err := a.enqueueTranslationBackfill(ctx); err != nil {
		return fmt.Errorf("enqueue translation backfill: %w", err)
	}
	return nil
}

type apiError struct {
	Status    int    `json:"-"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
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
	requestID := w.Header().Get(requestIDHeader)
	var apiErr apiError
	if errors.As(err, &apiErr) {
		apiErr.RequestID = requestID
		writeJSON(w, apiErr.Status, apiErr)
		return
	}
	writeJSON(w, http.StatusInternalServerError, apiError{
		Status:    http.StatusInternalServerError,
		Code:      "internal_error",
		Message:   "internal server error",
		RequestID: requestID,
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
	avatar_object_key TEXT NOT NULL DEFAULT '',
	avatar_content_type TEXT NOT NULL DEFAULT '',
	avatar_updated_at TEXT,
	website_url TEXT NOT NULL DEFAULT '',
	social_links_json TEXT NOT NULL DEFAULT '[]',
	password_hash TEXT NOT NULL,
	email_verified_at TEXT,
	verification_token_hash TEXT,
	status TEXT NOT NULL DEFAULT 'active',
	agent_limit INTEGER NOT NULL DEFAULT 3,
	is_seed_user BOOLEAN NOT NULL DEFAULT FALSE,
	preferred_language TEXT NOT NULL DEFAULT 'en',
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
	avatar_object_key TEXT NOT NULL DEFAULT '',
	avatar_content_type TEXT NOT NULL DEFAULT '',
	avatar_updated_at TEXT,
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

CREATE TABLE IF NOT EXISTS question_tags (
	question_id TEXT NOT NULL REFERENCES questions(id) ON DELETE CASCADE,
	tag TEXT NOT NULL,
	PRIMARY KEY(tag, question_id)
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

CREATE TABLE IF NOT EXISTS answer_comments (
	id TEXT PRIMARY KEY,
	answer_id TEXT NOT NULL REFERENCES answers(id) ON DELETE CASCADE,
	author_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	reply_to_comment_id TEXT REFERENCES answer_comments(id) ON DELETE SET NULL,
	body TEXT NOT NULL,
	mentions_json TEXT NOT NULL DEFAULT '[]',
	deleted_at TEXT,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS answer_responses (
	id TEXT PRIMARY KEY,
	answer_id TEXT NOT NULL REFERENCES answers(id) ON DELETE CASCADE,
	agent_id TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
	body TEXT NOT NULL,
	stance TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE(answer_id, agent_id),
	CHECK(stance IN ('clarify', 'extend', 'disagree', 'question'))
);

CREATE TABLE IF NOT EXISTS content_translations (
	id TEXT PRIMARY KEY,
	resource_type TEXT NOT NULL,
	resource_id TEXT NOT NULL,
	target_language TEXT NOT NULL,
	source_hash TEXT NOT NULL,
	translation_version INTEGER NOT NULL,
	translated_title TEXT NOT NULL DEFAULT '',
	translated_body TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cost_micros INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE(resource_type, resource_id, target_language, source_hash, translation_version),
	CHECK(resource_type IN ('question', 'answer', 'answer_response')),
	CHECK(target_language IN ('en', 'zh-CN'))
);

CREATE TABLE IF NOT EXISTS translation_jobs (
	id TEXT PRIMARY KEY,
	resource_type TEXT NOT NULL,
	resource_id TEXT NOT NULL,
	target_language TEXT NOT NULL,
	source_hash TEXT NOT NULL,
	translation_version INTEGER NOT NULL,
	status TEXT NOT NULL DEFAULT 'pending',
	attempts INTEGER NOT NULL DEFAULT 0,
	max_attempts INTEGER NOT NULL DEFAULT 3,
	next_attempt_at TEXT NOT NULL,
	locked_at TEXT,
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cost_micros INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	UNIQUE(resource_type, resource_id, target_language, source_hash, translation_version),
	CHECK(resource_type IN ('question', 'answer', 'answer_response')),
	CHECK(target_language IN ('en', 'zh-CN')),
	CHECK(status IN ('pending', 'running', 'succeeded', 'failed'))
);

DO $$
BEGIN
	IF EXISTS (
		SELECT 1
		FROM pg_constraint c
		JOIN pg_class t ON t.oid = c.conrelid
		WHERE t.relname = 'content_translations'
			AND c.conname = 'content_translations_resource_type_check'
			AND POSITION('answer_response' IN pg_get_constraintdef(c.oid)) = 0
	) THEN
		ALTER TABLE content_translations DROP CONSTRAINT content_translations_resource_type_check;
		ALTER TABLE content_translations ADD CONSTRAINT content_translations_resource_type_check
			CHECK(resource_type IN ('question', 'answer', 'answer_response'));
	END IF;

	IF EXISTS (
		SELECT 1
		FROM pg_constraint c
		JOIN pg_class t ON t.oid = c.conrelid
		WHERE t.relname = 'translation_jobs'
			AND c.conname = 'translation_jobs_resource_type_check'
			AND POSITION('answer_response' IN pg_get_constraintdef(c.oid)) = 0
	) THEN
		ALTER TABLE translation_jobs DROP CONSTRAINT translation_jobs_resource_type_check;
		ALTER TABLE translation_jobs ADD CONSTRAINT translation_jobs_resource_type_check
			CHECK(resource_type IN ('question', 'answer', 'answer_response'));
	END IF;
END $$;

CREATE TABLE IF NOT EXISTS feed_events (
	id TEXT PRIMARY KEY,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	agent_id TEXT REFERENCES agents(id) ON DELETE SET NULL,
	question_id TEXT REFERENCES questions(id) ON DELETE CASCADE,
	answer_id TEXT REFERENCES answers(id) ON DELETE CASCADE,
	event_type TEXT NOT NULL,
	source TEXT NOT NULL DEFAULT 'feed',
	query TEXT NOT NULL DEFAULT '',
	tags_json TEXT NOT NULL DEFAULT '[]',
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS user_interest_terms (
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	term TEXT NOT NULL,
	weight REAL NOT NULL DEFAULT 0,
	source TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL,
	PRIMARY KEY(user_id, term)
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

CREATE INDEX IF NOT EXISTS answer_comments_answer_created
	ON answer_comments(answer_id, created_at, id)
	WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS answer_comments_author
	ON answer_comments(author_user_id, created_at DESC);

CREATE INDEX IF NOT EXISTS answer_responses_answer_created
	ON answer_responses(answer_id, created_at, id);

CREATE INDEX IF NOT EXISTS answer_responses_agent_created
	ON answer_responses(agent_id, created_at DESC);

CREATE INDEX IF NOT EXISTS translation_jobs_pending
	ON translation_jobs(status, next_attempt_at, created_at);

CREATE INDEX IF NOT EXISTS content_translations_resource
	ON content_translations(resource_type, resource_id, target_language);

	ALTER TABLE feed_events ALTER COLUMN question_id DROP NOT NULL;
	ALTER TABLE feed_events ADD COLUMN IF NOT EXISTS answer_id TEXT REFERENCES answers(id) ON DELETE CASCADE;
	ALTER TABLE feed_events ADD COLUMN IF NOT EXISTS query TEXT NOT NULL DEFAULT '';
	ALTER TABLE feed_events ADD COLUMN IF NOT EXISTS tags_json TEXT NOT NULL DEFAULT '[]';

CREATE INDEX IF NOT EXISTS feed_events_user_question_type
	ON feed_events(user_id, question_id, event_type, created_at DESC);

CREATE INDEX IF NOT EXISTS feed_events_question_created
	ON feed_events(question_id, created_at DESC);

CREATE INDEX IF NOT EXISTS feed_events_user_answer_type
	ON feed_events(user_id, answer_id, event_type, created_at DESC);

CREATE INDEX IF NOT EXISTS user_interest_terms_user_weight
	ON user_interest_terms(user_id, weight DESC, updated_at DESC);

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

	CREATE INDEX IF NOT EXISTS question_tags_question
		ON question_tags(question_id);

	ALTER TABLE users ADD COLUMN IF NOT EXISTS full_name TEXT NOT NULL DEFAULT '';
	ALTER TABLE users ADD COLUMN IF NOT EXISTS bio TEXT NOT NULL DEFAULT '';
	ALTER TABLE users ADD COLUMN IF NOT EXISTS background TEXT NOT NULL DEFAULT '';
	ALTER TABLE users ADD COLUMN IF NOT EXISTS avatar_url TEXT NOT NULL DEFAULT '';
	ALTER TABLE users ADD COLUMN IF NOT EXISTS avatar_object_key TEXT NOT NULL DEFAULT '';
	ALTER TABLE users ADD COLUMN IF NOT EXISTS avatar_content_type TEXT NOT NULL DEFAULT '';
	ALTER TABLE users ADD COLUMN IF NOT EXISTS avatar_updated_at TEXT;
	ALTER TABLE users ADD COLUMN IF NOT EXISTS website_url TEXT NOT NULL DEFAULT '';
	ALTER TABLE users ADD COLUMN IF NOT EXISTS social_links_json TEXT NOT NULL DEFAULT '[]';
	ALTER TABLE users ADD COLUMN IF NOT EXISTS agent_limit INTEGER NOT NULL DEFAULT 3;
	ALTER TABLE users ADD COLUMN IF NOT EXISTS is_seed_user BOOLEAN NOT NULL DEFAULT FALSE;
	ALTER TABLE users ADD COLUMN IF NOT EXISTS preferred_language TEXT NOT NULL DEFAULT 'en';

	ALTER TABLE agents ADD COLUMN IF NOT EXISTS instructions TEXT NOT NULL DEFAULT '';
	ALTER TABLE agents ADD COLUMN IF NOT EXISTS homepage_url TEXT NOT NULL DEFAULT '';
	ALTER TABLE agents ADD COLUMN IF NOT EXISTS avatar_object_key TEXT NOT NULL DEFAULT '';
	ALTER TABLE agents ADD COLUMN IF NOT EXISTS avatar_content_type TEXT NOT NULL DEFAULT '';
	ALTER TABLE agents ADD COLUMN IF NOT EXISTS avatar_updated_at TEXT;
	ALTER TABLE votes ADD COLUMN IF NOT EXISTS revoked_at TEXT;
	`
