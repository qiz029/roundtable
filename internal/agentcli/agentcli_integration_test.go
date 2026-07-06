package agentcli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/qiz029/roundtable/internal/agentcli"
	"github.com/qiz029/roundtable/internal/roundtable"
)

const testPassword = "correct horse battery staple 1"

func TestVersionCommandPrintsBuildInfo(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	err := agentcli.Run(context.Background(), []string{"version"}, agentcli.Options{
		Stdout: &stdout,
		Stderr: &bytes.Buffer{},
		Version: agentcli.VersionInfo{
			Version: "0.1.0",
			Commit:  "abc123",
			Date:    "2026-07-03T00:00:00Z",
		},
	})
	if err != nil {
		t.Fatalf("version command: %v", err)
	}

	output := strings.TrimSpace(stdout.String())
	want := "roundtable-agent version 0.1.0 commit abc123 built 2026-07-03T00:00:00Z"
	if output != want {
		t.Fatalf("version output = %q, want %q", output, want)
	}
}

func TestUpdateCommandDryRunPrintsInstallerCommand(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	if err := agentcli.Run(context.Background(), []string{
		"update",
		"--version", "1.2.3",
		"--install-dir", "/tmp/round table/bin",
		"--dry-run",
	}, agentcli.Options{
		Stdout: &stdout,
		Stderr: &bytes.Buffer{},
	}); err != nil {
		t.Fatalf("update dry-run: %v", err)
	}

	output := stdout.String()
	for _, want := range []string{
		"curl -fsSL https://github.com/qiz029/roundtable/releases/latest/download/install.sh",
		"-o \"$tmp\"",
		"ROUNDTABLE_AGENT_VERSION='1.2.3'",
		"ROUNDTABLE_INSTALL_DIR='/tmp/round table/bin'",
		"bash \"$tmp\"",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("update dry-run output = %q, missing %q", output, want)
		}
	}
}

func TestUpdateCommandRunsInstallerCommand(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var command string
	err := agentcli.Run(context.Background(), []string{"update"}, agentcli.Options{
		Stdout: &stdout,
		Stderr: &stderr,
		Exec: func(_ context.Context, got string, stdin []byte) ([]byte, []byte, error) {
			command = got
			if len(stdin) != 0 {
				t.Fatalf("update stdin = %q, want empty", string(stdin))
			}
			return []byte("updated\n"), []byte("installer warning\n"), nil
		},
	})
	if err != nil {
		t.Fatalf("update command: %v", err)
	}
	for _, want := range []string{
		"tmp=\"$(mktemp)\"",
		"curl -fsSL https://github.com/qiz029/roundtable/releases/latest/download/install.sh -o \"$tmp\"",
		"bash \"$tmp\"",
		"rm -f \"$tmp\"",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("update command = %q, missing %q", command, want)
		}
	}
	if got := stdout.String(); got != "updated\n" {
		t.Fatalf("stdout = %q, want installer stdout", got)
	}
	if got := stderr.String(); got != "installer warning\n" {
		t.Fatalf("stderr = %q, want installer stderr", got)
	}
}

func TestProfileShowPrintsCurrentAgentProfile(t *testing.T) {
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
	registerVerifiedUser(t, userClient, server.URL, mailer)
	loginUser(t, userClient, server.URL)
	agentToken := createAgent(t, userClient, server.URL)

	var stdout bytes.Buffer
	opts := agentcli.Options{
		HomeDir: t.TempDir(),
		Stdout:  &stdout,
		Stderr:  &bytes.Buffer{},
	}
	if err := agentcli.Run(context.Background(), []string{
		"login",
		"--api-url", server.URL,
		"--token", agentToken,
	}, opts); err != nil {
		t.Fatalf("agent login: %v", err)
	}

	stdout.Reset()
	if err := agentcli.Run(context.Background(), []string{"profile", "show"}, opts); err != nil {
		t.Fatalf("profile show: %v", err)
	}

	var profile map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &profile); err != nil {
		t.Fatalf("decode profile: %v", err)
	}
	if got := stringField(t, profile, "name"); got != "External Agent" {
		t.Fatalf("profile name = %q, want External Agent", got)
	}
	if got := stringField(t, profile, "description"); got != "Answers through the agent CLI." {
		t.Fatalf("profile description = %q", got)
	}
	if got := stringField(t, profile, "instructions"); got != "Prefer concise answers with concrete examples." {
		t.Fatalf("profile instructions = %q", got)
	}
	if got := len(listField(t, profile, "capabilities")); got != 1 {
		t.Fatalf("profile capabilities count = %d, want 1", got)
	}
}

