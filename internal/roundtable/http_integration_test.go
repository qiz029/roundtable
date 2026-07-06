package roundtable_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiz029/roundtable/internal/roundtable"
)

const testPassword = "correct horse battery staple 1"

type failingOnceMailer struct {
	delegate *roundtable.MemoryMailer
}

func (m *failingOnceMailer) SendVerification(ctx context.Context, email string, token string) error {
	if m.delegate == nil {
		m.delegate = roundtable.NewMemoryMailer()
		return errors.New("verification email delivery failed")
	}
	return m.delegate.SendVerification(ctx, email, token)
}

func (m *failingOnceMailer) VerificationToken(email string) (string, bool) {
	if m.delegate == nil {
		return "", false
	}
	return m.delegate.VerificationToken(email)
}

func TestUserAgentQuestionRoundTrip(t *testing.T) {
	t.Parallel()

	mailer := roundtable.NewMemoryMailer()
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
		Now: func() time.Time {
			return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
		},
		RateLimit: roundtable.RateLimitConfig{
			AgentPerSecond: 100,
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	userClient := newHTTPClient(t)

	postJSON(t, userClient, server.URL+"/api/v1/auth/register", "", map[string]any{
		"email":        "owner@example.com",
		"password":     testPassword,
		"display_name": "Owner",
	}, http.StatusCreated)

	verifyToken, ok := mailer.VerificationToken("owner@example.com")
	if !ok {
		t.Fatalf("verification token was not sent")
	}
	postJSON(t, userClient, server.URL+"/api/v1/auth/verify", "", map[string]any{
		"token": verifyToken,
	}, http.StatusOK)

	postJSON(t, userClient, server.URL+"/api/v1/auth/login", "", map[string]any{
		"email":    "owner@example.com",
		"password": testPassword,
	}, http.StatusOK)

	agentResp := postJSON(t, userClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":         "Go Reviewer",
		"description":  "Reviews Go backend designs.",
		"tags":         []string{"go", "backend"},
		"capabilities": []string{"golang", "api-review"},
		"is_public":    true,
	}, http.StatusCreated)
	agentToken := stringField(t, agentResp, "token")
	secondAgentResp := postJSON(t, userClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":         "API Critic",
		"description":  "Reviews API behavior.",
		"tags":         []string{"api"},
		"capabilities": []string{"api-review"},
		"is_public":    true,
	}, http.StatusCreated)
	secondAgentToken := stringField(t, secondAgentResp, "token")

	questionResp := postJSON(t, userClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "How should I test an agent answer platform?",
		"body":  "I want a small Go backend with real integration tests.",
		"tags":  []string{"go", "testing"},
	}, http.StatusCreated)
	questionID := stringField(t, questionResp, "id")
	if got := intField(t, questionResp, "invitation_count"); got != 2 {
		t.Fatalf("invitation_count = %d, want 2", got)
	}

	agentClient := newHTTPClient(t)
	invitations := getJSON(t, agentClient, server.URL+"/api/v1/agent/invitations", agentToken, http.StatusOK)
	items := listField(t, invitations, "items")
	if len(items) != 1 {
		t.Fatalf("invitation count = %d, want 1", len(items))
	}
	assertPagination(t, invitations, 100, 0, false, 0)
	invitation := items[0].(map[string]any)
	invitationID := stringField(t, invitation, "id")
	question := mapField(t, invitation, "question")
	if got := stringField(t, question, "id"); got != questionID {
		t.Fatalf("invitation question id = %q, want %q", got, questionID)
	}

	answerResp := postJSON(t, agentClient, server.URL+"/api/v1/agent/questions/"+questionID+"/answers", agentToken, map[string]any{
		"invitation_id": invitationID,
		"body":          "Start with API-level tests that exercise registration, invitations, answers, and voting.",
	}, http.StatusCreated)
	answerID := stringField(t, answerResp, "id")
	if !boolField(t, answerResp, "answered_via_invitation") {
		t.Fatalf("answer was not linked to invitation")
	}
	postJSON(t, agentClient, server.URL+"/api/v1/agent/questions/"+questionID+"/answers", agentToken, map[string]any{
		"body": "A duplicate answer should be rejected.",
	}, http.StatusConflict)

	detail := getJSON(t, userClient, server.URL+"/api/v1/questions/"+questionID, "", http.StatusOK)
	answers := listField(t, detail, "answers")
	if len(answers) != 1 {
		t.Fatalf("answer count = %d, want 1", len(answers))
	}
	answer := answers[0].(map[string]any)
	if got := stringField(t, answer, "id"); got != answerID {
		t.Fatalf("answer id = %q, want %q", got, answerID)
	}
	agent := mapField(t, answer, "agent")
	if got := stringField(t, agent, "owner_name"); got != "Owner" {
		t.Fatalf("answer agent owner_name = %q, want Owner", got)
	}
	if got := intField(t, answer, "like_count"); got != 0 {
		t.Fatalf("initial like_count = %d, want 0", got)
	}

	ownerLike := postJSON(t, userClient, server.URL+"/api/v1/answers/"+answerID+"/like", "", nil, http.StatusForbidden)
	if got := ownerLike["code"]; got != "forbidden" {
		t.Fatalf("owner like code = %#v, want forbidden", got)
	}
	sameOwnerAgentLike := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/answers/"+answerID+"/like", secondAgentToken, nil, http.StatusForbidden)
	if got := sameOwnerAgentLike["code"]; got != "forbidden" {
		t.Fatalf("same owner agent like code = %#v, want forbidden", got)
	}

	detail = getJSON(t, userClient, server.URL+"/api/v1/questions/"+questionID, "", http.StatusOK)
	answer = listField(t, detail, "answers")[0].(map[string]any)
	if got := intField(t, answer, "like_count"); got != 0 {
		t.Fatalf("like_count after forbidden own-network likes = %d, want 0", got)
	}

	voterClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, voterClient, server.URL, mailer, "voter@example.com", "Voter")
	voterAgentResp := postJSON(t, voterClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":        "External Reviewer",
		"description": "Reviews answers from another owner.",
		"is_public":   true,
	}, http.StatusCreated)
	voterAgentToken := stringField(t, voterAgentResp, "token")

	postJSON(t, voterClient, server.URL+"/api/v1/answers/"+answerID+"/like", "", nil, http.StatusOK)
	postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/answers/"+answerID+"/like", voterAgentToken, nil, http.StatusOK)

	detail = getJSON(t, userClient, server.URL+"/api/v1/questions/"+questionID, "", http.StatusOK)
	answer = listField(t, detail, "answers")[0].(map[string]any)
	if got := intField(t, answer, "like_count"); got != 2 {
		t.Fatalf("like_count = %d, want 2", got)
	}

	postJSON(t, agentClient, server.URL+"/api/v1/agent/answers/"+answerID+"/like", agentToken, nil, http.StatusForbidden)
}

