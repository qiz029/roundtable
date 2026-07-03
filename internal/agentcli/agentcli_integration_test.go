package agentcli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiz029/roundtable/internal/agentcli"
	"github.com/qiz029/roundtable/internal/roundtable"
)

func TestRunConsumesInvitationAndSubmitsAnswer(t *testing.T) {
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

func registerVerifiedUser(t *testing.T, client *http.Client, apiURL string, mailer *roundtable.MemoryMailer) {
	t.Helper()

	postJSON(t, client, apiURL+"/api/v1/auth/register", "", map[string]any{
		"email":        "owner@example.com",
		"password":     "correct horse battery staple",
		"display_name": "Owner",
	}, http.StatusCreated)
	token, ok := mailer.VerificationToken("owner@example.com")
	if !ok {
		t.Fatalf("verification token was not sent")
	}
	postJSON(t, client, apiURL+"/api/v1/auth/verify", "", map[string]any{"token": token}, http.StatusOK)
}

func loginUser(t *testing.T, client *http.Client, apiURL string) {
	t.Helper()

	postJSON(t, client, apiURL+"/api/v1/auth/login", "", map[string]any{
		"email":    "owner@example.com",
		"password": "correct horse battery staple",
	}, http.StatusOK)
}

func createAgent(t *testing.T, client *http.Client, apiURL string) string {
	t.Helper()

	resp := postJSON(t, client, apiURL+"/api/v1/me/agents", "", map[string]any{
		"name":         "External Agent",
		"description":  "Answers through the agent CLI.",
		"tags":         []string{"cli"},
		"capabilities": []string{"answering"},
		"is_public":    true,
	}, http.StatusCreated)
	return stringField(t, resp, "token")
}

func createQuestion(t *testing.T, client *http.Client, apiURL string) string {
	t.Helper()

	resp := postJSON(t, client, apiURL+"/api/v1/questions", "", map[string]any{
		"title": "Can an agent CLI answer invitations?",
		"body":  "The CLI should feed JSON to an external command.",
		"tags":  []string{"cli"},
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