func TestRunConsumesInvitationAndSubmitsAnswer(t *testing.T) {
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
	registerVerifiedUser(t, userClient, server.URL, mailer)
	loginUser(t, userClient, server.URL)
	agentToken := createAgent(t, userClient, server.URL)
	questionID := createQuestion(t, userClient, server.URL)

	var stdout bytes.Buffer
	var commandPayload map[string]any
	opts := agentcli.Options{
		HomeDir: t.TempDir(),
		Stdout:  &stdout,
		Stderr:  &bytes.Buffer{},
		Exec: func(_ context.Context, command string, stdin []byte) ([]byte, []byte, error) {
			if command != "mock-answer" {
				t.Fatalf("command = %q, want mock-answer", command)
			}
			if err := json.Unmarshal(stdin, &commandPayload); err != nil {
				t.Fatalf("decode command payload: %v", err)
			}
			return []byte("This answer was produced by an external agent command.\n"), nil, nil
		},
	}

	if err := agentcli.Run(context.Background(), []string{
		"login",
		"--api-url", server.URL,
		"--token", agentToken,
	}, opts); err != nil {
		t.Fatalf("agent login: %v", err)
	}
	stdout.Reset()
	if err := agentcli.Run(context.Background(), []string{
		"questions",
		"show",
		questionID,
	}, opts); err != nil {
		t.Fatalf("question show: %v", err)
	}
	var shownQuestion map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &shownQuestion); err != nil {
		t.Fatalf("decode shown question: %v", err)
	}
	if got := stringField(t, shownQuestion, "id"); got != questionID {
		t.Fatalf("shown question id = %q, want %q", got, questionID)
	}

	stdout.Reset()
	if err := agentcli.Run(context.Background(), []string{
		"run",
		"--once",
		"--exec", "mock-answer",
	}, opts); err != nil {
		t.Fatalf("agent run: %v", err)
	}

	question := mapField(t, commandPayload, "question")
	if got := stringField(t, question, "id"); got != questionID {
		t.Fatalf("command question id = %q, want %q", got, questionID)
	}
	detail := getJSON(t, userClient, server.URL+"/api/v1/questions/"+questionID, "", http.StatusOK)
	answers := listField(t, detail, "answers")
	if len(answers) != 1 {
		t.Fatalf("answer count = %d, want 1", len(answers))
	}
	answer := answers[0].(map[string]any)
	if got := stringField(t, answer, "body"); got != "This answer was produced by an external agent command." {
		t.Fatalf("answer body = %q", got)
	}

	stdout.Reset()
	if err := agentcli.Run(context.Background(), []string{
		"answers",
		"list",
		"--question", questionID,
	}, opts); err != nil {
		t.Fatalf("answers list: %v", err)
	}
	var answerList map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &answerList); err != nil {
		t.Fatalf("decode answer list: %v", err)
	}
	if got := len(listField(t, answerList, "items")); got != 1 {
		t.Fatalf("cli answer count = %d, want 1", got)
	}
}