func TestRegisterCanRetryAfterVerificationEmailFailure(t *testing.T) {
	t.Parallel()

	mailer := &failingOnceMailer{}
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
		Now: func() time.Time {
			return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	client := newHTTPClient(t)
	firstResp := postJSON(t, client, server.URL+"/api/v1/auth/register", "", map[string]any{
		"email":        "retry@example.com",
		"password":     testPassword,
		"display_name": "Retry Owner",
	}, http.StatusInternalServerError)
	if got := firstResp["code"]; got != "internal_error" {
		t.Fatalf("first registration code = %#v, want internal_error", got)
	}

	postJSON(t, client, server.URL+"/api/v1/auth/register", "", map[string]any{
		"email":        "retry@example.com",
		"password":     testPassword,
		"display_name": "Retry Owner",
	}, http.StatusCreated)
	token, ok := mailer.VerificationToken("retry@example.com")
	if !ok {
		t.Fatalf("verification token was not sent on retry")
	}
	postJSON(t, client, server.URL+"/api/v1/auth/verify", "", map[string]any{
		"token": token,
	}, http.StatusOK)
}

func TestMigrateBackfillsAdditiveColumnsOnExistingTables(t *testing.T) {
	t.Parallel()

	databaseURL := newTestDatabaseURL(t)
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(context.Background(), `
		CREATE TABLE users (
			id TEXT PRIMARY KEY,
			email TEXT NOT NULL UNIQUE,
			display_name TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			email_verified_at TEXT,
			verification_token_hash TEXT,
			status TEXT NOT NULL DEFAULT 'active',
			created_at TEXT NOT NULL
		);

		CREATE TABLE agents (
			id TEXT PRIMARY KEY,
			owner_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			tags_json TEXT NOT NULL DEFAULT '[]',
			capabilities_json TEXT NOT NULL DEFAULT '[]',
			is_public BOOLEAN NOT NULL DEFAULT TRUE,
			status TEXT NOT NULL DEFAULT 'active',
			token_hash TEXT NOT NULL UNIQUE,
			created_at TEXT NOT NULL
		);

		CREATE TABLE questions (
			id TEXT PRIMARY KEY,
			author_user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			title TEXT NOT NULL,
			body TEXT NOT NULL,
			tags_json TEXT NOT NULL DEFAULT '[]',
			created_at TEXT NOT NULL
		);

		CREATE TABLE feed_events (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			agent_id TEXT REFERENCES agents(id) ON DELETE SET NULL,
			question_id TEXT NOT NULL REFERENCES questions(id) ON DELETE CASCADE,
			event_type TEXT NOT NULL,
			source TEXT NOT NULL DEFAULT 'feed',
			created_at TEXT NOT NULL
		);
	`); err != nil {
		t.Fatalf("seed old schema: %v", err)
	}

	app, err := roundtable.NewApp(roundtable.Options{
		DatabaseURL: databaseURL,
		Mailer:      roundtable.NewMemoryMailer(),
	})
	if err != nil {
		t.Fatalf("new app with old schema: %v", err)
	}
	defer app.Close()

	assertSelectable(t, db, `SELECT full_name, bio, background, avatar_url, website_url, social_links_json FROM users LIMIT 0`)
	assertSelectable(t, db, `SELECT avatar_object_key, avatar_content_type, avatar_updated_at FROM users LIMIT 0`)
	assertSelectable(t, db, `SELECT agent_limit FROM users LIMIT 0`)
	assertSelectable(t, db, `SELECT is_seed_user FROM users LIMIT 0`)
	assertSelectable(t, db, `SELECT preferred_language FROM users LIMIT 0`)
	assertSelectable(t, db, `SELECT instructions, homepage_url FROM agents LIMIT 0`)
	assertSelectable(t, db, `SELECT avatar_object_key, avatar_content_type, avatar_updated_at FROM agents LIMIT 0`)
	assertSelectable(t, db, `SELECT answer_id, query, tags_json FROM feed_events LIMIT 0`)
	assertSelectable(t, db, `SELECT follower_user_id, followee_user_id, created_at FROM user_follows LIMIT 0`)
	assertSelectable(t, db, `SELECT revoked_at FROM votes LIMIT 0`)
	assertSelectable(t, db, `SELECT answer_id, voter_type, action, created_at FROM vote_events LIMIT 0`)
	assertSelectable(t, db, `SELECT period, status, starts_at, ends_at, frozen_at FROM score_periods LIMIT 0`)
	assertSelectable(t, db, `SELECT period, agent_id, owner_user_id, total_score, rank, details_json FROM agent_monthly_scores LIMIT 0`)
	assertSelectable(t, db, `SELECT period, user_id, total_score, rank, details_json FROM user_monthly_scores LIMIT 0`)
	assertSelectable(t, db, `SELECT answer_id, author_user_id, reply_to_comment_id, mentions_json, deleted_at FROM answer_comments LIMIT 0`)
	assertSelectable(t, db, `SELECT answer_id, agent_id, body, stance, created_at, updated_at FROM answer_responses LIMIT 0`)
	assertSelectable(t, db, `SELECT resource_type, resource_id, target_language, source_hash, translation_version FROM content_translations LIMIT 0`)
	assertSelectable(t, db, `SELECT resource_type, resource_id, target_language, source_hash, status, attempts, last_error FROM translation_jobs LIMIT 0`)
}

func TestSeedUserMarkerIsExposedAndReadOnly(t *testing.T) {
	t.Parallel()

	databaseURL := newTestDatabaseURL(t)
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer db.Close()

	mailer := roundtable.NewMemoryMailer()
	app, err := roundtable.NewApp(roundtable.Options{
		DatabaseURL: databaseURL,
		Mailer:      mailer,
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	userClient := newHTTPClient(t)
	created := postJSON(t, userClient, server.URL+"/api/v1/auth/register", "", map[string]any{
		"email":        "seed@example.com",
		"password":     testPassword,
		"display_name": "Seed User",
	}, http.StatusCreated)
	if boolField(t, created, "is_seed_user") {
		t.Fatal("registered user is_seed_user = true, want false")
	}
	token, ok := mailer.VerificationToken("seed@example.com")
	if !ok {
		t.Fatalf("verification token was not sent")
	}
	postJSON(t, userClient, server.URL+"/api/v1/auth/verify", "", map[string]any{
		"token": token,
	}, http.StatusOK)
	postJSON(t, userClient, server.URL+"/api/v1/auth/login", "", map[string]any{
		"email":    "seed@example.com",
		"password": testPassword,
	}, http.StatusOK)

	initialMe := getJSON(t, userClient, server.URL+"/api/v1/auth/me", "", http.StatusOK)
	if boolField(t, initialMe, "is_seed_user") {
		t.Fatal("initial auth/me is_seed_user = true, want false")
	}
	userID := stringField(t, initialMe, "id")

	if _, err := db.ExecContext(context.Background(), `
		UPDATE users SET is_seed_user = TRUE WHERE id = $1
	`, userID); err != nil {
		t.Fatalf("mark seed user: %v", err)
	}

	seedMe := getJSON(t, userClient, server.URL+"/api/v1/auth/me", "", http.StatusOK)
	if !boolField(t, seedMe, "is_seed_user") {
		t.Fatal("auth/me is_seed_user = false, want true")
	}
	seedPrivate := getJSON(t, userClient, server.URL+"/api/v1/me/profile", "", http.StatusOK)
	if !boolField(t, seedPrivate, "is_seed_user") {
		t.Fatal("private profile is_seed_user = false, want true")
	}
	seedPublic := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/users/"+userID+"/profile", "", http.StatusOK)
	if !boolField(t, seedPublic, "is_seed_user") {
		t.Fatal("public profile is_seed_user = false, want true")
	}

	rejected := patchJSON(t, userClient, server.URL+"/api/v1/me/profile", "", map[string]any{
		"is_seed_user": false,
	}, http.StatusBadRequest)
	if got := rejected["code"]; got != "invalid_input" {
		t.Fatalf("patch is_seed_user code = %#v, want invalid_input", got)
	}
	patched := patchJSON(t, userClient, server.URL+"/api/v1/me/profile", "", map[string]any{
		"display_name": "Seed User Renamed",
	}, http.StatusOK)
	if !boolField(t, patched, "is_seed_user") {
		t.Fatal("profile patch changed is_seed_user, want read-only true")
	}
}

func TestUserProfileGetUpdateAndPublicRead(t *testing.T) {
	t.Parallel()

	mailer := roundtable.NewMemoryMailer()
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
		Now: func() time.Time {
			return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	userClient := newHTTPClient(t)
	registerAndVerifyUser(t, userClient, server.URL, mailer, "owner@example.com")
	postJSON(t, userClient, server.URL+"/api/v1/auth/login", "", map[string]any{
		"email":    "owner@example.com",
		"password": testPassword,
	}, http.StatusOK)

	initial := getJSON(t, userClient, server.URL+"/api/v1/me/profile", "", http.StatusOK)
	userID := stringField(t, initial, "id")
	if got := stringField(t, initial, "display_name"); got != "Owner" {
		t.Fatalf("initial display_name = %q, want Owner", got)
	}
	if got := initial["email"]; got != "owner@example.com" {
		t.Fatalf("initial email = %#v, want owner@example.com", got)
	}
	if got := len(listField(t, initial, "social_links")); got != 0 {
		t.Fatalf("initial social_links count = %d, want 0", got)
	}
	if got := stringFieldAllowEmpty(t, initial, "avatar_url"); got != "" {
		t.Fatalf("initial avatar_url = %q, want empty", got)
	}
	if got := intField(t, initial, "follower_count"); got != 0 {
		t.Fatalf("initial follower_count = %d, want 0", got)
	}
	if got := intField(t, initial, "following_count"); got != 0 {
		t.Fatalf("initial following_count = %d, want 0", got)
	}
	if got := stringField(t, initial, "preferred_language"); got != "en" {
		t.Fatalf("initial preferred_language = %q, want en", got)
	}

	updated := patchJSON(t, userClient, server.URL+"/api/v1/me/profile", "", map[string]any{
		"display_name":       "Owner Renamed",
		"full_name":          "Ada Lovelace",
		"bio":                "Builds agent collaboration tools.",
		"background":         "Distributed systems and developer tooling.",
		"website_url":        "https://example.com",
		"preferred_language": "zh-CN",
		"social_links": []map[string]any{
			{"label": "GitHub", "url": "https://github.com/example"},
			{"label": "LinkedIn", "url": "https://linkedin.com/in/example"},
		},
	}, http.StatusOK)
	if got := stringField(t, updated, "display_name"); got != "Owner Renamed" {
		t.Fatalf("updated display_name = %q, want Owner Renamed", got)
	}
	if got := stringField(t, updated, "full_name"); got != "Ada Lovelace" {
		t.Fatalf("updated full_name = %q, want Ada Lovelace", got)
	}
	if got := stringField(t, updated, "background"); got != "Distributed systems and developer tooling." {
		t.Fatalf("updated background = %q", got)
	}
	if got := stringField(t, updated, "preferred_language"); got != "zh-CN" {
		t.Fatalf("updated preferred_language = %q, want zh-CN", got)
	}
	links := listField(t, updated, "social_links")
	if len(links) != 2 {
		t.Fatalf("updated social_links count = %d, want 2", len(links))
	}
	firstLink := links[0].(map[string]any)
	if got := firstLink["label"]; got != "GitHub" {
		t.Fatalf("first social link label = %#v, want GitHub", got)
	}

	me := getJSON(t, userClient, server.URL+"/api/v1/auth/me", "", http.StatusOK)
	if got := stringField(t, me, "display_name"); got != "Owner Renamed" {
		t.Fatalf("auth/me display_name = %q, want Owner Renamed", got)
	}
	if got := stringFieldAllowEmpty(t, me, "avatar_url"); got != "" {
		t.Fatalf("auth/me avatar_url = %q, want empty", got)
	}
	if got := stringField(t, me, "preferred_language"); got != "zh-CN" {
		t.Fatalf("auth/me preferred_language = %q, want zh-CN", got)
	}

	public := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/users/"+userID+"/profile", "", http.StatusOK)
	if got := stringField(t, public, "display_name"); got != "Owner Renamed" {
		t.Fatalf("public display_name = %q, want Owner Renamed", got)
	}
	if _, ok := public["email"]; ok {
		t.Fatalf("public profile leaked email: %#v", public)
	}
	if _, ok := public["preferred_language"]; ok {
		t.Fatalf("public profile leaked preferred_language: %#v", public)
	}
	if got := len(listField(t, public, "social_links")); got != 2 {
		t.Fatalf("public social_links count = %d, want 2", got)
	}
	if got := stringFieldAllowEmpty(t, public, "avatar_url"); got != "" {
		t.Fatalf("public avatar_url = %q, want empty", got)
	}

	badAvatar := patchJSON(t, userClient, server.URL+"/api/v1/me/profile", "", map[string]any{
		"avatar_url": "https://example.com/avatar.png",
	}, http.StatusBadRequest)
	if got := badAvatar["message"]; got != "avatar_url is managed by avatar upload endpoints" {
		t.Fatalf("bad avatar message = %#v", got)
	}

	badLink := patchJSON(t, userClient, server.URL+"/api/v1/me/profile", "", map[string]any{
		"social_links": []map[string]any{{"label": "GitHub"}},
	}, http.StatusBadRequest)
	if got := badLink["message"]; got != "social_links entries require label and url" {
		t.Fatalf("bad social link message = %#v", got)
	}

	badLanguage := patchJSON(t, userClient, server.URL+"/api/v1/me/profile", "", map[string]any{
		"preferred_language": "es",
	}, http.StatusBadRequest)
	if got := badLanguage["message"]; got != "preferred_language must be en or zh-CN" {
		t.Fatalf("bad preferred_language message = %#v", got)
	}

	getJSON(t, newHTTPClient(t), server.URL+"/api/v1/users/usr_missing/profile", "", http.StatusNotFound)
}

func TestUserAvatarUploadDeleteAndProfileSurfaces(t *testing.T) {
	t.Parallel()

	store, err := roundtable.NewLocalAvatarStore(t.TempDir())
	if err != nil {
		t.Fatalf("new avatar store: %v", err)
	}
	mailer := roundtable.NewMemoryMailer()
	app, err := newTestApp(t, roundtable.Options{
		Mailer:      mailer,
		AvatarStore: store,
		Now: func() time.Time {
			return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	ownerClient := newHTTPClient(t)
	ownerID := registerVerifyAndLoginUser(t, ownerClient, server.URL, mailer, "avatar-owner@example.com", "Avatar Owner")
	initial := getJSON(t, ownerClient, server.URL+"/api/v1/me/profile", "", http.StatusOK)
	if got := stringFieldAllowEmpty(t, initial, "avatar_url"); got != "" {
		t.Fatalf("initial avatar_url = %q, want empty", got)
	}

	rejected := patchJSON(t, ownerClient, server.URL+"/api/v1/me/profile", "", map[string]any{
		"avatar_url": "https://example.com/not-accepted.png",
	}, http.StatusBadRequest)
	if got := rejected["message"]; got != "avatar_url is managed by avatar upload endpoints" {
		t.Fatalf("avatar_url patch message = %#v", got)
	}

	invalid := postAvatar(t, ownerClient, server.URL+"/api/v1/me/avatar", "", []byte("<svg></svg>"), "avatar.svg", http.StatusBadRequest)
	if got := invalid["code"]; got != "invalid_input" {
		t.Fatalf("invalid avatar code = %#v, want invalid_input", got)
	}

	uploaded := postAvatar(t, ownerClient, server.URL+"/api/v1/me/avatar", "", testPNG(t, 16, 12), "avatar.png", http.StatusOK)
	avatarURL := stringField(t, uploaded, "avatar_url")
	if !strings.HasPrefix(avatarURL, "/api/v1/media/avatars/") {
		t.Fatalf("avatar_url = %q, want backend media route", avatarURL)
	}
	assertAvatarMedia(t, newHTTPClient(t), server.URL+avatarURL)

	me := getJSON(t, ownerClient, server.URL+"/api/v1/auth/me", "", http.StatusOK)
	if got := stringField(t, me, "avatar_url"); got != avatarURL {
		t.Fatalf("auth/me avatar_url = %q, want %q", got, avatarURL)
	}
	public := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/users/"+ownerID+"/profile", "", http.StatusOK)
	if got := stringField(t, public, "avatar_url"); got != avatarURL {
		t.Fatalf("public avatar_url = %q, want %q", got, avatarURL)
	}

	followerClient := newHTTPClient(t)
	followerID := registerVerifyAndLoginUser(t, followerClient, server.URL, mailer, "avatar-follower@example.com", "Avatar Follower")
	followerUpload := postAvatar(t, followerClient, server.URL+"/api/v1/me/avatar", "", testPNG(t, 10, 10), "follower.png", http.StatusOK)
	followerAvatarURL := stringField(t, followerUpload, "avatar_url")
	postJSON(t, followerClient, server.URL+"/api/v1/users/"+ownerID+"/follow", "", nil, http.StatusOK)

	followers := getJSON(t, ownerClient, server.URL+"/api/v1/users/"+ownerID+"/followers", "", http.StatusOK)
	followerItems := listField(t, followers, "items")
	if got := stringField(t, followerItems[0].(map[string]any), "id"); got != followerID {
		t.Fatalf("follower id = %q, want %q", got, followerID)
	}
	if got := stringField(t, followerItems[0].(map[string]any), "avatar_url"); got != followerAvatarURL {
		t.Fatalf("follower avatar_url = %q, want %q", got, followerAvatarURL)
	}
	following := getJSON(t, followerClient, server.URL+"/api/v1/users/"+followerID+"/following", "", http.StatusOK)
	followingItems := listField(t, following, "items")
	if got := stringField(t, followingItems[0].(map[string]any), "id"); got != ownerID {
		t.Fatalf("following id = %q, want %q", got, ownerID)
	}
	if got := stringField(t, followingItems[0].(map[string]any), "avatar_url"); got != avatarURL {
		t.Fatalf("following avatar_url = %q, want %q", got, avatarURL)
	}

	deleted := deleteJSON(t, ownerClient, server.URL+"/api/v1/me/avatar", "", http.StatusOK)
	if got := stringFieldAllowEmpty(t, deleted, "avatar_url"); got != "" {
		t.Fatalf("deleted avatar_url = %q, want empty", got)
	}
	resp := getRaw(t, newHTTPClient(t), server.URL+avatarURL)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted avatar media status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestRequestIDHeaderAndErrorPayload(t *testing.T) {
	t.Parallel()

	app, err := newTestApp(t, roundtable.Options{})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	const requestID = "rt_req_frontend_123"
	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/auth/me", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Request-Id", requestID)
	resp, err := newHTTPClient(t).Do(req)
	if err != nil {
		t.Fatalf("get auth/me: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Request-Id"); got != requestID {
		t.Fatalf("X-Request-Id = %q, want %q", got, requestID)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if got := decoded["request_id"]; got != requestID {
		t.Fatalf("request_id = %#v, want %q", got, requestID)
	}
}

func TestStructuredAccessLogIncludesRequestContext(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	mailer := roundtable.NewMemoryMailer()
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	client := newHTTPClient(t)
	created := postJSON(t, client, server.URL+"/api/v1/auth/register", "", map[string]any{
		"email":        "access-log@example.com",
		"password":     testPassword,
		"display_name": "Access Log",
	}, http.StatusCreated)
	userID := stringField(t, created, "id")
	token, ok := mailer.VerificationToken("access-log@example.com")
	if !ok {
		t.Fatalf("verification token was not sent")
	}
	postJSON(t, client, server.URL+"/api/v1/auth/verify", "", map[string]any{"token": token}, http.StatusOK)
	postJSON(t, client, server.URL+"/api/v1/auth/login", "", map[string]any{
		"email":    "access-log@example.com",
		"password": testPassword,
	}, http.StatusOK)

	const requestID = "rt_req_log_context"
	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/auth/me", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Request-Id", requestID)
	req.Header.Set("CF-Ray", "abc123-SJC")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get auth/me: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	entry := findLogEntry(t, logs.String(), requestID)
	if got := entry["msg"]; got != "http_request" {
		t.Fatalf("msg = %#v, want http_request", got)
	}
	if got := entry["method"]; got != http.MethodGet {
		t.Fatalf("method = %#v, want GET", got)
	}
	if got := entry["path"]; got != "/api/v1/auth/me" {
		t.Fatalf("path = %#v, want /api/v1/auth/me", got)
	}
	if got := entry["status"]; got != float64(http.StatusOK) {
		t.Fatalf("status = %#v, want %d", got, http.StatusOK)
	}
	if got := entry["user_id"]; got != userID {
		t.Fatalf("user_id = %#v, want %q", got, userID)
	}
	if got := entry["cf_ray"]; got != "abc123-SJC" {
		t.Fatalf("cf_ray = %#v, want abc123-SJC", got)
	}
}

func TestAvatarMediaBaseURLReturnsAbsoluteBackendMediaRoute(t *testing.T) {
	t.Parallel()

	store, err := roundtable.NewLocalAvatarStore(t.TempDir())
	if err != nil {
		t.Fatalf("new avatar store: %v", err)
	}
	mailer := roundtable.NewMemoryMailer()
	app, err := newTestApp(t, roundtable.Options{
		Mailer:             mailer,
		AvatarStore:        store,
		AvatarMediaBaseURL: "https://roundtable.example.com/",
		Now: func() time.Time {
			return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	ownerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, ownerClient, server.URL, mailer, "avatar-media-base@example.com", "Avatar Media Base")
	uploaded := postAvatar(t, ownerClient, server.URL+"/api/v1/me/avatar", "", testPNG(t, 16, 12), "avatar.png", http.StatusOK)
	avatarURL := stringField(t, uploaded, "avatar_url")
	const prefix = "https://roundtable.example.com/api/v1/media/avatars/"
	if !strings.HasPrefix(avatarURL, prefix) {
		t.Fatalf("avatar_url = %q, want absolute backend media route prefix %q", avatarURL, prefix)
	}
	mediaPath := strings.TrimPrefix(avatarURL, "https://roundtable.example.com")
	assertAvatarMedia(t, newHTTPClient(t), server.URL+mediaPath)
}

func TestUserFollowFlow(t *testing.T) {
	t.Parallel()

	mailer := roundtable.NewMemoryMailer()
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
		Now: func() time.Time {
			return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	targetClient := newHTTPClient(t)
	registerAndVerifyUser(t, targetClient, server.URL, mailer, "target@example.com")
	postJSON(t, targetClient, server.URL+"/api/v1/auth/login", "", map[string]any{
		"email":    "target@example.com",
		"password": testPassword,
	}, http.StatusOK)
	targetProfile := getJSON(t, targetClient, server.URL+"/api/v1/me/profile", "", http.StatusOK)
	targetUserID := stringField(t, targetProfile, "id")

	followerClient := newHTTPClient(t)
	registerAndVerifyUser(t, followerClient, server.URL, mailer, "follower@example.com")
	postJSON(t, followerClient, server.URL+"/api/v1/auth/login", "", map[string]any{
		"email":    "follower@example.com",
		"password": testPassword,
	}, http.StatusOK)
	followerProfile := getJSON(t, followerClient, server.URL+"/api/v1/me/profile", "", http.StatusOK)
	followerUserID := stringField(t, followerProfile, "id")

	follow := postJSON(t, followerClient, server.URL+"/api/v1/users/"+targetUserID+"/follow", "", nil, http.StatusOK)
	if !boolField(t, follow, "following") {
		t.Fatalf("follow following = false, want true")
	}
	if got := intField(t, follow, "follower_count"); got != 1 {
		t.Fatalf("follow follower_count = %d, want 1", got)
	}
	repeatedFollow := postJSON(t, followerClient, server.URL+"/api/v1/users/"+targetUserID+"/follow", "", nil, http.StatusOK)
	if got := intField(t, repeatedFollow, "follower_count"); got != 1 {
		t.Fatalf("repeated follow follower_count = %d, want 1", got)
	}

	publicTarget := getJSON(t, followerClient, server.URL+"/api/v1/users/"+targetUserID+"/profile", "", http.StatusOK)
	if got := intField(t, publicTarget, "follower_count"); got != 1 {
		t.Fatalf("public follower_count = %d, want 1", got)
	}
	if !boolField(t, publicTarget, "viewer_following") {
		t.Fatalf("viewer_following = false, want true")
	}
	followerProfile = getJSON(t, followerClient, server.URL+"/api/v1/me/profile", "", http.StatusOK)
	if got := intField(t, followerProfile, "following_count"); got != 1 {
		t.Fatalf("follower following_count = %d, want 1", got)
	}

	followers := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/users/"+targetUserID+"/followers", "", http.StatusOK)
	followerItems := listField(t, followers, "items")
	if len(followerItems) != 1 {
		t.Fatalf("followers count = %d, want 1", len(followerItems))
	}
	assertPagination(t, followers, 100, 0, false, 0)
	if got := stringField(t, followerItems[0].(map[string]any), "id"); got != followerUserID {
		t.Fatalf("followers[0].id = %q, want %q", got, followerUserID)
	}

	following := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/users/"+followerUserID+"/following", "", http.StatusOK)
	followingItems := listField(t, following, "items")
	if len(followingItems) != 1 {
		t.Fatalf("following count = %d, want 1", len(followingItems))
	}
	assertPagination(t, following, 100, 0, false, 0)
	if got := stringField(t, followingItems[0].(map[string]any), "id"); got != targetUserID {
		t.Fatalf("following[0].id = %q, want %q", got, targetUserID)
	}

	selfFollow := postJSON(t, followerClient, server.URL+"/api/v1/users/"+followerUserID+"/follow", "", nil, http.StatusBadRequest)
	if got := selfFollow["message"]; got != "cannot follow yourself" {
		t.Fatalf("self follow message = %#v, want cannot follow yourself", got)
	}
	postJSON(t, followerClient, server.URL+"/api/v1/users/usr_missing/follow", "", nil, http.StatusNotFound)

	unfollow := deleteJSON(t, followerClient, server.URL+"/api/v1/users/"+targetUserID+"/follow", "", http.StatusOK)
	if boolField(t, unfollow, "following") {
		t.Fatalf("unfollow following = true, want false")
	}
	if got := intField(t, unfollow, "follower_count"); got != 0 {
		t.Fatalf("unfollow follower_count = %d, want 0", got)
	}
	repeatedUnfollow := deleteJSON(t, followerClient, server.URL+"/api/v1/users/"+targetUserID+"/follow", "", http.StatusOK)
	if got := intField(t, repeatedUnfollow, "follower_count"); got != 0 {
		t.Fatalf("repeated unfollow follower_count = %d, want 0", got)
	}

	publicTarget = getJSON(t, followerClient, server.URL+"/api/v1/users/"+targetUserID+"/profile", "", http.StatusOK)
	if got := intField(t, publicTarget, "follower_count"); got != 0 {
		t.Fatalf("public follower_count after unfollow = %d, want 0", got)
	}
	if boolField(t, publicTarget, "viewer_following") {
		t.Fatalf("viewer_following after unfollow = true, want false")
	}

	getJSON(t, newHTTPClient(t), server.URL+"/api/v1/users/usr_missing/followers", "", http.StatusNotFound)
}

func TestOwnedAgentProfileGetAndPatch(t *testing.T) {
	t.Parallel()

	mailer := roundtable.NewMemoryMailer()
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
		Now: func() time.Time {
			return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	userClient := newHTTPClient(t)
	registerAndVerifyUser(t, userClient, server.URL, mailer, "owner@example.com")
	postJSON(t, userClient, server.URL+"/api/v1/auth/login", "", map[string]any{
		"email":    "owner@example.com",
		"password": testPassword,
	}, http.StatusOK)

	created := postJSON(t, userClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":         "Profile Agent",
		"description":  "Original description.",
		"tags":         []string{"profile"},
		"capabilities": []string{"answering"},
		"instructions": "Original private instructions.",
		"homepage_url": "https://example.com/agent",
		"is_public":    true,
	}, http.StatusCreated)
	agentID := stringField(t, created, "id")
	_ = stringField(t, created, "token")
	if got := stringField(t, created, "instructions"); got != "Original private instructions." {
		t.Fatalf("created instructions = %q", got)
	}

	detail := getJSON(t, userClient, server.URL+"/api/v1/me/agents/"+agentID, "", http.StatusOK)
	if got := stringField(t, detail, "homepage_url"); got != "https://example.com/agent" {
		t.Fatalf("agent homepage_url = %q", got)
	}

	updated := patchJSON(t, userClient, server.URL+"/api/v1/me/agents/"+agentID, "", map[string]any{
		"name":         "Updated Profile Agent",
		"description":  "Updated description.",
		"tags":         []string{"profile", "updated"},
		"capabilities": []string{"answering", "review"},
		"instructions": "Updated private instructions.",
		"homepage_url": "https://example.com/agent-updated",
		"is_public":    false,
	}, http.StatusOK)
	if got := stringField(t, updated, "name"); got != "Updated Profile Agent" {
		t.Fatalf("updated name = %q, want Updated Profile Agent", got)
	}
	if boolField(t, updated, "is_public") {
		t.Fatalf("updated is_public = true, want false")
	}
	if got := len(listField(t, updated, "tags")); got != 2 {
		t.Fatalf("updated tags count = %d, want 2", got)
	}

	list := getJSON(t, userClient, server.URL+"/api/v1/me/agents", "", http.StatusOK)
	items := listField(t, list, "items")
	if len(items) != 1 {
		t.Fatalf("agent list count = %d, want 1", len(items))
	}
	assertPagination(t, list, 100, 0, false, 0)
	listed := items[0].(map[string]any)
	if got := stringField(t, listed, "instructions"); got != "Updated private instructions." {
		t.Fatalf("listed instructions = %q", got)
	}

	resetResp := postJSON(t, userClient, server.URL+"/api/v1/me/agents/"+agentID+"/token", "", nil, http.StatusOK)
	if got := stringField(t, resetResp, "id"); got != agentID {
		t.Fatalf("reset id = %q, want %q", got, agentID)
	}

	otherClient := newHTTPClient(t)
	registerAndVerifyUser(t, otherClient, server.URL, mailer, "other@example.com")
	postJSON(t, otherClient, server.URL+"/api/v1/auth/login", "", map[string]any{
		"email":    "other@example.com",
		"password": testPassword,
	}, http.StatusOK)
	patchJSON(t, otherClient, server.URL+"/api/v1/me/agents/"+agentID, "", map[string]any{
		"name": "Hijacked",
	}, http.StatusNotFound)
}

func TestAgentAvatarUploadDeleteAndEmbeddedSurfaces(t *testing.T) {
	t.Parallel()

	store, err := roundtable.NewLocalAvatarStore(t.TempDir())
	if err != nil {
		t.Fatalf("new avatar store: %v", err)
	}
	mailer := roundtable.NewMemoryMailer()
	app, err := newTestApp(t, roundtable.Options{
		Mailer:      mailer,
		AvatarStore: store,
		Now: func() time.Time {
			return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	ownerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, ownerClient, server.URL, mailer, "agent-avatar-owner@example.com", "Agent Avatar Owner")
	agentResp := postJSON(t, ownerClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":         "Avatar Agent",
		"description":  "Avatar-owning agent.",
		"tags":         []string{"avatar"},
		"capabilities": []string{"answering"},
		"is_public":    true,
	}, http.StatusCreated)
	agentID := stringField(t, agentResp, "id")
	agentToken := stringField(t, agentResp, "token")
	if got := stringFieldAllowEmpty(t, agentResp, "avatar_url"); got != "" {
		t.Fatalf("created agent avatar_url = %q, want empty", got)
	}

	rejectedPatch := patchJSON(t, ownerClient, server.URL+"/api/v1/me/agents/"+agentID, "", map[string]any{
		"avatar_url": "https://example.com/agent.png",
	}, http.StatusBadRequest)
	if got := rejectedPatch["message"]; got != "avatar_url is managed by avatar upload endpoints" {
		t.Fatalf("agent avatar patch message = %#v", got)
	}
	rejectedCreate := postJSON(t, ownerClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":       "Rejected Avatar Agent",
		"avatar_url": "https://example.com/agent.png",
	}, http.StatusBadRequest)
	if got := rejectedCreate["message"]; got != "avatar_url is managed by avatar upload endpoints" {
		t.Fatalf("agent avatar create message = %#v", got)
	}

	uploaded := postAvatar(t, ownerClient, server.URL+"/api/v1/me/agents/"+agentID+"/avatar", "", testPNG(t, 20, 14), "agent.png", http.StatusOK)
	agentAvatarURL := stringField(t, uploaded, "avatar_url")
	if !strings.HasPrefix(agentAvatarURL, "/api/v1/media/avatars/") {
		t.Fatalf("agent avatar_url = %q, want backend media route", agentAvatarURL)
	}
	assertAvatarMedia(t, newHTTPClient(t), server.URL+agentAvatarURL)

	detail := getJSON(t, ownerClient, server.URL+"/api/v1/me/agents/"+agentID, "", http.StatusOK)
	if got := stringField(t, detail, "avatar_url"); got != agentAvatarURL {
		t.Fatalf("agent detail avatar_url = %q, want %q", got, agentAvatarURL)
	}
	list := getJSON(t, ownerClient, server.URL+"/api/v1/me/agents", "", http.StatusOK)
	listed := listField(t, list, "items")[0].(map[string]any)
	if got := stringField(t, listed, "avatar_url"); got != agentAvatarURL {
		t.Fatalf("agent list avatar_url = %q, want %q", got, agentAvatarURL)
	}
	agentProfile := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/profile", agentToken, http.StatusOK)
	if got := stringField(t, agentProfile, "avatar_url"); got != agentAvatarURL {
		t.Fatalf("agent bearer profile avatar_url = %q, want %q", got, agentAvatarURL)
	}

	otherClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, otherClient, server.URL, mailer, "agent-avatar-other@example.com", "Other Agent Owner")
	postAvatar(t, otherClient, server.URL+"/api/v1/me/agents/"+agentID+"/avatar", "", testPNG(t, 8, 8), "hijack.png", http.StatusNotFound)
	postAvatar(t, ownerClient, server.URL+"/api/v1/agents/"+agentID+"/avatar", "", testPNG(t, 8, 8), "alias.png", http.StatusNotFound)

	question := postJSON(t, ownerClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "Do embedded answer agents expose avatars?",
		"body":  "The answer payload should carry the answering agent avatar.",
		"tags":  []string{"avatar"},
	}, http.StatusCreated)
	questionID := stringField(t, question, "id")
	answer := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/questions/"+questionID+"/answers", agentToken, map[string]any{
		"body": "Yes, answer payloads carry the normalized avatar URL.",
	}, http.StatusCreated)
	answerID := stringField(t, answer, "id")

	detailWithAnswer := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/questions/"+questionID, "", http.StatusOK)
	answerItems := listField(t, detailWithAnswer, "answers")
	answerAgent := mapField(t, answerItems[0].(map[string]any), "agent")
	if got := stringField(t, answerAgent, "avatar_url"); got != agentAvatarURL {
		t.Fatalf("question detail answer avatar_url = %q, want %q", got, agentAvatarURL)
	}

	answerFeed := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/feed/answers", "", http.StatusOK)
	feedAnswer := answerFeedItem(t, answerFeed, answerID)
	feedAgent := mapField(t, feedAnswer, "agent")
	if got := stringField(t, feedAgent, "avatar_url"); got != agentAvatarURL {
		t.Fatalf("answer feed agent avatar_url = %q, want %q", got, agentAvatarURL)
	}

	secondAgentResp := postJSON(t, otherClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":         "Response Avatar Agent",
		"description":  "Responds with an avatar.",
		"tags":         []string{"avatar"},
		"capabilities": []string{"responding"},
		"is_public":    true,
	}, http.StatusCreated)
	secondAgentID := stringField(t, secondAgentResp, "id")
	secondAgentToken := stringField(t, secondAgentResp, "token")
	secondUpload := postAvatar(t, otherClient, server.URL+"/api/v1/me/agents/"+secondAgentID+"/avatar", "", testPNG(t, 18, 18), "response-agent.png", http.StatusOK)
	secondAvatarURL := stringField(t, secondUpload, "avatar_url")
	response := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/answers/"+answerID+"/responses", secondAgentToken, map[string]any{
		"body":   "The responder identity should also carry an avatar.",
		"stance": "extend",
	}, http.StatusCreated)
	responseAgent := mapField(t, response, "agent")
	if got := stringField(t, responseAgent, "avatar_url"); got != secondAvatarURL {
		t.Fatalf("response agent avatar_url = %q, want %q", got, secondAvatarURL)
	}

	deleted := deleteJSON(t, ownerClient, server.URL+"/api/v1/me/agents/"+agentID+"/avatar", "", http.StatusOK)
	if got := stringFieldAllowEmpty(t, deleted, "avatar_url"); got != "" {
		t.Fatalf("deleted agent avatar_url = %q, want empty", got)
	}
	resp := getRaw(t, newHTTPClient(t), server.URL+agentAvatarURL)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted agent avatar media status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestPublicAgentDetailAndAnswers(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	store, err := roundtable.NewLocalAvatarStore(t.TempDir())
	if err != nil {
		t.Fatalf("new avatar store: %v", err)
	}
	mailer := roundtable.NewMemoryMailer()
	app, err := newTestApp(t, roundtable.Options{
		Mailer:      mailer,
		AvatarStore: store,
		Now: func() time.Time {
			return now
		},
		RateLimit: roundtable.RateLimitConfig{
			AgentPerSecond: 100,
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	ownerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, ownerClient, server.URL, mailer, "public-agent-owner@example.com", "Public Agent Owner")
	agentResp := postJSON(t, ownerClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":         "Public Explainer",
		"description":  "Explains public answers.",
		"tags":         []string{"analysis", "public"},
		"capabilities": []string{"explain", "summarize"},
		"instructions": "Private operating instructions.",
		"homepage_url": "https://example.com/public-explainer",
		"is_public":    true,
	}, http.StatusCreated)
	agentID := stringField(t, agentResp, "id")
	agentToken := stringField(t, agentResp, "token")
	uploaded := postAvatar(t, ownerClient, server.URL+"/api/v1/me/agents/"+agentID+"/avatar", "", testPNG(t, 24, 24), "public-agent.png", http.StatusOK)
	avatarURL := stringField(t, uploaded, "avatar_url")

	detail := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/agents/"+agentID, "", http.StatusOK)
	if got := stringField(t, detail, "id"); got != agentID {
		t.Fatalf("public detail id = %q, want %q", got, agentID)
	}
	if got := stringField(t, detail, "description"); got != "Explains public answers." {
		t.Fatalf("public detail description = %q", got)
	}
	if got := stringField(t, detail, "avatar_url"); got != avatarURL {
		t.Fatalf("public detail avatar_url = %q, want %q", got, avatarURL)
	}
	if got := stringField(t, detail, "owner_name"); got != "Public Agent Owner" {
		t.Fatalf("public detail owner_name = %q", got)
	}
	if got := stringField(t, detail, "homepage_url"); got != "https://example.com/public-explainer" {
		t.Fatalf("public detail homepage_url = %q", got)
	}
	if got := boolField(t, detail, "is_public"); !got {
		t.Fatal("public detail is_public = false, want true")
	}
	if got := stringField(t, detail, "status"); got != "active" {
		t.Fatalf("public detail status = %q, want active", got)
	}
	if got := intField(t, detail, "answer_count"); got != 0 {
		t.Fatalf("public detail answer_count = %d, want 0", got)
	}
	if _, ok := detail["instructions"]; ok {
		t.Fatalf("public detail leaked instructions: %#v", detail["instructions"])
	}
	if _, ok := detail["token"]; ok {
		t.Fatalf("public detail leaked token: %#v", detail["token"])
	}
	if got := len(listField(t, detail, "tags")); got != 2 {
		t.Fatalf("public detail tags count = %d, want 2", got)
	}
	if got := len(listField(t, detail, "capabilities")); got != 2 {
		t.Fatalf("public detail capabilities count = %d, want 2", got)
	}

	privateResp := postJSON(t, ownerClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":         "Private Explainer",
		"description":  "Should not be public.",
		"instructions": "Private instructions.",
		"is_public":    false,
		"status":       "paused",
	}, http.StatusCreated)
	privateAgentID := stringField(t, privateResp, "id")
	getJSON(t, newHTTPClient(t), server.URL+"/api/v1/agents/"+privateAgentID, "", http.StatusNotFound)
	getJSON(t, newHTTPClient(t), server.URL+"/api/v1/agents/"+privateAgentID+"/answers", "", http.StatusNotFound)
	getJSON(t, newHTTPClient(t), server.URL+"/api/v1/agents/agt_missing", "", http.StatusNotFound)
	getJSON(t, newHTTPClient(t), server.URL+"/api/v1/agents/agt_missing/answers", "", http.StatusNotFound)

	now = now.Add(time.Minute)
	firstQuestion := postJSON(t, ownerClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "First public agent answer?",
		"body":  "This question should appear second in newest-first answer order.",
		"tags":  []string{"analysis"},
	}, http.StatusCreated)
	firstQuestionID := stringField(t, firstQuestion, "id")
	now = now.Add(time.Minute)
	firstAnswer := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/questions/"+firstQuestionID+"/answers", agentToken, map[string]any{
		"body": "Older public answer.",
	}, http.StatusCreated)
	firstAnswerID := stringField(t, firstAnswer, "id")

	now = now.Add(time.Minute)
	secondQuestion := postJSON(t, ownerClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "Second public agent answer?",
		"body":  "This question should appear first in newest-first answer order.",
		"tags":  []string{"public"},
	}, http.StatusCreated)
	secondQuestionID := stringField(t, secondQuestion, "id")
	now = now.Add(time.Minute)
	secondAnswer := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/questions/"+secondQuestionID+"/answers", agentToken, map[string]any{
		"body": "Newer public answer.",
	}, http.StatusCreated)
	secondAnswerID := stringField(t, secondAnswer, "id")

	detail = getJSON(t, newHTTPClient(t), server.URL+"/api/v1/agents/"+agentID, "", http.StatusOK)
	if got := intField(t, detail, "answer_count"); got != 2 {
		t.Fatalf("public detail answer_count = %d, want 2", got)
	}

	firstPage := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/agents/"+agentID+"/answers?limit=1", "", http.StatusOK)
	assertPagination(t, firstPage, 1, 0, true, 1)
	firstItems := listField(t, firstPage, "items")
	if len(firstItems) != 1 {
		t.Fatalf("first page item count = %d, want 1", len(firstItems))
	}
	firstItem := firstItems[0].(map[string]any)
	firstPageQuestion := mapField(t, firstItem, "question")
	if got := stringField(t, firstPageQuestion, "id"); got != secondQuestionID {
		t.Fatalf("newest question id = %q, want %q", got, secondQuestionID)
	}
	if got := stringField(t, firstPageQuestion, "title"); got != "Second public agent answer?" {
		t.Fatalf("newest question title = %q", got)
	}
	if got := intField(t, firstPageQuestion, "answer_count"); got != 1 {
		t.Fatalf("newest question answer_count = %d, want 1", got)
	}
	firstPageAnswer := mapField(t, firstItem, "answer")
	if got := stringField(t, firstPageAnswer, "id"); got != secondAnswerID {
		t.Fatalf("newest answer id = %q, want %q", got, secondAnswerID)
	}
	if got := stringField(t, firstPageAnswer, "body"); got != "Newer public answer." {
		t.Fatalf("newest answer body = %q", got)
	}
	if got := stringField(t, firstPageAnswer, "created_at"); got == "" {
		t.Fatal("newest answer created_at is empty")
	}
	if got := intField(t, firstPageAnswer, "like_count"); got != 0 {
		t.Fatalf("newest answer like_count = %d, want 0", got)
	}
	if got := intField(t, firstPageAnswer, "comment_count"); got != 0 {
		t.Fatalf("newest answer comment_count = %d, want 0", got)
	}
	answerAgent := mapField(t, firstPageAnswer, "agent")
	if got := stringField(t, answerAgent, "id"); got != agentID {
		t.Fatalf("answer agent id = %q, want %q", got, agentID)
	}
	if got := stringField(t, answerAgent, "avatar_url"); got != avatarURL {
		t.Fatalf("answer agent avatar_url = %q, want %q", got, avatarURL)
	}
	if got := stringField(t, answerAgent, "owner_name"); got != "Public Agent Owner" {
		t.Fatalf("answer agent owner_name = %q", got)
	}

	secondPage := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/agents/"+agentID+"/answers?limit=1&offset=1", "", http.StatusOK)
	assertPagination(t, secondPage, 1, 1, false, 0)
	secondItems := listField(t, secondPage, "items")
	if len(secondItems) != 1 {
		t.Fatalf("second page item count = %d, want 1", len(secondItems))
	}
	secondPageAnswer := mapField(t, secondItems[0].(map[string]any), "answer")
	if got := stringField(t, secondPageAnswer, "id"); got != firstAnswerID {
		t.Fatalf("older answer id = %q, want %q", got, firstAnswerID)
	}
	if got := stringField(t, secondPageAnswer, "body"); got != "Older public answer." {
		t.Fatalf("older answer body = %q", got)
	}
}

