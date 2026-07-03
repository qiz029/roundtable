package roundtable_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiz029/roundtable/internal/roundtable"
)

const testPassword = "correct horse battery staple 1"

func TestUserAgentQuestionRoundTrip(t *testing.T) {
	t.Parallel()

	mailer := roundtable.NewMemoryMailer()
	app, err := roundtable.NewApp(roundtable.Options{
		DBPath: filepath.Join(t.TempDir(), "roundtable.db"),
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

	postJSON(t, userClient, server.URL+"/api/v1/answers/"+answerID+"/like", "", nil, http.StatusOK)
	postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/answers/"+answerID+"/like", secondAgentToken, nil, http.StatusOK)

	detail = getJSON(t, userClient, server.URL+"/api/v1/questions/"+questionID, "", http.StatusOK)
	answer = listField(t, detail, "answers")[0].(map[string]any)
	if got := intField(t, answer, "like_count"); got != 2 {
		t.Fatalf("like_count = %d, want 2", got)
	}

	postJSON(t, agentClient, server.URL+"/api/v1/agent/answers/"+answerID+"/like", agentToken, nil, http.StatusForbidden)
}

func TestQuestionSearchMatchesTitleAndBody(t *testing.T) {
	t.Parallel()

	mailer := roundtable.NewMemoryMailer()
	app, err := roundtable.NewApp(roundtable.Options{
		DBPath: filepath.Join(t.TempDir(), "roundtable.db"),
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

func TestQuestionSearchBackfillsExistingQuestions(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "roundtable.db")
	mailer := roundtable.NewMemoryMailer()
	app, err := roundtable.NewApp(roundtable.Options{
		DBPath: dbPath,
		Mailer: mailer,
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
	if err := app.Close(); err != nil {
		t.Fatalf("close app: %v", err)
	}

	reopened, err := roundtable.NewApp(roundtable.Options{
		DBPath: dbPath,
		Mailer: roundtable.NewMemoryMailer(),
	})
	if err != nil {
		t.Fatalf("reopen app: %v", err)
	}
	defer reopened.Close()
	reopenedServer := httptest.NewServer(reopened.Handler())
	defer reopenedServer.Close()

	found := getJSON(t, newHTTPClient(t), reopenedServer.URL+"/api/v1/questions?q=backfill", "", http.StatusOK)
	items := listField(t, found, "items")
	if len(items) != 1 {
		t.Fatalf("search result count after reopen = %d, want 1", len(items))
	}
	if got := stringField(t, items[0].(map[string]any), "id"); got != questionID {
		t.Fatalf("search result id after reopen = %q, want %q", got, questionID)
	}
}

func TestQuestionInvitesAtMostFiveAgents(t *testing.T) {
	t.Parallel()

	mailer := roundtable.NewMemoryMailer()
	app, err := roundtable.NewApp(roundtable.Options{
		DBPath: filepath.Join(t.TempDir(), "roundtable.db"),
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

	for i := 0; i < 6; i++ {
		postJSON(t, userClient, server.URL+"/api/v1/me/agents", "", map[string]any{
			"name":         "Agent " + string(rune('A'+i)),
			"description":  "Participates in random invitations.",
			"tags":         []string{"random"},
			"capabilities": []string{"answering"},
			"is_public":    true,
		}, http.StatusCreated)
	}

	questionResp := postJSON(t, userClient, server.URL+"/api/v1/questions", "", map[string]any{
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
	app, err := roundtable.NewApp(roundtable.Options{
		DBPath: filepath.Join(t.TempDir(), "roundtable.db"),
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
	app, err := roundtable.NewApp(roundtable.Options{
		DBPath: filepath.Join(t.TempDir(), "roundtable.db"),
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
	app, err := roundtable.NewApp(roundtable.Options{
		DBPath: filepath.Join(t.TempDir(), "roundtable.db"),
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
	app, err := roundtable.NewApp(roundtable.Options{
		DBPath: filepath.Join(t.TempDir(), "roundtable.db"),
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
	assertLoginRequired(t, postRawJSON(t, anonymousClient, server.URL+"/api/v1/answers/"+answerID+"/like", nil), "login required to like answers")
}

func TestAuthRateLimit(t *testing.T) {
	t.Parallel()

	app, err := roundtable.NewApp(roundtable.Options{
		DBPath: filepath.Join(t.TempDir(), "roundtable.db"),
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
	app, err := roundtable.NewApp(roundtable.Options{
		DBPath: filepath.Join(t.TempDir(), "roundtable.db"),
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

	app, err := roundtable.NewApp(roundtable.Options{
		DBPath: filepath.Join(t.TempDir(), "roundtable.db"),
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

	app, err := roundtable.NewApp(roundtable.Options{
		DBPath: filepath.Join(t.TempDir(), "roundtable.db"),
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
	assertCORSHeader(t, preflightResp.Header(), "Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	assertCORSHeader(t, preflightResp.Header(), "Access-Control-Allow-Headers", "content-type, authorization")
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
}

func assertCORSHeader(t *testing.T, header http.Header, name string, want string) {
	t.Helper()

	if got := header.Get(name); got != want {
		t.Fatalf("%s = %q, want %q", name, got, want)
	}
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

func intField(t *testing.T, values map[string]any, name string) int {
	t.Helper()

	value, ok := values[name].(float64)
	if !ok {
		t.Fatalf("field %q = %#v, want number", name, values[name])
	}
	return int(value)
}

func boolField(t *testing.T, values map[string]any, name string) bool {
	t.Helper()

	value, ok := values[name].(bool)
	if !ok {
		t.Fatalf("field %q = %#v, want bool", name, values[name])
	}
	return value
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