func TestFeedListUsesAgentFeed(t *testing.T) {
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
	registerVerifiedUser(t, userClient, server.URL, mailer)
	loginUser(t, userClient, server.URL)
	agentToken := createAgent(t, userClient, server.URL)
	matchedQuestionID := createQuestion(t, userClient, server.URL)
	now = now.Add(time.Minute)
	recentQuestionID := createQuestionWithTags(t, userClient, server.URL,
		"How should office snacks be organized?",
		"This is newer but not related to automation.",
		[]string{"office"},
	)

	var stdout bytes.Buffer
	opts := agentcli.Options{
		HomeDir: t.TempDir(),
		Stdout:  &stdout,
		Stderr:  &bytes.Buffer{},
	}
	if err := agentcli.Run(context.Background(), []string{
		"login",
		"--api-url", server.URL,
		"--token", agentToken,
	}, opts); err != nil {
		t.Fatalf("agent login: %v", err)
	}

	stdout.Reset()
	if err := agentcli.Run(context.Background(), []string{"feed", "list"}, opts); err != nil {
		t.Fatalf("feed list: %v", err)
	}
	var feed map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &feed); err != nil {
		t.Fatalf("decode feed: %v", err)
	}
	items := listField(t, feed, "items")
	if len(items) != 2 {
		t.Fatalf("feed item count = %d, want 2", len(items))
	}
	if got := stringField(t, items[0].(map[string]any), "id"); got != matchedQuestionID {
		t.Fatalf("feed first id = %q, want tag-matched question", got)
	}
	if got := stringField(t, items[1].(map[string]any), "id"); got != recentQuestionID {
		t.Fatalf("feed second id = %q, want recent unrelated question", got)
	}
}

func TestResponsesCommandsSubmitListAndUpdate(t *testing.T) {
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

	askerClient := newHTTPClient(t)
	registerVerifiedUserWithEmail(t, askerClient, server.URL, mailer, "cli-response-asker@example.com", "CLI Response Asker")
	loginUserWithEmail(t, askerClient, server.URL, "cli-response-asker@example.com")
	questionID := createQuestion(t, askerClient, server.URL)

	answerOwnerClient := newHTTPClient(t)
	registerVerifiedUserWithEmail(t, answerOwnerClient, server.URL, mailer, "cli-answer-owner@example.com", "CLI Answer Owner")
	loginUserWithEmail(t, answerOwnerClient, server.URL, "cli-answer-owner@example.com")
	answerAgentToken := createAgentWithName(t, answerOwnerClient, server.URL, "CLI Answer Agent")
	answer := postJSON(t, newHTTPClient(t), server.URL+"/api/v1/agent/questions/"+questionID+"/answers", answerAgentToken, map[string]any{
		"body": "Answer responses should be bounded manual annotations.",
	}, http.StatusCreated)
	answerID := stringField(t, answer, "id")

	responseOwnerClient := newHTTPClient(t)
	registerVerifiedUserWithEmail(t, responseOwnerClient, server.URL, mailer, "cli-response-owner@example.com", "CLI Response Owner")
	loginUserWithEmail(t, responseOwnerClient, server.URL, "cli-response-owner@example.com")
	responseAgentToken := createAgentWithName(t, responseOwnerClient, server.URL, "CLI Response Agent")

	var stdout bytes.Buffer
	opts := agentcli.Options{
		HomeDir: t.TempDir(),
		Stdout:  &stdout,
		Stderr:  &bytes.Buffer{},
	}
	if err := agentcli.Run(context.Background(), []string{
		"login",
		"--api-url", server.URL,
		"--token", responseAgentToken,
	}, opts); err != nil {
		t.Fatalf("agent login: %v", err)
	}

	stdout.Reset()
	if err := agentcli.Run(context.Background(), []string{
		"responses",
		"submit",
		"--answer", answerID,
		"--stance", "disagree",
		"--body", "  This should not create another agent task.  ",
	}, opts); err != nil {
		t.Fatalf("responses submit: %v", err)
	}
	var submitted map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &submitted); err != nil {
		t.Fatalf("decode submitted response: %v", err)
	}
	responseID := stringField(t, submitted, "id")
	if got := stringField(t, submitted, "body"); got != "This should not create another agent task." {
		t.Fatalf("submitted response body = %q", got)
	}
	if got := stringField(t, submitted, "stance"); got != "disagree" {
		t.Fatalf("submitted response stance = %q", got)
	}

	stdout.Reset()
	if err := agentcli.Run(context.Background(), []string{
		"responses",
		"list",
		"--answer", answerID,
	}, opts); err != nil {
		t.Fatalf("responses list: %v", err)
	}
	var listed map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &listed); err != nil {
		t.Fatalf("decode response list: %v", err)
	}
	items := listField(t, listed, "items")
	if len(items) != 1 {
		t.Fatalf("response list count = %d, want 1", len(items))
	}
	if got := stringField(t, items[0].(map[string]any), "id"); got != responseID {
		t.Fatalf("listed response id = %q, want %q", got, responseID)
	}

	stdout.Reset()
	if err := agentcli.Run(context.Background(), []string{
		"responses",
		"update",
		responseID,
		"--stance", "clarify",
		"--body", "Updated bounded response.",
	}, opts); err != nil {
		t.Fatalf("responses update: %v", err)
	}
	var updated map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &updated); err != nil {
		t.Fatalf("decode updated response: %v", err)
	}
	if got := stringField(t, updated, "body"); got != "Updated bounded response." {
		t.Fatalf("updated response body = %q", got)
	}
	if got := stringField(t, updated, "stance"); got != "clarify" {
		t.Fatalf("updated response stance = %q", got)
	}
}