func TestAgentSelfProfilePatchAndAvatarUpload(t *testing.T) {
	t.Parallel()

	store, err := roundtable.NewLocalAvatarStore(t.TempDir())
	if err != nil {
		t.Fatalf("new avatar store: %v", err)
	}
	mailer := roundtable.NewMemoryMailer()
	app, err := newTestApp(t, roundtable.Options{
		Mailer:      mailer,
		AvatarStore: store,
		Now: func() time.Time {
			return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
		},
		RateLimit: roundtable.RateLimitConfig{
			AgentPerSecond: 100,
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	ownerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, ownerClient, server.URL, mailer, "agent-self-owner@example.com", "Agent Self Owner")
	agentResp := postJSON(t, ownerClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":         "Self Managed Agent",
		"description":  "Original self-managed description.",
		"tags":         []string{"self"},
		"capabilities": []string{"answering"},
		"instructions": "Owner-provided instructions stay owner-managed.",
		"homepage_url": "https://example.com/original-agent",
		"is_public":    true,
	}, http.StatusCreated)
	agentID := stringField(t, agentResp, "id")
	agentToken := stringField(t, agentResp, "token")

	rejectedInstructions := patchJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/profile", agentToken, map[string]any{
		"instructions": "Agent should not be able to edit instructions.",
	}, http.StatusBadRequest)
	if got := rejectedInstructions["message"]; got != "instructions is owner-managed" {
		t.Fatalf("instructions error = %#v, want owner-managed message", got)
	}
	rejectedAvatarURL := patchJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/profile", agentToken, map[string]any{
		"avatar_url": "https://example.com/avatar.png",
	}, http.StatusBadRequest)
	if got := rejectedAvatarURL["message"]; got != "avatar_url is managed by avatar upload endpoints" {
		t.Fatalf("avatar_url error = %#v, want upload endpoint message", got)
	}

	updated := patchJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/profile", agentToken, map[string]any{
		"name":         "Agent Renamed Itself",
		"description":  "Agent-maintained public description.",
		"homepage_url": "https://example.com/agent-self",
	}, http.StatusOK)
	if got := stringField(t, updated, "name"); got != "Agent Renamed Itself" {
		t.Fatalf("updated name = %q, want Agent Renamed Itself", got)
	}
	if got := stringField(t, updated, "description"); got != "Agent-maintained public description." {
		t.Fatalf("updated description = %q", got)
	}
	if got := stringField(t, updated, "homepage_url"); got != "https://example.com/agent-self" {
		t.Fatalf("updated homepage_url = %q", got)
	}
	if got := stringField(t, updated, "instructions"); got != "Owner-provided instructions stay owner-managed." {
		t.Fatalf("updated instructions = %q, want owner-managed instructions unchanged", got)
	}

	ownerView := getJSON(t, ownerClient, server.URL+"/api/v1/me/agents/"+agentID, "", http.StatusOK)
	if got := stringField(t, ownerView, "name"); got != "Agent Renamed Itself" {
		t.Fatalf("owner view name = %q, want Agent Renamed Itself", got)
	}

	uploaded := postAvatar(t, newHTTPClient(t), server.URL+"/api/v1/agent/avatar", agentToken, testPNG(t, 24, 16), "self-avatar.png", http.StatusOK)
	avatarURL := stringField(t, uploaded, "avatar_url")
	if !strings.HasPrefix(avatarURL, "/api/v1/media/avatars/") {
		t.Fatalf("agent self avatar_url = %q, want backend media route", avatarURL)
	}
	assertAvatarMedia(t, newHTTPClient(t), server.URL+avatarURL)

	ownerView = getJSON(t, ownerClient, server.URL+"/api/v1/me/agents/"+agentID, "", http.StatusOK)
	if got := stringField(t, ownerView, "avatar_url"); got != avatarURL {
		t.Fatalf("owner view avatar_url = %q, want %q", got, avatarURL)
	}

	deleted := deleteJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/avatar", agentToken, http.StatusOK)
	if got := stringFieldAllowEmpty(t, deleted, "avatar_url"); got != "" {
		t.Fatalf("deleted self avatar_url = %q, want empty", got)
	}
	resp := getRaw(t, newHTTPClient(t), server.URL+avatarURL)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted self avatar media status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestOwnedAgentActiveLimitAndPausedAgents(t *testing.T) {
	t.Parallel()

	mailer := roundtable.NewMemoryMailer()
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
		Now: func() time.Time {
			return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
		},
		RateLimit: roundtable.RateLimitConfig{
			AgentPerSecond: 100,
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	userClient := newHTTPClient(t)
	registerAndVerifyUser(t, userClient, server.URL, mailer, "agent-limit-owner@example.com")
	postJSON(t, userClient, server.URL+"/api/v1/auth/login", "", map[string]any{
		"email":    "agent-limit-owner@example.com",
		"password": testPassword,
	}, http.StatusOK)

	created := make([]map[string]any, 0, 4)
	for i := 1; i <= 3; i++ {
		created = append(created, postJSON(t, userClient, server.URL+"/api/v1/me/agents", "", map[string]any{
			"name":         "Limit Agent " + string(rune('0'+i)),
			"description":  "Participates while active.",
			"capabilities": []string{"answering"},
			"is_public":    true,
		}, http.StatusCreated))
	}

	limitResp := postJSON(t, userClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":         "Fourth Agent",
		"description":  "Should require freeing an active slot first.",
		"capabilities": []string{"answering"},
		"is_public":    true,
	}, http.StatusConflict)
	if got := limitResp["code"]; got != "agent_limit_exceeded" {
		t.Fatalf("limit error code = %#v, want agent_limit_exceeded", got)
	}

	firstAgentID := stringField(t, created[0], "id")
	firstAgentToken := stringField(t, created[0], "token")
	paused := patchJSON(t, userClient, server.URL+"/api/v1/me/agents/"+firstAgentID, "", map[string]any{
		"status": "paused",
	}, http.StatusOK)
	if got := stringField(t, paused, "status"); got != "paused" {
		t.Fatalf("paused status = %q, want paused", got)
	}

	getJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/invitations", firstAgentToken, http.StatusUnauthorized)

	fourth := postJSON(t, userClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":         "Fourth Agent",
		"description":  "Uses the freed active slot.",
		"capabilities": []string{"answering"},
		"is_public":    true,
	}, http.StatusCreated)
	created = append(created, fourth)

	list := getJSON(t, userClient, server.URL+"/api/v1/me/agents", "", http.StatusOK)
	if got := intField(t, list, "agent_limit"); got != 3 {
		t.Fatalf("agent_limit = %d, want 3", got)
	}
	if got := intField(t, list, "active_count"); got != 3 {
		t.Fatalf("active_count = %d, want 3", got)
	}

	questionResp := postJSON(t, userClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "Which agents should receive invitations?",
		"body":  "Paused agents should not receive work and should not count toward the active agent limit.",
	}, http.StatusCreated)
	if got := intField(t, questionResp, "invitation_count"); got != 3 {
		t.Fatalf("invitation_count = %d, want 3 active agents", got)
	}

	resume := patchJSON(t, userClient, server.URL+"/api/v1/me/agents/"+firstAgentID, "", map[string]any{
		"status": "active",
	}, http.StatusConflict)
	if got := resume["code"]; got != "agent_limit_exceeded" {
		t.Fatalf("resume limit error code = %#v, want agent_limit_exceeded", got)
	}
}

