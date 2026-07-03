package roundtable_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qiz029/roundtable/internal/roundtable"
)

func TestMailgunMailerSendsVerificationEmail(t *testing.T) {
	var sawRequest bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequest = true
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v3/mg.example.com/messages" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		username, password, ok := r.BasicAuth()
		if !ok || username != "api" || password != "test-key" {
			t.Fatalf("basic auth = %q/%q/%v, want api/test-key/true", username, password, ok)
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data;") {
			t.Fatalf("content type = %q, want multipart/form-data", r.Header.Get("Content-Type"))
		}
		if err := r.ParseMultipartForm(4096); err != nil {
			t.Fatalf("parse multipart form: %v", err)
		}
		assertFormValue(t, r, "from", "Roundtable <noreply@mg.example.com>")
		assertFormValue(t, r, "to", "owner@example.com")
		assertFormValue(t, r, "subject", "Verify your Roundtable account")
		text := r.FormValue("text")
		if !strings.Contains(text, "rt_verify_test") {
			t.Fatalf("text body does not contain token: %q", text)
		}
		if !strings.Contains(text, "https://roundtable.example.com/verify?token=rt_verify_test") {
			t.Fatalf("text body does not contain verification URL: %q", text)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"message-id","message":"Queued. Thank you."}`))
	}))
	defer server.Close()

	mailer, err := roundtable.NewMailgunMailer(roundtable.MailgunOptions{
		APIBaseURL: server.URL,
		Domain:     "mg.example.com",
		APIKey:     "test-key",
		From:       "Roundtable <noreply@mg.example.com>",
		PublicURL:  "https://roundtable.example.com",
	})
	if err != nil {
		t.Fatalf("new mailgun mailer: %v", err)
	}

	if err := mailer.SendVerification(context.Background(), "owner@example.com", "rt_verify_test"); err != nil {
		t.Fatalf("send verification: %v", err)
	}
	if !sawRequest {
		t.Fatal("mailgun server did not receive request")
	}
}

func assertFormValue(t *testing.T, r *http.Request, name string, want string) {
	t.Helper()
	if got := r.FormValue(name); got != want {
		t.Fatalf("form[%s] = %q, want %q", name, got, want)
	}
}