func registerVerifiedUser(t *testing.T, client *http.Client, apiURL string, mailer *roundtable.MemoryMailer) {
	t.Helper()

	registerVerifiedUserWithEmail(t, client, apiURL, mailer, "owner@example.com", "Owner")
}

func registerVerifiedUserWithEmail(t *testing.T, client *http.Client, apiURL string, mailer *roundtable.MemoryMailer, email string, displayName string) {
	t.Helper()

	postJSON(t, client, apiURL+"/api/v1/auth/register", "", map[string]any{
		"email":        email,
		"password":     testPassword,
		"display_name": displayName,
	}, http.StatusCreated)
	token, ok := mailer.VerificationToken(email)
	if !ok {
		t.Fatalf("verification token was not sent")
	}
	postJSON(t, client, apiURL+"/api/v1/auth/verify", "", map[string]any{"token": token}, http.StatusOK)
}

func loginUser(t *testing.T, client *http.Client, apiURL string) {
	t.Helper()

	loginUserWithEmail(t, client, apiURL, "owner@example.com")
}

func loginUserWithEmail(t *testing.T, client *http.Client, apiURL string, email string) {
	t.Helper()

	postJSON(t, client, apiURL+"/api/v1/auth/login", "", map[string]any{
		"email":    email,
		"password": testPassword,
	}, http.StatusOK)
}

func createAgent(t *testing.T, client *http.Client, apiURL string) string {
	t.Helper()

	return createAgentWithName(t, client, apiURL, "External Agent")
}

func createAgentWithName(t *testing.T, client *http.Client, apiURL string, name string) string {
	t.Helper()

	resp := postJSON(t, client, apiURL+"/api/v1/me/agents", "", map[string]any{
		"name":         name,
		"description":  "Answers through the agent CLI.",
		"tags":         []string{"cli"},
		"capabilities": []string{"answering"},
		"instructions": "Prefer concise answers with concrete examples.",
		"is_public":    true,
	}, http.StatusCreated)
	return stringField(t, resp, "token")
}

func createQuestion(t *testing.T, client *http.Client, apiURL string) string {
	t.Helper()

	return createQuestionWithTags(t, client, apiURL,
		"Can an agent CLI answer invitations?",
		"The CLI should feed JSON to an external command.",
		[]string{"cli"},
	)
}

func createQuestionWithTags(t *testing.T, client *http.Client, apiURL string, title string, body string, tags []string) string {
	t.Helper()

	resp := postJSON(t, client, apiURL+"/api/v1/questions", "", map[string]any{
		"title": title,
		"body":  body,
		"tags":  tags,
	}, http.StatusCreated)
	return stringField(t, resp, "id")
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

	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
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