func TestMonthlyLeaderboardsScoreAgentAnswersCurationAndOwners(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	mailer := roundtable.NewMemoryMailer()
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
		Now: func() time.Time {
			return now
		},
		RateLimit: roundtable.RateLimitConfig{
			AgentPerSecond: 100,
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	answerOwnerClient := newHTTPClient(t)
	answerOwnerID := registerVerifyAndLoginUser(t, answerOwnerClient, server.URL, mailer, "answer-owner@example.com", "Answer Owner")
	answerAgent := postJSON(t, answerOwnerClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":         "Answer Agent",
		"description":  "Provides useful answers.",
		"capabilities": []string{"answering"},
		"is_public":    true,
	}, http.StatusCreated)
	answerAgentID := stringField(t, answerAgent, "id")
	answerAgentToken := stringField(t, answerAgent, "token")

	curatorClient := newHTTPClient(t)
	curatorUserID := registerVerifyAndLoginUser(t, curatorClient, server.URL, mailer, "curator-owner@example.com", "Curator Owner")
	curatorAgent := postJSON(t, curatorClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":         "Curator Agent",
		"description":  "Finds good answers early.",
		"capabilities": []string{"curation"},
		"is_public":    true,
	}, http.StatusCreated)
	curatorAgentID := stringField(t, curatorAgent, "id")
	curatorAgentToken := stringField(t, curatorAgent, "token")

	question := postJSON(t, curatorClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "How should monthly scoring work?",
		"body":  "We need agent answer quality and curation quality to both be visible.",
	}, http.StatusCreated)
	questionID := stringField(t, question, "id")

	answer := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/questions/"+questionID+"/answers", answerAgentToken, map[string]any{
		"body": "Use answer quality for useful responses and curation quality for early recognition of useful responses.",
	}, http.StatusCreated)
	answerID := stringField(t, answer, "id")

	now = now.Add(5 * time.Minute)
	postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/answers/"+answerID+"/like", curatorAgentToken, nil, http.StatusOK)
	now = now.Add(time.Minute)
	deleteJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/answers/"+answerID+"/like", curatorAgentToken, http.StatusOK)
	now = now.Add(time.Minute)
	postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/answers/"+answerID+"/like", curatorAgentToken, nil, http.StatusOK)
	now = now.Add(3 * time.Minute)
	postJSON(t, curatorClient, server.URL+"/api/v1/answers/"+answerID+"/like", "", nil, http.StatusOK)

	detail := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/questions/"+questionID, "", http.StatusOK)
	answers := listField(t, detail, "answers")
	if len(answers) != 1 {
		t.Fatalf("answer count = %d, want 1", len(answers))
	}
	if got := intField(t, answers[0].(map[string]any), "like_count"); got != 2 {
		t.Fatalf("like_count after unlike and relike = %d, want 2", got)
	}

	agentLeaderboard := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/leaderboards/agents?period=2026-07", "", http.StatusOK)
	answerAgentScore := leaderboardAgentScore(t, agentLeaderboard, answerAgentID)
	if got := floatField(t, answerAgentScore, "answer_score"); got <= 0 {
		t.Fatalf("answer agent answer_score = %f, want positive", got)
	}
	if got := floatField(t, answerAgentScore, "penalty_score"); got != 0 {
		t.Fatalf("answer agent penalty_score = %f, want 0", got)
	}
	curatorAgentScore := leaderboardAgentScore(t, agentLeaderboard, curatorAgentID)
	if got := floatField(t, curatorAgentScore, "curation_score"); got <= 0 {
		t.Fatalf("curator agent curation_score = %f, want positive", got)
	}
	if got := floatField(t, curatorAgentScore, "penalty_score"); got != 0 {
		t.Fatalf("curator agent penalty_score = %f, want 0", got)
	}
	if got := floatField(t, curatorAgentScore, "answer_score"); got != 0 {
		t.Fatalf("curator agent answer_score = %f, want 0", got)
	}
	agentLeaderboardPage := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/leaderboards/agents?period=2026-07&limit=1", "", http.StatusOK)
	if got := len(listField(t, agentLeaderboardPage, "items")); got != 1 {
		t.Fatalf("agent leaderboard page count = %d, want 1", got)
	}
	assertPagination(t, agentLeaderboardPage, 1, 0, true, 1)
	agentLeaderboardSecondPage := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/leaderboards/agents?period=2026-07&limit=1&offset=1", "", http.StatusOK)
	if got := len(listField(t, agentLeaderboardSecondPage, "items")); got != 1 {
		t.Fatalf("agent leaderboard second page count = %d, want 1", got)
	}
	assertPagination(t, agentLeaderboardSecondPage, 1, 1, false, 0)

	userLeaderboard := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/leaderboards/users?period=2026-07", "", http.StatusOK)
	answerOwnerScore := leaderboardUserScore(t, userLeaderboard, answerOwnerID)
	if got := floatField(t, answerOwnerScore, "owned_agent_score"); got <= 0 {
		t.Fatalf("answer owner owned_agent_score = %f, want positive", got)
	}
	if got := floatField(t, answerOwnerScore, "penalty_score"); got != 0 {
		t.Fatalf("answer owner penalty_score = %f, want 0", got)
	}
	curatorOwnerScore := leaderboardUserScore(t, userLeaderboard, curatorUserID)
	if got := floatField(t, curatorOwnerScore, "owned_agent_score"); got <= 0 {
		t.Fatalf("curator owner owned_agent_score = %f, want positive", got)
	}
	if got := floatField(t, curatorOwnerScore, "penalty_score"); got != 0 {
		t.Fatalf("curator owner penalty_score = %f, want 0", got)
	}
	userLeaderboardPage := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/leaderboards/users?period=2026-07&limit=1", "", http.StatusOK)
	if got := len(listField(t, userLeaderboardPage, "items")); got != 1 {
		t.Fatalf("user leaderboard page count = %d, want 1", got)
	}
	assertPagination(t, userLeaderboardPage, 1, 0, true, 1)
	userLeaderboardSecondPage := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/leaderboards/users?period=2026-07&limit=1&offset=1", "", http.StatusOK)
	if got := len(listField(t, userLeaderboardSecondPage, "items")); got != 1 {
		t.Fatalf("user leaderboard second page count = %d, want 1", got)
	}
	assertPagination(t, userLeaderboardSecondPage, 1, 1, false, 0)

	agentScore := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/agents/"+answerAgentID+"/scores?period=2026-07", "", http.StatusOK)
	if got := floatField(t, agentScore, "answer_score"); got <= 0 {
		t.Fatalf("agent score answer_score = %f, want positive", got)
	}
	userScore := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/users/"+answerOwnerID+"/scores?period=2026-07", "", http.StatusOK)
	if got := floatField(t, userScore, "total_score"); got <= 0 {
		t.Fatalf("user score total_score = %f, want positive", got)
	}
	rewards := getJSON(t, answerOwnerClient, server.URL+"/api/v1/me/rewards?period=2026-07", "", http.StatusOK)
	rewardScore := mapField(t, rewards, "score")
	if got := floatField(t, rewardScore, "total_score"); got <= 0 {
		t.Fatalf("reward total_score = %f, want positive", got)
	}
}

func TestQuestionSearchMatchesTitleAndBody(t *testing.T) {
	t.Parallel()

	mailer := roundtable.NewMemoryMailer()
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
		Now: func() time.Time {
			return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	userClient := newHTTPClient(t)
	registerAndVerifyUser(t, userClient, server.URL, mailer, "owner@example.com")
	postJSON(t, userClient, server.URL+"/api/v1/auth/login", "", map[string]any{
		"email":    "owner@example.com",
		"password": testPassword,
	}, http.StatusOK)

	titleMatch := postJSON(t, userClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "Mercury release planning",
		"body":  "How should we stage a backend release?",
		"tags":  []string{"release"},
	}, http.StatusCreated)
	bodyMatch := postJSON(t, userClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "Search implementation",
		"body":  "Can the question index find mercury when it appears in the description?",
		"tags":  []string{"search"},
	}, http.StatusCreated)
	postJSON(t, userClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "Lunch menu",
		"body":  "This should not match the query.",
		"tags":  []string{"misc"},
	}, http.StatusCreated)

	found := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/questions?q=mercury", "", http.StatusOK)
	items := listField(t, found, "items")
	if len(items) != 2 {
		t.Fatalf("search result count = %d, want 2", len(items))
	}
	foundIDs := map[string]bool{}
	for _, item := range items {
		foundIDs[stringField(t, item.(map[string]any), "id")] = true
	}
	for _, wantID := range []string{stringField(t, titleMatch, "id"), stringField(t, bodyMatch, "id")} {
		if !foundIDs[wantID] {
			t.Fatalf("search result ids = %#v, missing %s", foundIDs, wantID)
		}
	}

	noMatch := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/questions?q=banana", "", http.StatusOK)
	if got := len(listField(t, noMatch, "items")); got != 0 {
		t.Fatalf("no-match result count = %d, want 0", got)
	}
}

func TestQuestionListFiltersByTags(t *testing.T) {
	t.Parallel()

	mailer := roundtable.NewMemoryMailer()
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
		Now: func() time.Time {
			return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	userClient := newHTTPClient(t)
	registerAndVerifyUser(t, userClient, server.URL, mailer, "owner@example.com")
	postJSON(t, userClient, server.URL+"/api/v1/auth/login", "", map[string]any{
		"email":    "owner@example.com",
		"password": testPassword,
	}, http.StatusOK)

	tagMatch := postJSON(t, userClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "Mercury rollout",
		"body":  "How should we stage this deployment?",
		"tags":  []string{"Backend", "release"},
	}, http.StatusCreated)
	textMatch := postJSON(t, userClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "Backend roadmap",
		"body":  "This question should only match full text search.",
		"tags":  []string{"product"},
	}, http.StatusCreated)
	postJSON(t, userClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "Frontend polish",
		"body":  "This question should not match topic filters.",
		"tags":  []string{"backend-tools"},
	}, http.StatusCreated)

	byQ := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/questions?q=backend", "", http.StatusOK)
	assertQuestionIDs(t, byQ, []string{stringField(t, textMatch, "id")})

	byTag := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/questions?tags=backend", "", http.StatusOK)
	assertQuestionIDs(t, byTag, []string{stringField(t, tagMatch, "id")})

	byHashtag := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/questions?tags=%23backend", "", http.StatusOK)
	assertQuestionIDs(t, byHashtag, []string{stringField(t, tagMatch, "id")})

	intersection := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/questions?q=mercury&tags=backend", "", http.StatusOK)
	assertQuestionIDs(t, intersection, []string{stringField(t, tagMatch, "id")})

	noIntersection := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/questions?q=roadmap&tags=backend", "", http.StatusOK)
	assertQuestionIDs(t, noIntersection, []string{})

	multiTag := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/questions?tags=backend&tags=release", "", http.StatusOK)
	assertQuestionIDs(t, multiTag, []string{stringField(t, tagMatch, "id")})
}

func TestQuestionListPagination(t *testing.T) {
	t.Parallel()

	mailer := roundtable.NewMemoryMailer()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
		Now: func() time.Time {
			return now
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	userClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, userClient, server.URL, mailer, "pagination-owner@example.com", "Pagination Owner")
	agentResp := postJSON(t, userClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":        "Pagination Agent",
		"description": "Browses paginated question lists.",
		"is_public":   true,
	}, http.StatusCreated)
	agentToken := stringField(t, agentResp, "token")

	first := postJSON(t, userClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "First pagination question",
		"body":  "This should appear on the second page.",
	}, http.StatusCreated)
	now = now.Add(time.Minute)
	second := postJSON(t, userClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "Second pagination question",
		"body":  "This should appear on the first page.",
	}, http.StatusCreated)
	now = now.Add(time.Minute)
	third := postJSON(t, userClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "Third pagination question",
		"body":  "This should appear first.",
	}, http.StatusCreated)

	firstPage := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/questions?limit=2", "", http.StatusOK)
	firstItems := listField(t, firstPage, "items")
	if len(firstItems) != 2 {
		t.Fatalf("first page question count = %d, want 2", len(firstItems))
	}
	if got := stringField(t, firstItems[0].(map[string]any), "id"); got != stringField(t, third, "id") {
		t.Fatalf("first page first id = %q, want latest question", got)
	}
	if got := stringField(t, firstItems[1].(map[string]any), "id"); got != stringField(t, second, "id") {
		t.Fatalf("first page second id = %q, want second question", got)
	}
	assertPagination(t, firstPage, 2, 0, true, 2)

	agentFirstPage := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/questions?limit=2", agentToken, http.StatusOK)
	if got := len(listField(t, agentFirstPage, "items")); got != 2 {
		t.Fatalf("agent first page question count = %d, want 2", got)
	}
	assertPagination(t, agentFirstPage, 2, 0, true, 2)

	secondPage := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/questions?limit=2&offset=2", "", http.StatusOK)
	secondItems := listField(t, secondPage, "items")
	if len(secondItems) != 1 {
		t.Fatalf("second page question count = %d, want 1", len(secondItems))
	}
	if got := stringField(t, secondItems[0].(map[string]any), "id"); got != stringField(t, first, "id") {
		t.Fatalf("second page id = %q, want first question", got)
	}
	assertPagination(t, secondPage, 2, 2, false, 0)

	invalid := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/questions?limit=101", "", http.StatusBadRequest)
	if got := invalid["code"]; got != "invalid_input" {
		t.Fatalf("invalid limit code = %#v, want invalid_input", got)
	}
}

func TestFeedPersonalizesQuestionsWithPagination(t *testing.T) {
	t.Parallel()

	mailer := roundtable.NewMemoryMailer()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
		Now: func() time.Time {
			return now
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	viewerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, viewerClient, server.URL, mailer, "feed-viewer@example.com", "Feed Viewer")
	postJSON(t, viewerClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":         "ML Review Agent",
		"description":  "Reviews model evaluation questions.",
		"tags":         []string{"ml"},
		"capabilities": []string{"evaluation"},
		"is_public":    true,
	}, http.StatusCreated)

	askerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, askerClient, server.URL, mailer, "feed-asker@example.com", "Feed Asker")
	matched := postJSON(t, askerClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "How should we compare ML evaluation results?",
		"body":  "We need a review of model benchmark tradeoffs.",
		"tags":  []string{"ml"},
	}, http.StatusCreated)
	now = now.Add(time.Minute)
	recent := postJSON(t, askerClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "How should we organize office snacks?",
		"body":  "This is newer but unrelated to the viewer agent.",
		"tags":  []string{"office"},
	}, http.StatusCreated)

	anonymousFeed := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/feed?limit=1", "", http.StatusOK)
	anonymousItems := listField(t, anonymousFeed, "items")
	if got := stringField(t, anonymousItems[0].(map[string]any), "id"); got != stringField(t, recent, "id") {
		t.Fatalf("anonymous feed first id = %q, want newest question", got)
	}
	assertPagination(t, anonymousFeed, 1, 0, true, 1)

	personalizedFeed := getJSON(t, viewerClient, server.URL+"/api/v1/feed?limit=1", "", http.StatusOK)
	personalizedItems := listField(t, personalizedFeed, "items")
	if got := stringField(t, personalizedItems[0].(map[string]any), "id"); got != stringField(t, matched, "id") {
		t.Fatalf("personalized feed first id = %q, want agent-tag match", got)
	}
	assertPagination(t, personalizedFeed, 1, 0, true, 1)

	secondPage := getJSON(t, viewerClient, server.URL+"/api/v1/feed?limit=1&offset=1", "", http.StatusOK)
	secondItems := listField(t, secondPage, "items")
	if got := stringField(t, secondItems[0].(map[string]any), "id"); got != stringField(t, recent, "id") {
		t.Fatalf("personalized feed second id = %q, want recent unrelated question", got)
	}
	assertPagination(t, secondPage, 1, 1, false, 0)
}

func TestFeedEventsDemoteViewedQuestions(t *testing.T) {
	t.Parallel()

	mailer := roundtable.NewMemoryMailer()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
		Now: func() time.Time {
			return now
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	viewerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, viewerClient, server.URL, mailer, "feed-history-viewer@example.com", "Feed History Viewer")
	postJSON(t, viewerClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":      "Robotics Agent",
		"tags":      []string{"robotics"},
		"is_public": true,
	}, http.StatusCreated)

	askerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, askerClient, server.URL, mailer, "feed-history-asker@example.com", "Feed History Asker")
	matched := postJSON(t, askerClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "How should robotics planners compare routes?",
		"body":  "Robotics route planning needs a careful answer.",
		"tags":  []string{"robotics"},
	}, http.StatusCreated)
	now = now.Add(time.Minute)
	recent := postJSON(t, askerClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "What lunch format should we use?",
		"body":  "A newer but unrelated question.",
		"tags":  []string{"office"},
	}, http.StatusCreated)

	before := getJSON(t, viewerClient, server.URL+"/api/v1/feed?limit=1", "", http.StatusOK)
	beforeItems := listField(t, before, "items")
	if got := stringField(t, beforeItems[0].(map[string]any), "id"); got != stringField(t, matched, "id") {
		t.Fatalf("feed first id before event = %q, want matched question", got)
	}

	event := postJSON(t, viewerClient, server.URL+"/api/v1/feed/events", "", map[string]any{
		"question_id": stringField(t, matched, "id"),
		"event_type":  "open",
		"source":      "feed",
	}, http.StatusCreated)
	if got := stringField(t, event, "question_id"); got != stringField(t, matched, "id") {
		t.Fatalf("event question_id = %q, want %q", got, stringField(t, matched, "id"))
	}
	if got := stringField(t, event, "event_type"); got != "open" {
		t.Fatalf("event_type = %q, want open", got)
	}

	after := getJSON(t, viewerClient, server.URL+"/api/v1/feed?limit=1", "", http.StatusOK)
	afterItems := listField(t, after, "items")
	if got := stringField(t, afterItems[0].(map[string]any), "id"); got != stringField(t, recent, "id") {
		t.Fatalf("feed first id after event = %q, want unviewed recent question", got)
	}
	assertPagination(t, after, 1, 0, true, 1)

	assertLoginRequired(t, postRawJSON(t, newHTTPClient(t), server.URL+"/api/v1/feed/events", map[string]any{
		"question_id": stringField(t, recent, "id"),
		"event_type":  "open",
	}), "login required to record feed events")
}

func TestFeedInterestEventsPromoteMatchingQuestions(t *testing.T) {
	t.Parallel()

	mailer := roundtable.NewMemoryMailer()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
		Now: func() time.Time {
			return now
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	viewerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, viewerClient, server.URL, mailer, "interest-viewer@example.com", "Interest Viewer")

	askerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, askerClient, server.URL, mailer, "interest-asker@example.com", "Interest Asker")
	matching := postJSON(t, askerClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "How should backend queues be tuned?",
		"body":  "Queue workers need careful throughput planning.",
		"tags":  []string{"backend"},
	}, http.StatusCreated)
	now = now.Add(time.Minute)
	recent := postJSON(t, askerClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "What should lunch include?",
		"body":  "A newer but unrelated question.",
		"tags":  []string{"office"},
	}, http.StatusCreated)

	before := getJSON(t, viewerClient, server.URL+"/api/v1/feed?limit=1", "", http.StatusOK)
	beforeItems := listField(t, before, "items")
	if got := stringField(t, beforeItems[0].(map[string]any), "id"); got != stringField(t, recent, "id") {
		t.Fatalf("feed first id before interest = %q, want recent question", got)
	}

	searchEvent := postJSON(t, viewerClient, server.URL+"/api/v1/feed/events", "", map[string]any{
		"event_type": "search",
		"source":     "search",
		"query":      "backend queues",
	}, http.StatusCreated)
	if got := stringField(t, searchEvent, "event_type"); got != "search" {
		t.Fatalf("search event_type = %q, want search", got)
	}
	if got := stringField(t, searchEvent, "query"); got != "backend queues" {
		t.Fatalf("search event query = %q, want backend queues", got)
	}
	postJSON(t, viewerClient, server.URL+"/api/v1/feed/events", "", map[string]any{
		"event_type": "tag_filter",
		"source":     "search",
		"tags":       []string{"Backend"},
	}, http.StatusCreated)

	after := getJSON(t, viewerClient, server.URL+"/api/v1/feed?limit=1", "", http.StatusOK)
	afterItems := listField(t, after, "items")
	afterFirst := afterItems[0].(map[string]any)
	if got := stringField(t, afterFirst, "id"); got != stringField(t, matching, "id") {
		t.Fatalf("feed first id after interest = %q, want matching question", got)
	}
	reasons := listField(t, afterFirst, "feed_reasons")
	if !containsStringValue(reasons, "matched_interest_tags") {
		t.Fatalf("feed reasons = %#v, want matched_interest_tags", reasons)
	}
}

func TestAnswerFeedRanksHotAnswersWithPagination(t *testing.T) {
	t.Parallel()

	mailer := roundtable.NewMemoryMailer()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
		Now: func() time.Time {
			return now
		},
		RateLimit: roundtable.RateLimitConfig{
			AgentPerSecond: 100,
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	askerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, askerClient, server.URL, mailer, "answer-feed-asker@example.com", "Answer Feed Asker")

	hotOwnerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, hotOwnerClient, server.URL, mailer, "answer-feed-hot-owner@example.com", "Hot Owner")
	hotAgent := postJSON(t, hotOwnerClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":        "Careful Answerer",
		"description": "Writes strong answers.",
		"tags":        []string{"backend"},
		"is_public":   true,
	}, http.StatusCreated)
	hotAgentToken := stringField(t, hotAgent, "token")

	recentOwnerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, recentOwnerClient, server.URL, mailer, "answer-feed-recent-owner@example.com", "Recent Owner")
	recentAgent := postJSON(t, recentOwnerClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":        "Recent Answerer",
		"description": "Writes newer answers.",
		"tags":        []string{"office"},
		"is_public":   true,
	}, http.StatusCreated)
	recentAgentToken := stringField(t, recentAgent, "token")

	hotQuestion := postJSON(t, askerClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "How should we design a backend feed?",
		"body":  "I need answer ranking that avoids browser N+1 requests.",
		"tags":  []string{"backend"},
	}, http.StatusCreated)
	hotQuestionID := stringField(t, hotQuestion, "id")
	hotAnswer := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/questions/"+hotQuestionID+"/answers", hotAgentToken, map[string]any{
		"body": "Rank answers directly with a hotness score and return the card payload in one response.",
	}, http.StatusCreated)
	hotAnswerID := stringField(t, hotAnswer, "id")

	now = now.Add(time.Minute)
	recentQuestion := postJSON(t, askerClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "What should the office lunch plan be?",
		"body":  "This question is newer but has a less useful answer.",
		"tags":  []string{"office"},
	}, http.StatusCreated)
	recentQuestionID := stringField(t, recentQuestion, "id")
	recentAnswer := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/questions/"+recentQuestionID+"/answers", recentAgentToken, map[string]any{
		"body": "Order sandwiches.",
	}, http.StatusCreated)
	recentAnswerID := stringField(t, recentAnswer, "id")

	now = now.Add(time.Minute)
	unansweredQuestion := postJSON(t, askerClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "This unanswered question should not enter answer feed",
		"body":  "Answer feed should only render existing answers.",
		"tags":  []string{"backend"},
	}, http.StatusCreated)

	voterClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, voterClient, server.URL, mailer, "answer-feed-voter@example.com", "Answer Feed Voter")
	postJSON(t, voterClient, server.URL+"/api/v1/answers/"+hotAnswerID+"/like", "", nil, http.StatusOK)

	feed := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/feed/answers?limit=10", "", http.StatusOK)
	items := listField(t, feed, "items")
	if len(items) != 2 {
		t.Fatalf("answer feed item count = %d, want 2, items = %#v", len(items), items)
	}
	first := items[0].(map[string]any)
	firstQuestion := mapField(t, first, "question")
	if got := stringField(t, firstQuestion, "id"); got != hotQuestionID {
		t.Fatalf("first answer feed question id = %q, want hot question %q", got, hotQuestionID)
	}
	if got := stringField(t, firstQuestion, "title"); got != "How should we design a backend feed?" {
		t.Fatalf("first answer feed question title = %q", got)
	}
	firstAnswer := mapField(t, first, "answer")
	if got := stringField(t, firstAnswer, "id"); got != hotAnswerID {
		t.Fatalf("first answer feed answer id = %q, want hot answer %q", got, hotAnswerID)
	}
	if got := intField(t, firstAnswer, "like_count"); got != 1 {
		t.Fatalf("first answer like_count = %d, want 1", got)
	}
	firstAgent := mapField(t, firstAnswer, "agent")
	if got := stringField(t, firstAgent, "name"); got != "Careful Answerer" {
		t.Fatalf("first answer agent name = %q", got)
	}
	if got := stringField(t, firstAgent, "owner_name"); got != "Hot Owner" {
		t.Fatalf("first answer agent owner_name = %q", got)
	}
	reasons := listField(t, first, "rank_reasons")
	if !containsStringValue(reasons, "liked_answer") {
		t.Fatalf("rank_reasons = %#v, want liked_answer", reasons)
	}

	second := items[1].(map[string]any)
	secondAnswer := mapField(t, second, "answer")
	if got := stringField(t, secondAnswer, "id"); got != recentAnswerID {
		t.Fatalf("second answer feed answer id = %q, want recent answer %q", got, recentAnswerID)
	}
	secondQuestion := mapField(t, second, "question")
	if got := stringField(t, secondQuestion, "id"); got == stringField(t, unansweredQuestion, "id") {
		t.Fatalf("unanswered question appeared in answer feed: %q", got)
	}
	assertPagination(t, feed, 10, 0, false, 0)

	firstPage := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/feed/answers?limit=1", "", http.StatusOK)
	assertPagination(t, firstPage, 1, 0, true, 1)
	secondPage := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/feed/answers?limit=1&offset=1", "", http.StatusOK)
	assertPagination(t, secondPage, 1, 1, false, 0)

	detail := getJSON(t, askerClient, server.URL+"/api/v1/questions/"+hotQuestionID+"?limit=20", "", http.StatusOK)
	answers := listField(t, detail, "answers")
	for _, raw := range answers {
		answer := raw.(map[string]any)
		if stringField(t, answer, "id") == hotAnswerID {
			return
		}
	}
	t.Fatalf("hot answer %s was not present in question detail first page", hotAnswerID)
}

func TestAnswerFeedEventsAcceptAnswerID(t *testing.T) {
	t.Parallel()

	mailer := roundtable.NewMemoryMailer()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
		Now: func() time.Time {
			return now
		},
		RateLimit: roundtable.RateLimitConfig{
			AgentPerSecond: 100,
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	askerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, askerClient, server.URL, mailer, "answer-event-asker@example.com", "Answer Event Asker")

	firstOwnerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, firstOwnerClient, server.URL, mailer, "answer-event-first-owner@example.com", "First Event Owner")
	firstAgent := postJSON(t, firstOwnerClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":      "First Event Agent",
		"is_public": true,
	}, http.StatusCreated)
	firstAgentToken := stringField(t, firstAgent, "token")

	secondOwnerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, secondOwnerClient, server.URL, mailer, "answer-event-second-owner@example.com", "Second Event Owner")
	secondAgent := postJSON(t, secondOwnerClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":      "Second Event Agent",
		"is_public": true,
	}, http.StatusCreated)
	secondAgentToken := stringField(t, secondAgent, "token")

	firstQuestion := postJSON(t, askerClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "How should answer feed telemetry work?",
		"body":  "Open events should be tied to the rendered answer card.",
	}, http.StatusCreated)
	firstQuestionID := stringField(t, firstQuestion, "id")
	firstAnswer := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/questions/"+firstQuestionID+"/answers", firstAgentToken, map[string]any{
		"body": "Telemetry should accept answer_id and infer the owning question.",
	}, http.StatusCreated)
	firstAnswerID := stringField(t, firstAnswer, "id")

	now = now.Add(time.Minute)
	secondQuestion := postJSON(t, askerClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "Which answer should show after the first card was opened?",
		"body":  "A comparable answer should move above an opened card.",
	}, http.StatusCreated)
	secondQuestionID := stringField(t, secondQuestion, "id")
	secondAnswer := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/questions/"+secondQuestionID+"/answers", secondAgentToken, map[string]any{
		"body": "Use answer-specific open events to demote just that card.",
	}, http.StatusCreated)
	secondAnswerID := stringField(t, secondAnswer, "id")

	viewerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, viewerClient, server.URL, mailer, "answer-event-viewer@example.com", "Answer Event Viewer")
	postJSON(t, viewerClient, server.URL+"/api/v1/answers/"+firstAnswerID+"/like", "", nil, http.StatusOK)

	before := getJSON(t, viewerClient, server.URL+"/api/v1/feed/answers?limit=1", "", http.StatusOK)
	beforeItems := listField(t, before, "items")
	beforeAnswer := mapField(t, beforeItems[0].(map[string]any), "answer")
	if got := stringField(t, beforeAnswer, "id"); got != firstAnswerID {
		t.Fatalf("first answer before event = %q, want liked answer %q", got, firstAnswerID)
	}

	event := postJSON(t, viewerClient, server.URL+"/api/v1/feed/events", "", map[string]any{
		"answer_id":  firstAnswerID,
		"event_type": "open",
		"source":     "answer_feed",
	}, http.StatusCreated)
	if got := stringField(t, event, "question_id"); got != firstQuestionID {
		t.Fatalf("event inferred question_id = %q, want %q", got, firstQuestionID)
	}
	if got := stringField(t, event, "answer_id"); got != firstAnswerID {
		t.Fatalf("event answer_id = %q, want %q", got, firstAnswerID)
	}
	if got := stringField(t, event, "source"); got != "answer_feed" {
		t.Fatalf("event source = %q, want answer_feed", got)
	}

	after := getJSON(t, viewerClient, server.URL+"/api/v1/feed/answers?limit=1", "", http.StatusOK)
	afterItems := listField(t, after, "items")
	afterAnswer := mapField(t, afterItems[0].(map[string]any), "answer")
	if got := stringField(t, afterAnswer, "id"); got != secondAnswerID {
		t.Fatalf("first answer after event = %q, want unopened answer %q", got, secondAnswerID)
	}
}

func TestAnswerCommentsCreateReplyListDeleteAndCounts(t *testing.T) {
	t.Parallel()

	mailer := roundtable.NewMemoryMailer()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
		Now: func() time.Time {
			return now
		},
		RateLimit: roundtable.RateLimitConfig{
			AgentPerSecond: 100,
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	askerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, askerClient, server.URL, mailer, "comment-asker@example.com", "Comment Asker")

	firstOwnerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, firstOwnerClient, server.URL, mailer, "comment-agent-one@example.com", "Comment Agent One Owner")
	firstAgent := postJSON(t, firstOwnerClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":      "Comment Agent One",
		"is_public": true,
	}, http.StatusCreated)
	firstAgentToken := stringField(t, firstAgent, "token")

	secondOwnerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, secondOwnerClient, server.URL, mailer, "comment-agent-two@example.com", "Comment Agent Two Owner")
	secondAgent := postJSON(t, secondOwnerClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":      "Comment Agent Two",
		"is_public": true,
	}, http.StatusCreated)
	secondAgentToken := stringField(t, secondAgent, "token")

	question := postJSON(t, askerClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "How should answer comments work?",
		"body":  "Each answer should have a flat comment thread.",
	}, http.StatusCreated)
	questionID := stringField(t, question, "id")
	firstAnswer := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/questions/"+questionID+"/answers", firstAgentToken, map[string]any{
		"body": "Use a flat comment list below each answer.",
	}, http.StatusCreated)
	firstAnswerID := stringField(t, firstAnswer, "id")
	secondAnswer := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/questions/"+questionID+"/answers", secondAgentToken, map[string]any{
		"body": "Do not build nested reply trees for the MVP.",
	}, http.StatusCreated)
	secondAnswerID := stringField(t, secondAnswer, "id")

	empty := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/answers/"+firstAnswerID+"/comments", "", http.StatusOK)
	if got := len(listField(t, empty, "items")); got != 0 {
		t.Fatalf("initial comments count = %d, want 0", got)
	}

	anonymousClient := newHTTPClient(t)
	assertLoginRequired(t, postRawJSON(t, anonymousClient, server.URL+"/api/v1/answers/"+firstAnswerID+"/comments", map[string]any{
		"body": "Anonymous comment",
	}), "login required to comment on answers")

	commenterClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, commenterClient, server.URL, mailer, "commenter@example.com", "Commenter")
	firstComment := postJSON(t, commenterClient, server.URL+"/api/v1/answers/"+firstAnswerID+"/comments", "", map[string]any{
		"body": "  This adds useful context.  ",
	}, http.StatusCreated)
	firstCommentID := stringField(t, firstComment, "id")
	if got := stringField(t, firstComment, "body"); got != "This adds useful context." {
		t.Fatalf("first comment body = %q", got)
	}
	if got := firstComment["reply_to_comment_id"]; got != nil {
		t.Fatalf("first reply_to_comment_id = %#v, want nil", got)
	}
	firstAuthor := mapField(t, firstComment, "author")
	if got := stringField(t, firstAuthor, "display_name"); got != "Commenter" {
		t.Fatalf("first author display_name = %q", got)
	}

	tooLong := postJSON(t, commenterClient, server.URL+"/api/v1/answers/"+firstAnswerID+"/comments", "", map[string]any{
		"body": strings.Repeat("x", 2001),
	}, http.StatusBadRequest)
	if got := tooLong["code"]; got != "invalid_input" {
		t.Fatalf("too-long comment code = %#v", got)
	}

	now = now.Add(time.Minute)
	replyClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, replyClient, server.URL, mailer, "reply-commenter@example.com", "Reply Commenter")
	replyComment := postJSON(t, replyClient, server.URL+"/api/v1/answers/"+firstAnswerID+"/comments", "", map[string]any{
		"body":                "@Commenter agreed.",
		"reply_to_comment_id": firstCommentID,
	}, http.StatusCreated)
	replyCommentID := stringField(t, replyComment, "id")
	if got := stringField(t, replyComment, "reply_to_comment_id"); got != firstCommentID {
		t.Fatalf("reply_to_comment_id = %q, want %q", got, firstCommentID)
	}

	now = now.Add(time.Minute)
	otherAnswerComment := postJSON(t, commenterClient, server.URL+"/api/v1/answers/"+secondAnswerID+"/comments", "", map[string]any{
		"body": "This belongs to the second answer.",
	}, http.StatusCreated)
	otherAnswerCommentID := stringField(t, otherAnswerComment, "id")
	crossReply := postJSON(t, commenterClient, server.URL+"/api/v1/answers/"+firstAnswerID+"/comments", "", map[string]any{
		"body":                "This should be rejected.",
		"reply_to_comment_id": otherAnswerCommentID,
	}, http.StatusBadRequest)
	if got := crossReply["code"]; got != "invalid_input" {
		t.Fatalf("cross-answer reply code = %#v", got)
	}

	firstPage := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/answers/"+firstAnswerID+"/comments?limit=1", "", http.StatusOK)
	firstPageItems := listField(t, firstPage, "items")
	if got := stringField(t, firstPageItems[0].(map[string]any), "id"); got != firstCommentID {
		t.Fatalf("first comments page first id = %q, want %q", got, firstCommentID)
	}
	assertPagination(t, firstPage, 1, 0, true, 1)
	secondPage := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/answers/"+firstAnswerID+"/comments?limit=1&offset=1", "", http.StatusOK)
	secondPageItems := listField(t, secondPage, "items")
	if got := stringField(t, secondPageItems[0].(map[string]any), "id"); got != replyCommentID {
		t.Fatalf("second comments page first id = %q, want %q", got, replyCommentID)
	}
	assertPagination(t, secondPage, 1, 1, false, 0)

	assertAnswerCommentCount(t, getJSON(t, askerClient, server.URL+"/api/v1/questions/"+questionID, "", http.StatusOK), firstAnswerID, 2)
	assertAnswerFeedCommentCount(t, getJSON(t, newHTTPClient(t), server.URL+"/api/v1/feed/answers?limit=10", "", http.StatusOK), firstAnswerID, 2)

	otherUserClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, otherUserClient, server.URL, mailer, "other-commenter@example.com", "Other Commenter")
	forbiddenDelete := deleteJSON(t, otherUserClient, server.URL+"/api/v1/comments/"+firstCommentID, "", http.StatusForbidden)
	if got := forbiddenDelete["code"]; got != "forbidden" {
		t.Fatalf("forbidden delete code = %#v", got)
	}

	deleteResp := deleteJSON(t, commenterClient, server.URL+"/api/v1/comments/"+firstCommentID, "", http.StatusOK)
	if got := stringField(t, deleteResp, "comment_id"); got != firstCommentID {
		t.Fatalf("deleted comment id = %q, want %q", got, firstCommentID)
	}
	if got := deleteResp["deleted"]; got != true {
		t.Fatalf("deleted flag = %#v, want true", got)
	}

	afterDelete := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/answers/"+firstAnswerID+"/comments", "", http.StatusOK)
	afterDeleteItems := listField(t, afterDelete, "items")
	if len(afterDeleteItems) != 1 {
		t.Fatalf("comments after delete count = %d, want 1", len(afterDeleteItems))
	}
	if got := stringField(t, afterDeleteItems[0].(map[string]any), "id"); got != replyCommentID {
		t.Fatalf("remaining comment id = %q, want reply %q", got, replyCommentID)
	}
	deletedReply := postJSON(t, replyClient, server.URL+"/api/v1/answers/"+firstAnswerID+"/comments", "", map[string]any{
		"body":                "Cannot reply to a deleted comment.",
		"reply_to_comment_id": firstCommentID,
	}, http.StatusBadRequest)
	if got := deletedReply["code"]; got != "invalid_input" {
		t.Fatalf("reply to deleted comment code = %#v", got)
	}

	assertAnswerCommentCount(t, getJSON(t, askerClient, server.URL+"/api/v1/questions/"+questionID, "", http.StatusOK), firstAnswerID, 1)
	assertAnswerFeedCommentCount(t, getJSON(t, newHTTPClient(t), server.URL+"/api/v1/feed/answers?limit=10", "", http.StatusOK), firstAnswerID, 1)

	getJSON(t, newHTTPClient(t), server.URL+"/api/v1/answers/ans_missing/comments", "", http.StatusNotFound)
	deleteJSON(t, commenterClient, server.URL+"/api/v1/comments/cmt_missing", "", http.StatusNotFound)
}

func TestAnswerResponsesCreateUpdateListAndGuardrails(t *testing.T) {
	t.Parallel()

	mailer := roundtable.NewMemoryMailer()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
		Now: func() time.Time {
			return now
		},
		RateLimit: roundtable.RateLimitConfig{
			AgentPerSecond: 100,
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	askerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, askerClient, server.URL, mailer, "response-asker@example.com", "Response Asker")

	firstOwnerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, firstOwnerClient, server.URL, mailer, "response-agent-one@example.com", "Response Agent One Owner")
	firstAgent := postJSON(t, firstOwnerClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":      "Response Agent One",
		"is_public": true,
	}, http.StatusCreated)
	firstAgentToken := stringField(t, firstAgent, "token")
	firstOwnerSecondAgent := postJSON(t, firstOwnerClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":      "Same Owner Response Agent",
		"is_public": true,
	}, http.StatusCreated)
	firstOwnerSecondAgentToken := stringField(t, firstOwnerSecondAgent, "token")

	secondOwnerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, secondOwnerClient, server.URL, mailer, "response-agent-two@example.com", "Response Agent Two Owner")
	secondAgent := postJSON(t, secondOwnerClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":      "Response Agent Two",
		"is_public": true,
	}, http.StatusCreated)
	secondAgentToken := stringField(t, secondAgent, "token")

	thirdOwnerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, thirdOwnerClient, server.URL, mailer, "response-agent-three@example.com", "Response Agent Three Owner")
	thirdAgent := postJSON(t, thirdOwnerClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":      "Response Agent Three",
		"is_public": true,
	}, http.StatusCreated)
	thirdAgentToken := stringField(t, thirdAgent, "token")

	question := postJSON(t, askerClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "How should answer responses work?",
		"body":  "Agents should add bounded counterpoints without creating an infinite thread.",
	}, http.StatusCreated)
	questionID := stringField(t, question, "id")
	firstAnswer := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/questions/"+questionID+"/answers", firstAgentToken, map[string]any{
		"body": "Responses should be annotations, not conversation turns.",
	}, http.StatusCreated)
	firstAnswerID := stringField(t, firstAnswer, "id")

	empty := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/answers/"+firstAnswerID+"/responses", "", http.StatusOK)
	if got := len(listField(t, empty, "items")); got != 0 {
		t.Fatalf("initial responses count = %d, want 0", got)
	}

	self := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/answers/"+firstAnswerID+"/responses", firstAgentToken, map[string]any{
		"body":   "I should not respond to myself.",
		"stance": "extend",
	}, http.StatusForbidden)
	if got := self["code"]; got != "forbidden" {
		t.Fatalf("self response code = %#v, want forbidden", got)
	}
	sameOwner := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/answers/"+firstAnswerID+"/responses", firstOwnerSecondAgentToken, map[string]any{
		"body":   "Same owner agents should not manufacture agreement.",
		"stance": "extend",
	}, http.StatusForbidden)
	if got := sameOwner["code"]; got != "forbidden" {
		t.Fatalf("same-owner response code = %#v, want forbidden", got)
	}
	badStance := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/answers/"+firstAnswerID+"/responses", secondAgentToken, map[string]any{
		"body":   "This stance is unsupported.",
		"stance": "debate",
	}, http.StatusBadRequest)
	if got := badStance["code"]; got != "invalid_input" {
		t.Fatalf("bad stance code = %#v, want invalid_input", got)
	}

	firstResponse := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/answers/"+firstAnswerID+"/responses", secondAgentToken, map[string]any{
		"body":   "  This should stay bounded and factual.  ",
		"stance": "disagree",
	}, http.StatusCreated)
	firstResponseID := stringField(t, firstResponse, "id")
	if got := stringField(t, firstResponse, "body"); got != "This should stay bounded and factual." {
		t.Fatalf("response body = %q", got)
	}
	if got := stringField(t, firstResponse, "stance"); got != "disagree" {
		t.Fatalf("response stance = %q", got)
	}
	firstResponseAgent := mapField(t, firstResponse, "agent")
	if got := stringField(t, firstResponseAgent, "name"); got != "Response Agent Two" {
		t.Fatalf("response agent name = %q", got)
	}

	duplicate := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/answers/"+firstAnswerID+"/responses", secondAgentToken, map[string]any{
		"body":   "A second response should not be allowed.",
		"stance": "extend",
	}, http.StatusConflict)
	if got := duplicate["code"]; got != "conflict" {
		t.Fatalf("duplicate response code = %#v, want conflict", got)
	}

	now = now.Add(time.Minute)
	secondResponse := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/answers/"+firstAnswerID+"/responses", thirdAgentToken, map[string]any{
		"body":   "Another agent can add a separate bounded response.",
		"stance": "question",
	}, http.StatusCreated)
	secondResponseID := stringField(t, secondResponse, "id")

	forbiddenUpdate := patchJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/responses/"+firstResponseID, thirdAgentToken, map[string]any{
		"body": "Only the responding agent can update.",
	}, http.StatusForbidden)
	if got := forbiddenUpdate["code"]; got != "forbidden" {
		t.Fatalf("forbidden update code = %#v, want forbidden", got)
	}
	emptyUpdate := patchJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/responses/"+firstResponseID, secondAgentToken, map[string]any{}, http.StatusBadRequest)
	if got := emptyUpdate["code"]; got != "invalid_input" {
		t.Fatalf("empty update code = %#v, want invalid_input", got)
	}

	now = now.Add(time.Minute)
	updated := patchJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/responses/"+firstResponseID, secondAgentToken, map[string]any{
		"body":   "Updated bounded counterpoint.",
		"stance": "clarify",
	}, http.StatusOK)
	if got := stringField(t, updated, "body"); got != "Updated bounded counterpoint." {
		t.Fatalf("updated response body = %q", got)
	}
	if got := stringField(t, updated, "stance"); got != "clarify" {
		t.Fatalf("updated response stance = %q", got)
	}
	if stringField(t, updated, "created_at") == stringField(t, updated, "updated_at") {
		t.Fatalf("updated_at did not change after update")
	}

	firstPage := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/answers/"+firstAnswerID+"/responses?limit=1", "", http.StatusOK)
	firstPageItems := listField(t, firstPage, "items")
	if got := stringField(t, firstPageItems[0].(map[string]any), "id"); got != firstResponseID {
		t.Fatalf("first responses page first id = %q, want %q", got, firstResponseID)
	}
	assertPagination(t, firstPage, 1, 0, true, 1)
	secondPage := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/answers/"+firstAnswerID+"/responses?limit=1&offset=1", "", http.StatusOK)
	secondPageItems := listField(t, secondPage, "items")
	if got := stringField(t, secondPageItems[0].(map[string]any), "id"); got != secondResponseID {
		t.Fatalf("second responses page first id = %q, want %q", got, secondResponseID)
	}
	assertPagination(t, secondPage, 1, 1, false, 0)

	getJSON(t, newHTTPClient(t), server.URL+"/api/v1/answers/ans_missing/responses", "", http.StatusNotFound)
	postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/answers/ans_missing/responses", secondAgentToken, map[string]any{
		"body":   "Missing answer.",
		"stance": "extend",
	}, http.StatusNotFound)
	patchJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/responses/rsp_missing", secondAgentToken, map[string]any{
		"body": "Missing response.",
	}, http.StatusNotFound)
}

func TestAnswerPagination(t *testing.T) {
	t.Parallel()

	mailer := roundtable.NewMemoryMailer()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
		Now: func() time.Time {
			return now
		},
		RateLimit: roundtable.RateLimitConfig{
			AgentPerSecond: 100,
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	userClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, userClient, server.URL, mailer, "answer-pagination-owner@example.com", "Answer Pagination Owner")
	agentTokens := []string{}
	for _, name := range []string{"Answer Pager One", "Answer Pager Two", "Answer Pager Three"} {
		agent := postJSON(t, userClient, server.URL+"/api/v1/me/agents", "", map[string]any{
			"name":        name,
			"description": "Answers pagination test questions.",
			"is_public":   true,
		}, http.StatusCreated)
		agentTokens = append(agentTokens, stringField(t, agent, "token"))
	}

	question := postJSON(t, userClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "How should answers be paginated?",
		"body":  "Return the first page by default and let clients request more.",
	}, http.StatusCreated)
	questionID := stringField(t, question, "id")

	for _, token := range agentTokens {
		now = now.Add(time.Minute)
		postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/questions/"+questionID+"/answers", token, map[string]any{
			"body": "Paginated answer body.",
		}, http.StatusCreated)
	}

	detail := getJSON(t, userClient, server.URL+"/api/v1/questions/"+questionID+"?limit=2", "", http.StatusOK)
	answers := listField(t, detail, "answers")
	if len(answers) != 2 {
		t.Fatalf("detail answer count = %d, want 2", len(answers))
	}
	assertNestedPagination(t, detail, "answers_pagination", 2, 0, true, 2)

	agentAnswers := getJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/questions/"+questionID+"/answers?limit=2&offset=2", agentTokens[0], http.StatusOK)
	answerItems := listField(t, agentAnswers, "items")
	if len(answerItems) != 1 {
		t.Fatalf("agent answer page count = %d, want 1", len(answerItems))
	}
	assertPagination(t, agentAnswers, 2, 2, false, 0)
}

func TestTranslationCacheAPIAndWorker(t *testing.T) {
	t.Parallel()

	provider := &fakeTranslationProvider{}
	mailer := roundtable.NewMemoryMailer()
	app, err := newTestApp(t, roundtable.Options{
		Mailer:              mailer,
		TranslationProvider: provider,
		TranslationWorker: roundtable.TranslationWorkerConfig{
			BatchSize: 20,
		},
		RateLimit: roundtable.RateLimitConfig{
			AgentPerSecond: 100,
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	userClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, userClient, server.URL, mailer, "translation-owner@example.com", "Translation Owner")
	agentResp := postJSON(t, userClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":        "Translation Agent",
		"description": "Answers translation test questions.",
		"is_public":   true,
	}, http.StatusCreated)
	agentToken := stringField(t, agentResp, "token")

	question := postJSON(t, userClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "How should translation caching work?",
		"body":  "Translate the public question without blocking creation.",
	}, http.StatusCreated)
	questionID := stringField(t, question, "id")
	answer := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/questions/"+questionID+"/answers", agentToken, map[string]any{
		"body": "Translate this public answer too.",
	}, http.StatusCreated)
	answerID := stringField(t, answer, "id")

	anonymousMiss := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/translations", "", map[string]any{
		"resource_type":   "question",
		"resource_id":     questionID,
		"target_language": "zh-CN",
	}, http.StatusNotFound)
	if got := anonymousMiss["message"]; got != "translation not found" {
		t.Fatalf("anonymous miss message = %#v", got)
	}

	pending := postJSON(t, userClient, server.URL+"/api/v1/translations", "", map[string]any{
		"resource_type":   "question",
		"resource_id":     questionID,
		"target_language": "zh-CN",
	}, http.StatusAccepted)
	if got := stringField(t, pending, "status"); got != "pending" {
		t.Fatalf("pending status = %q", got)
	}
	if got := stringField(t, pending, "target_language"); got != "zh-CN" {
		t.Fatalf("pending target_language = %q", got)
	}

	processed, err := app.ProcessTranslationJobs(context.Background(), 20)
	if err != nil {
		t.Fatalf("process translations: %v", err)
	}
	if processed != 2 {
		t.Fatalf("processed jobs = %d, want 2", processed)
	}
	if got := provider.CallCount(); got != 2 {
		t.Fatalf("provider call count = %d, want 2", got)
	}

	ready := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/translations", "", map[string]any{
		"resource_type":   "question",
		"resource_id":     questionID,
		"target_language": "zh-CN",
	}, http.StatusOK)
	if got := stringField(t, ready, "status"); got != "ready" {
		t.Fatalf("ready status = %q", got)
	}
	if got := stringField(t, ready, "source_language"); got != "en" {
		t.Fatalf("ready source_language = %q, want en", got)
	}
	if got := stringField(t, ready, "target_language"); got != "zh-CN" {
		t.Fatalf("ready target_language = %q, want zh-CN", got)
	}
	translation := mapField(t, ready, "translation")
	if got := stringField(t, translation, "title"); got != "[zh-CN] How should translation caching work?" {
		t.Fatalf("translated title = %q", got)
	}
	if got := stringField(t, translation, "body"); got != "[zh-CN] Translate the public question without blocking creation." {
		t.Fatalf("translated body = %q", got)
	}
	if got := stringField(t, ready, "provider"); got != "fake" {
		t.Fatalf("provider = %q, want fake", got)
	}

	answerReady := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/translations", "", map[string]any{
		"resource_type":   "answer",
		"resource_id":     answerID,
		"target_language": "en",
	}, http.StatusOK)
	answerTranslation := mapField(t, answerReady, "translation")
	if got := stringFieldAllowEmpty(t, answerTranslation, "title"); got != "" {
		t.Fatalf("answer translated title = %q, want empty", got)
	}
	if got := stringField(t, answerReady, "source_language"); got != "en" {
		t.Fatalf("answer source_language = %q, want en", got)
	}
	if got := stringField(t, answerTranslation, "body"); got != "Translate this public answer too." {
		t.Fatalf("answer translated body = %q", got)
	}

	processedAgain, err := app.ProcessTranslationJobs(context.Background(), 20)
	if err != nil {
		t.Fatalf("process translations again: %v", err)
	}
	if processedAgain != 0 {
		t.Fatalf("processed duplicate jobs = %d, want 0", processedAgain)
	}
	if got := provider.CallCount(); got != 2 {
		t.Fatalf("provider call count after cache hit = %d, want 2", got)
	}

	chineseQuestion := postJSON(t, userClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "如何缓存翻译？",
		"body":  "中文内容请求中文翻译时应该直接返回原文。",
	}, http.StatusCreated)
	chineseQuestionID := stringField(t, chineseQuestion, "id")
	original := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/translations", "", map[string]any{
		"resource_type":   "question",
		"resource_id":     chineseQuestionID,
		"target_language": "zh-CN",
	}, http.StatusOK)
	if got := stringField(t, original, "source_language"); got != "zh-CN" {
		t.Fatalf("original source_language = %q, want zh-CN", got)
	}
	originalTranslation := mapField(t, original, "translation")
	if got := stringField(t, originalTranslation, "title"); got != "如何缓存翻译？" {
		t.Fatalf("original title = %q", got)
	}
	if got := provider.CallCount(); got != 2 {
		t.Fatalf("provider call count after original response = %d, want 2", got)
	}
}

func TestTranslationWorkerFailureAndBudgetDoNotBreakReads(t *testing.T) {
	t.Parallel()

	failingProvider := &fakeTranslationProvider{err: errors.New("provider down")}
	mailer := roundtable.NewMemoryMailer()
	app, err := newTestApp(t, roundtable.Options{
		Mailer:              mailer,
		TranslationProvider: failingProvider,
		TranslationWorker: roundtable.TranslationWorkerConfig{
			BatchSize:      10,
			MaxAttempts:    1,
			RetryBaseDelay: time.Second,
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()
	server := httptest.NewServer(app.Handler())
	defer server.Close()

	userClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, userClient, server.URL, mailer, "translation-failure-owner@example.com", "Translation Failure Owner")
	question := postJSON(t, userClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "Will failed translation break reads?",
		"body":  "Normal question reads should continue.",
	}, http.StatusCreated)
	questionID := stringField(t, question, "id")

	processed, err := app.ProcessTranslationJobs(context.Background(), 10)
	if err != nil {
		t.Fatalf("process failing translations: %v", err)
	}
	if processed != 1 {
		t.Fatalf("processed failing jobs = %d, want 1", processed)
	}
	getJSON(t, newHTTPClient(t), server.URL+"/api/v1/questions/"+questionID, "", http.StatusOK)
	postJSON(t, newHTTPClient(t), server.URL+"/api/v1/translations", "", map[string]any{
		"resource_type":   "question",
		"resource_id":     questionID,
		"target_language": "zh-CN",
	}, http.StatusNotFound)

	budgetProvider := &fakeTranslationProvider{}
	budgetMailer := roundtable.NewMemoryMailer()
	budgetApp, err := newTestApp(t, roundtable.Options{
		Mailer:              budgetMailer,
		TranslationProvider: budgetProvider,
		TranslationWorker: roundtable.TranslationWorkerConfig{
			BatchSize:           10,
			DailyBudgetMicros:   1,
			EstimatedCostMicros: 2,
		},
	})
	if err != nil {
		t.Fatalf("new budget app: %v", err)
	}
	defer budgetApp.Close()
	budgetServer := httptest.NewServer(budgetApp.Handler())
	defer budgetServer.Close()

	budgetClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, budgetClient, budgetServer.URL, budgetMailer, "translation-budget-owner@example.com", "Translation Budget Owner")
	budgetQuestion := postJSON(t, budgetClient, budgetServer.URL+"/api/v1/questions", "", map[string]any{
		"title": "Will budget block provider calls?",
		"body":  "Budget exhaustion should keep reads healthy.",
	}, http.StatusCreated)
	budgetQuestionID := stringField(t, budgetQuestion, "id")

	budgetProcessed, err := budgetApp.ProcessTranslationJobs(context.Background(), 10)
	if err != nil {
		t.Fatalf("process budget translations: %v", err)
	}
	if budgetProcessed != 1 {
		t.Fatalf("budget processed jobs = %d, want 1 deferred job", budgetProcessed)
	}
	if got := budgetProvider.CallCount(); got != 0 {
		t.Fatalf("budget provider call count = %d, want 0", got)
	}
	getJSON(t, newHTTPClient(t), budgetServer.URL+"/api/v1/questions/"+budgetQuestionID, "", http.StatusOK)
}

func TestQuestionSearchBackfillsExistingQuestions(t *testing.T) {
	t.Parallel()

	databaseURL := newTestDatabaseURL(t)
	mailer := roundtable.NewMemoryMailer()
	app, err := roundtable.NewApp(roundtable.Options{
		DatabaseURL: databaseURL,
		Mailer:      mailer,
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	server := httptest.NewServer(app.Handler())

	userClient := newHTTPClient(t)
	registerAndVerifyUser(t, userClient, server.URL, mailer, "owner@example.com")
	postJSON(t, userClient, server.URL+"/api/v1/auth/login", "", map[string]any{
		"email":    "owner@example.com",
		"password": testPassword,
	}, http.StatusOK)
	question := postJSON(t, userClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "Backfill searchable question",
		"body":  "Existing rows should be indexed on startup.",
		"tags":  []string{"search"},
	}, http.StatusCreated)
	questionID := stringField(t, question, "id")
	server.Close()
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `DELETE FROM question_search_terms`); err != nil {
		t.Fatalf("clear search terms: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `DELETE FROM question_tags`); err != nil {
		t.Fatalf("clear question tags: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if err := app.Close(); err != nil {
		t.Fatalf("close app: %v", err)
	}

	reopened, err := roundtable.NewApp(roundtable.Options{
		DatabaseURL: databaseURL,
		Mailer:      roundtable.NewMemoryMailer(),
	})
	if err != nil {
		t.Fatalf("reopen app: %v", err)
	}
	defer reopened.Close()
	reopenedServer := httptest.NewServer(reopened.Handler())
	defer reopenedServer.Close()

	found := getJSON(t, newHTTPClient(t), reopenedServer.URL+"/api/v1/questions?q=backfill", "", http.StatusOK)
	assertQuestionIDs(t, found, []string{questionID})

	foundByTag := getJSON(t, newHTTPClient(t), reopenedServer.URL+"/api/v1/questions?tags=search", "", http.StatusOK)
	assertQuestionIDs(t, foundByTag, []string{questionID})
}

func TestQuestionInvitesAtMostFiveAgents(t *testing.T) {
	t.Parallel()

	mailer := roundtable.NewMemoryMailer()
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
		Now: func() time.Time {
			return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	askerClient := newHTTPClient(t)
	registerVerifyAndLoginUser(t, askerClient, server.URL, mailer, "asker@example.com", "Asker")

	agentIndex := 0
	for ownerIndex := 0; ownerIndex < 2; ownerIndex++ {
		ownerClient := newHTTPClient(t)
		email := "invitation-owner-" + string(rune('a'+ownerIndex)) + "@example.com"
		registerVerifyAndLoginUser(t, ownerClient, server.URL, mailer, email, "Invitation Owner")
		for i := 0; i < 3; i++ {
			postJSON(t, ownerClient, server.URL+"/api/v1/me/agents", "", map[string]any{
				"name":         "Agent " + string(rune('A'+agentIndex)),
				"description":  "Participates in random invitations.",
				"tags":         []string{"random"},
				"capabilities": []string{"answering"},
				"is_public":    true,
			}, http.StatusCreated)
			agentIndex++
		}
	}

	questionResp := postJSON(t, askerClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "How many agents should be invited?",
		"body":  "The platform should cap random invitations at five.",
		"tags":  []string{"random"},
	}, http.StatusCreated)
	if got := intField(t, questionResp, "invitation_count"); got != 5 {
		t.Fatalf("invitation_count = %d, want 5", got)
	}
}

func TestAgentTokenResetAndInvitationExpiry(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	mailer := roundtable.NewMemoryMailer()
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
		Now: func() time.Time {
			return now
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	userClient := newHTTPClient(t)
	registerAndVerifyUser(t, userClient, server.URL, mailer, "owner@example.com")
	postJSON(t, userClient, server.URL+"/api/v1/auth/login", "", map[string]any{
		"email":    "owner@example.com",
		"password": testPassword,
	}, http.StatusOK)

	agentResp := postJSON(t, userClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":         "Expiry Agent",
		"description":  "Tests expiry behavior.",
		"tags":         []string{"expiry"},
		"capabilities": []string{"testing"},
		"is_public":    true,
	}, http.StatusCreated)
	agentID := stringField(t, agentResp, "id")
	oldAgentToken := stringField(t, agentResp, "token")

	resetResp := postJSON(t, userClient, server.URL+"/api/v1/me/agents/"+agentID+"/token", "", nil, http.StatusOK)
	newAgentToken := stringField(t, resetResp, "token")
	if newAgentToken == oldAgentToken {
		t.Fatalf("reset returned the same token")
	}
	getJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/invitations", oldAgentToken, http.StatusUnauthorized)

	questionResp := postJSON(t, userClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "Should expired invitations disappear?",
		"body":  "Expired invitations should not block free exploration answers.",
		"tags":  []string{"expiry"},
	}, http.StatusCreated)
	questionID := stringField(t, questionResp, "id")

	agentClient := newHTTPClient(t)
	invitations := getJSON(t, agentClient, server.URL+"/api/v1/agent/invitations", newAgentToken, http.StatusOK)
	items := listField(t, invitations, "items")
	if len(items) != 1 {
		t.Fatalf("invitation count = %d, want 1", len(items))
	}
	invitationID := stringField(t, items[0].(map[string]any), "id")

	now = now.Add(25 * time.Hour)
	expiredInvitations := getJSON(t, agentClient, server.URL+"/api/v1/agent/invitations", newAgentToken, http.StatusOK)
	if got := len(listField(t, expiredInvitations, "items")); got != 0 {
		t.Fatalf("expired invitation count = %d, want 0", got)
	}

	answerResp := postJSON(t, agentClient, server.URL+"/api/v1/agent/questions/"+questionID+"/answers", newAgentToken, map[string]any{
		"invitation_id": invitationID,
		"body":          "The invitation expired, but the agent can still answer through exploration.",
	}, http.StatusCreated)
	if boolField(t, answerResp, "answered_via_invitation") {
		t.Fatalf("expired invitation was marked answered")
	}
}

func TestUnverifiedUserCannotCreateAgent(t *testing.T) {
	t.Parallel()

	mailer := roundtable.NewMemoryMailer()
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
		Now: func() time.Time {
			return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	userClient := newHTTPClient(t)
	postJSON(t, userClient, server.URL+"/api/v1/auth/register", "", map[string]any{
		"email":        "unverified@example.com",
		"password":     testPassword,
		"display_name": "Unverified",
	}, http.StatusCreated)
	postJSON(t, userClient, server.URL+"/api/v1/auth/login", "", map[string]any{
		"email":    "unverified@example.com",
		"password": testPassword,
	}, http.StatusOK)
	postJSON(t, userClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":         "Blocked Agent",
		"description":  "Should not be created.",
		"tags":         []string{"blocked"},
		"capabilities": []string{"none"},
		"is_public":    true,
	}, http.StatusForbidden)
}

func TestRegisterPasswordPolicy(t *testing.T) {
	t.Parallel()

	mailer := roundtable.NewMemoryMailer()
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	wantMessage := "password must be at least 9 characters and include at least one letter and one number"
	tests := []struct {
		name     string
		email    string
		password string
	}{
		{
			name:     "too short",
			email:    "short@example.com",
			password: "abc12345",
		},
		{
			name:     "missing number",
			email:    "nonumber@example.com",
			password: "correct horse battery",
		},
		{
			name:     "missing letter",
			email:    "noletter@example.com",
			password: "123456789",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/auth/register", "", map[string]any{
				"email":        tt.email,
				"password":     tt.password,
				"display_name": "Blocked",
			}, http.StatusBadRequest)
			if got := resp["code"]; got != "invalid_input" {
				t.Fatalf("code = %#v, want invalid_input", got)
			}
			if got := resp["message"]; got != wantMessage {
				t.Fatalf("message = %#v, want %q", got, wantMessage)
			}
		})
	}

	postJSON(t, newHTTPClient(t), server.URL+"/api/v1/auth/register", "", map[string]any{
		"email":        "valid@example.com",
		"password":     "abc123456",
		"display_name": "Valid",
	}, http.StatusCreated)
}

func TestAnonymousUserCanOnlyReadQuestionsAndAnswers(t *testing.T) {
	t.Parallel()

	mailer := roundtable.NewMemoryMailer()
	app, err := newTestApp(t, roundtable.Options{
		Mailer: mailer,
		Now: func() time.Time {
			return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	server := httptest.NewServer(app.Handler())
	defer server.Close()

	userClient := newHTTPClient(t)
	registerAndVerifyUser(t, userClient, server.URL, mailer, "owner@example.com")
	postJSON(t, userClient, server.URL+"/api/v1/auth/login", "", map[string]any{
		"email":    "owner@example.com",
		"password": testPassword,
	}, http.StatusOK)
	agentResp := postJSON(t, userClient, server.URL+"/api/v1/me/agents", "", map[string]any{
		"name":         "Public Answer Agent",
		"description":  "Creates an answer for anonymous read testing.",
		"tags":         []string{"public"},
		"capabilities": []string{"answering"},
		"is_public":    true,
	}, http.StatusCreated)
	agentToken := stringField(t, agentResp, "token")
	questionResp := postJSON(t, userClient, server.URL+"/api/v1/questions", "", map[string]any{
		"title": "Can anonymous visitors read questions?",
		"body":  "Anonymous visitors should only be able to read questions and answers.",
		"tags":  []string{"public"},
	}, http.StatusCreated)
	questionID := stringField(t, questionResp, "id")
	answerResp := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/questions/"+questionID+"/answers", agentToken, map[string]any{
		"body": "Anonymous visitors can read this answer, but cannot vote without logging in.",
	}, http.StatusCreated)
	answerID := stringField(t, answerResp, "id")

	anonymousClient := newHTTPClient(t)
	questions := getJSON(t, anonymousClient, server.URL+"/api/v1/questions", "", http.StatusOK)
	if got := len(listField(t, questions, "items")); got != 1 {
		t.Fatalf("anonymous question list count = %d, want 1", got)
	}
	detail := getJSON(t, anonymousClient, server.URL+"/api/v1/questions/"+questionID, "", http.StatusOK)
	if got := len(listField(t, detail, "answers")); got != 1 {
		t.Fatalf("anonymous answer count = %d, want 1", got)
	}

	assertLoginRequired(t, getRaw(t, anonymousClient, server.URL+"/api/v1/auth/me"), "login required to view current user")
	assertLoginRequired(t, postRawJSON(t, anonymousClient, server.URL+"/api/v1/auth/logout", nil), "login required to log out")
	assertLoginRequired(t, postRawJSON(t, anonymousClient, server.URL+"/api/v1/questions", map[string]any{
		"title": "Blocked",
		"body":  "Anonymous users cannot create questions.",
	}), "login required to create questions")
	assertLoginRequired(t, getRaw(t, anonymousClient, server.URL+"/api/v1/me/agents"), "login required to manage agents")
	assertLoginRequired(t, postRawJSON(t, anonymousClient, server.URL+"/api/v1/me/agents", map[string]any{
		"name": "Blocked Agent",
	}), "login required to manage agents")
	assertLoginRequired(t, postRawJSON(t, anonymousClient, server.URL+"/api/v1/users/usr_missing/follow", nil), "login required to follow users")
	assertLoginRequired(t, postRawJSON(t, anonymousClient, server.URL+"/api/v1/answers/"+answerID+"/like", nil), "login required to like answers")
}

func TestAuthRateLimit(t *testing.T) {
	t.Parallel()

	app, err := newTestApp(t, roundtable.Options{
		Mailer: roundtable.NewMemoryMailer(),
		Now: func() time.Time {
			return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
		},
		RateLimit: roundtable.RateLimitConfig{
			AuthPerMinute: 2,
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	handler := app.Handler()
	body := map[string]any{
		"email":    "missing@example.com",
		"password": testPassword,
	}
	for i := 0; i < 2; i++ {
		resp := postDirectJSON(t, handler, "/api/v1/auth/login", body)
		if resp.Code != http.StatusUnauthorized {
			t.Fatalf("login attempt %d status = %d, want %d", i+1, resp.Code, http.StatusUnauthorized)
		}
	}
	resp := postDirectJSON(t, handler, "/api/v1/auth/login", body)
	if resp.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limited status = %d, want %d", resp.Code, http.StatusTooManyRequests)
	}
}

func TestAgentAPIKeyRateLimit(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	app, err := newTestApp(t, roundtable.Options{
		Mailer: roundtable.NewMemoryMailer(),
		Now: func() time.Time {
			return now
		},
		RateLimit: roundtable.RateLimitConfig{
			AgentPerSecond: 2,
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	handler := app.Handler()
	agentToken := "rt_agent_test_one"
	otherAgentToken := "rt_agent_test_two"

	for i, path := range []string{"/api/v1/agent/invitations", "/api/v1/agent/questions"} {
		resp := getDirectBearer(handler, path, agentToken)
		if resp.Code != http.StatusUnauthorized {
			t.Fatalf("agent request %d status = %d, want %d", i+1, resp.Code, http.StatusUnauthorized)
		}
	}

	limited := getDirectBearer(handler, "/api/v1/agent/questions/qst_missing", agentToken)
	if limited.Code != http.StatusConflict {
		t.Fatalf("agent rate limited status = %d, want %d", limited.Code, http.StatusConflict)
	}
	var limitedBody map[string]any
	if err := json.NewDecoder(limited.Body).Decode(&limitedBody); err != nil {
		t.Fatalf("decode rate limit response: %v", err)
	}
	if got := limitedBody["code"]; got != "agent_rate_limited" {
		t.Fatalf("rate limit code = %#v, want agent_rate_limited", got)
	}
	if got := limitedBody["message"]; got != "agent API key rate limit exceeded: max 2 requests per second" {
		t.Fatalf("rate limit message = %#v", got)
	}

	otherKeyResp := getDirectBearer(handler, "/api/v1/agent/invitations", otherAgentToken)
	if otherKeyResp.Code != http.StatusUnauthorized {
		t.Fatalf("other api key status = %d, want %d", otherKeyResp.Code, http.StatusUnauthorized)
	}

	now = now.Add(time.Second)
	nextSecondResp := getDirectBearer(handler, "/api/v1/agent/invitations", agentToken)
	if nextSecondResp.Code != http.StatusUnauthorized {
		t.Fatalf("next second status = %d, want %d", nextSecondResp.Code, http.StatusUnauthorized)
	}
}

func TestAgentHealthzDoesNotRequireBearerToken(t *testing.T) {
	t.Parallel()

	app, err := newTestApp(t, roundtable.Options{
		Mailer: roundtable.NewMemoryMailer(),
		RateLimit: roundtable.RateLimitConfig{
			AgentPerSecond: 2,
		},
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	handler := app.Handler()
	withoutToken := getDirect(handler, "/api/v1/agent/healthz")
	assertOKHealth(t, withoutToken)

	for i := 0; i < 3; i++ {
		withToken := getDirectBearer(handler, "/api/v1/agent/healthz", "rt_agent_healthz_check")
		assertOKHealth(t, withToken)
	}
}

func TestCORSAllowsBrowserFrontend(t *testing.T) {
	t.Parallel()

	app, err := newTestApp(t, roundtable.Options{
		Mailer: roundtable.NewMemoryMailer(),
	})
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	defer app.Close()

	handler := app.Handler()

	preflight := httptest.NewRequest(http.MethodOptions, "/api/v1/auth/login", nil)
	preflight.Header.Set("Origin", "http://localhost:5173")
	preflight.Header.Set("Access-Control-Request-Method", http.MethodPost)
	preflight.Header.Set("Access-Control-Request-Headers", "content-type, authorization")
	preflightResp := httptest.NewRecorder()
	handler.ServeHTTP(preflightResp, preflight)

	if preflightResp.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, want %d", preflightResp.Code, http.StatusNoContent)
	}
	assertCORSHeader(t, preflightResp.Header(), "Access-Control-Allow-Origin", "http://localhost:5173")
	assertCORSHeader(t, preflightResp.Header(), "Access-Control-Allow-Credentials", "true")
	assertCORSHeader(t, preflightResp.Header(), "Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
	assertCORSHeader(t, preflightResp.Header(), "Access-Control-Allow-Headers", "content-type, authorization")
	assertCORSHeader(t, preflightResp.Header(), "Access-Control-Expose-Headers", "X-Request-Id")
	assertCORSHeader(t, preflightResp.Header(), "Vary", "Origin")

	health := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	health.Header.Set("Origin", "http://localhost:5173")
	healthResp := httptest.NewRecorder()
	handler.ServeHTTP(healthResp, health)

	if healthResp.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", healthResp.Code, http.StatusOK)
	}
	assertCORSHeader(t, healthResp.Header(), "Access-Control-Allow-Origin", "http://localhost:5173")
	assertCORSHeader(t, healthResp.Header(), "Access-Control-Allow-Credentials", "true")
	assertCORSHeader(t, healthResp.Header(), "Access-Control-Expose-Headers", "X-Request-Id")
}

func assertCORSHeader(t *testing.T, header http.Header, name string, want string) {
	t.Helper()

	if got := header.Get(name); got != want {
		t.Fatalf("%s = %q, want %q", name, got, want)
	}
}

func assertSelectable(t *testing.T, db *sql.DB, query string) {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	_ = rows.Close()
}

func registerAndVerifyUser(t *testing.T, client *http.Client, apiURL string, mailer *roundtable.MemoryMailer, email string) {
	t.Helper()

	postJSON(t, client, apiURL+"/api/v1/auth/register", "", map[string]any{
		"email":        email,
		"password":     testPassword,
		"display_name": "Owner",
	}, http.StatusCreated)
	token, ok := mailer.VerificationToken(email)
	if !ok {
		t.Fatalf("verification token was not sent")
	}
	postJSON(t, client, apiURL+"/api/v1/auth/verify", "", map[string]any{
		"token": token,
	}, http.StatusOK)
}

func registerVerifyAndLoginUser(t *testing.T, client *http.Client, apiURL string, mailer *roundtable.MemoryMailer, email string, displayName string) string {
	t.Helper()

	resp := postJSON(t, client, apiURL+"/api/v1/auth/register", "", map[string]any{
		"email":        email,
		"password":     testPassword,
		"display_name": displayName,
	}, http.StatusCreated)
	token, ok := mailer.VerificationToken(email)
	if !ok {
		t.Fatalf("verification token was not sent")
	}
	postJSON(t, client, apiURL+"/api/v1/auth/verify", "", map[string]any{
		"token": token,
	}, http.StatusOK)
	postJSON(t, client, apiURL+"/api/v1/auth/login", "", map[string]any{
		"email":    email,
		"password": testPassword,
	}, http.StatusOK)
	return stringField(t, resp, "id")
}

func postDirectJSON(t *testing.T, handler http.Handler, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()

	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	return resp
}

func getDirectBearer(handler http.Handler, path string, bearerToken string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	return resp
}

func getDirect(handler http.Handler, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	return resp
}

func assertOKHealth(t *testing.T, resp *httptest.ResponseRecorder) {
	t.Helper()

	if resp.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", resp.Code, http.StatusOK)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if got := body["ok"]; got != true {
		t.Fatalf("health ok = %#v, want true", got)
	}
}

func findLogEntry(t *testing.T, logLines string, requestID string) map[string]any {
	t.Helper()

	for _, line := range strings.Split(strings.TrimSpace(logLines), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode log entry %q: %v", line, err)
		}
		if entry["request_id"] == requestID {
			return entry
		}
	}
	t.Fatalf("log entry with request_id %q not found in:\n%s", requestID, logLines)
	return nil
}

func assertLoginRequired(t *testing.T, resp *http.Response, wantMessage string) {
	t.Helper()
	defer resp.Body.Close()

	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d, body = %#v", resp.StatusCode, http.StatusUnauthorized, decoded)
	}
	if got := decoded["code"]; got != "login_required" {
		t.Fatalf("code = %#v, want login_required", got)
	}
	if got := decoded["message"]; got != wantMessage {
		t.Fatalf("message = %#v, want %q", got, wantMessage)
	}
}

func getRaw(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	return resp
}

func postRawJSON(t *testing.T, client *http.Client, url string, body map[string]any) *http.Response {
	t.Helper()

	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	return resp
}

func newHTTPClient(t *testing.T) *http.Client {
	t.Helper()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("new cookie jar: %v", err)
	}
	return &http.Client{Jar: jar}
}

func postJSON(t *testing.T, client *http.Client, url string, bearerToken string, body map[string]any, wantStatus int) map[string]any {
	t.Helper()

	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close()

	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("post %s status = %d, want %d, body = %#v", url, resp.StatusCode, wantStatus, decoded)
	}
	return decoded
}

func patchJSON(t *testing.T, client *http.Client, url string, bearerToken string, body map[string]any, wantStatus int) map[string]any {
	t.Helper()

	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("patch %s: %v", url, err)
	}
	defer resp.Body.Close()

	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("patch %s status = %d, want %d, body = %#v", url, resp.StatusCode, wantStatus, decoded)
	}
	return decoded
}

func deleteJSON(t *testing.T, client *http.Client, url string, bearerToken string, wantStatus int) map[string]any {
	t.Helper()

	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("delete %s: %v", url, err)
	}
	defer resp.Body.Close()

	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("delete %s status = %d, want %d, body = %#v", url, resp.StatusCode, wantStatus, decoded)
	}
	return decoded
}

func postAvatar(t *testing.T, client *http.Client, url string, bearerToken string, body []byte, filename string, wantStatus int) map[string]any {
	t.Helper()

	var payload bytes.Buffer
	writer := multipart.NewWriter(&payload)
	part, err := writer.CreateFormFile("avatar", filename)
	if err != nil {
		t.Fatalf("create avatar form field: %v", err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatalf("write avatar form field: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, url, &payload)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post avatar %s: %v", url, err)
	}
	defer resp.Body.Close()

	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("post avatar %s status = %d, want %d, body = %#v", url, resp.StatusCode, wantStatus, decoded)
	}
	return decoded
}

func testPNG(t *testing.T, width int, height int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: 64, G: 128, B: 192, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func assertAvatarMedia(t *testing.T, client *http.Client, url string) {
	t.Helper()

	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("get avatar media: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("avatar media status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/jpeg" {
		t.Fatalf("avatar media content-type = %q, want image/jpeg", got)
	}
	if got := resp.Header.Get("Content-Disposition"); got != `inline; filename="avatar.jpg"` {
		t.Fatalf("avatar content disposition = %q", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("avatar nosniff header = %q", got)
	}
	if _, err := jpeg.Decode(resp.Body); err != nil {
		t.Fatalf("decode normalized avatar jpeg: %v", err)
	}
}

func getJSON(t *testing.T, client *http.Client, url string, bearerToken string, wantStatus int) map[string]any {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()

	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("get %s status = %d, want %d, body = %#v", url, resp.StatusCode, wantStatus, decoded)
	}
	return decoded
}

func stringField(t *testing.T, values map[string]any, name string) string {
	t.Helper()

	value, ok := values[name].(string)
	if !ok || value == "" {
		t.Fatalf("field %q = %#v, want non-empty string", name, values[name])
	}
	return value
}

func stringFieldAllowEmpty(t *testing.T, values map[string]any, name string) string {
	t.Helper()

	value, ok := values[name].(string)
	if !ok {
		t.Fatalf("field %q = %#v, want string", name, values[name])
	}
	return value
}

func intField(t *testing.T, values map[string]any, name string) int {
	t.Helper()

	value, ok := values[name].(float64)
	if !ok {
		t.Fatalf("field %q = %#v, want number", name, values[name])
	}
	return int(value)
}

func floatField(t *testing.T, values map[string]any, name string) float64 {
	t.Helper()

	value, ok := values[name].(float64)
	if !ok {
		t.Fatalf("field %q = %#v, want number", name, values[name])
	}
	return value
}

func boolField(t *testing.T, values map[string]any, name string) bool {
	t.Helper()

	value, ok := values[name].(bool)
	if !ok {
		t.Fatalf("field %q = %#v, want bool", name, values[name])
	}
	return value
}

func leaderboardAgentScore(t *testing.T, leaderboard map[string]any, agentID string) map[string]any {
	t.Helper()

	for _, raw := range listField(t, leaderboard, "items") {
		item := raw.(map[string]any)
		agent := mapField(t, item, "agent")
		if stringField(t, agent, "id") == agentID {
			return item
		}
	}
	t.Fatalf("agent %s was not present in leaderboard %#v", agentID, leaderboard)
	return nil
}

func leaderboardUserScore(t *testing.T, leaderboard map[string]any, userID string) map[string]any {
	t.Helper()

	for _, raw := range listField(t, leaderboard, "items") {
		item := raw.(map[string]any)
		user := mapField(t, item, "user")
		if stringField(t, user, "id") == userID {
			return item
		}
	}
	t.Fatalf("user %s was not present in leaderboard %#v", userID, leaderboard)
	return nil
}

func assertQuestionIDs(t *testing.T, values map[string]any, want []string) {
	t.Helper()

	items := listField(t, values, "items")
	if len(items) != len(want) {
		t.Fatalf("question count = %d, want %d, items = %#v", len(items), len(want), items)
	}
	got := map[string]bool{}
	for _, item := range items {
		got[stringField(t, item.(map[string]any), "id")] = true
	}
	for _, id := range want {
		if !got[id] {
			t.Fatalf("question ids = %#v, missing %s", got, id)
		}
	}
}

func containsStringValue(values []any, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func listField(t *testing.T, values map[string]any, name string) []any {
	t.Helper()

	value, ok := values[name].([]any)
	if !ok {
		t.Fatalf("field %q = %#v, want list", name, values[name])
	}
	return value
}

func mapField(t *testing.T, values map[string]any, name string) map[string]any {
	t.Helper()

	value, ok := values[name].(map[string]any)
	if !ok {
		t.Fatalf("field %q = %#v, want map", name, values[name])
	}
	return value
}

func assertPagination(t *testing.T, values map[string]any, limit int, offset int, hasMore bool, nextOffset int) {
	t.Helper()

	assertNestedPagination(t, values, "pagination", limit, offset, hasMore, nextOffset)
}

func assertNestedPagination(t *testing.T, values map[string]any, name string, limit int, offset int, hasMore bool, nextOffset int) {
	t.Helper()

	pagination := mapField(t, values, name)
	if got := intField(t, pagination, "limit"); got != limit {
		t.Fatalf("%s.limit = %d, want %d", name, got, limit)
	}
	if got := intField(t, pagination, "offset"); got != offset {
		t.Fatalf("%s.offset = %d, want %d", name, got, offset)
	}
	if got := boolField(t, pagination, "has_more"); got != hasMore {
		t.Fatalf("%s.has_more = %t, want %t", name, got, hasMore)
	}
	if hasMore {
		if got := intField(t, pagination, "next_offset"); got != nextOffset {
			t.Fatalf("%s.next_offset = %d, want %d", name, got, nextOffset)
		}
		return
	}
	if got := pagination["next_offset"]; got != nil {
		t.Fatalf("%s.next_offset = %#v, want nil", name, got)
	}
}

func assertAnswerCommentCount(t *testing.T, question map[string]any, answerID string, want int) {
	t.Helper()

	for _, raw := range listField(t, question, "answers") {
		answer := raw.(map[string]any)
		if stringField(t, answer, "id") == answerID {
			if got := intField(t, answer, "comment_count"); got != want {
				t.Fatalf("answer %s comment_count = %d, want %d", answerID, got, want)
			}
			return
		}
	}
	t.Fatalf("answer %s was not present in question detail", answerID)
}

func answerFeedItem(t *testing.T, feed map[string]any, answerID string) map[string]any {
	t.Helper()

	for _, raw := range listField(t, feed, "items") {
		item := raw.(map[string]any)
		answer := mapField(t, item, "answer")
		if stringField(t, answer, "id") == answerID {
			return answer
		}
	}
	t.Fatalf("answer %s was not present in answer feed", answerID)
	return nil
}

func assertAnswerFeedCommentCount(t *testing.T, feed map[string]any, answerID string, want int) {
	t.Helper()

	for _, raw := range listField(t, feed, "items") {
		item := raw.(map[string]any)
		answer := mapField(t, item, "answer")
		if stringField(t, answer, "id") == answerID {
			if got := intField(t, answer, "comment_count"); got != want {
				t.Fatalf("feed answer %s comment_count = %d, want %d", answerID, got, want)
			}
			return
		}
	}
	t.Fatalf("answer %s was not present in answer feed", answerID)
}

type fakeTranslationProvider struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (p *fakeTranslationProvider) Translate(_ context.Context, req roundtable.TranslationProviderRequest) (roundtable.TranslationProviderResult, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if req.SourceLanguage == "" {
		return roundtable.TranslationProviderResult{}, errors.New("missing source language")
	}
	if req.SourceLanguage == req.TargetLanguage {
		return roundtable.TranslationProviderResult{}, errors.New("same-language translation reached provider")
	}
	if p.err != nil {
		return roundtable.TranslationProviderResult{}, p.err
	}
	title := ""
	if req.Title != "" {
		title = "[" + req.TargetLanguage + "] " + req.Title
	}
	return roundtable.TranslationProviderResult{
		Title:        title,
		Body:         "[" + req.TargetLanguage + "] " + req.Body,
		Provider:     "fake",
		Model:        "fake-translation",
		InputTokens:  11,
		OutputTokens: 13,
		CostMicros:   17,
	}, nil
}

func (p *fakeTranslationProvider) CallCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}
