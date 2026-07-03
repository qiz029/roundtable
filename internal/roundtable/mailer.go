package roundtable

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/smtp"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Mailer interface {
	SendVerification(ctx context.Context, email string, token string) error
}

type MemoryMailer struct {
	mu     sync.Mutex
	tokens map[string]string
}

func NewMemoryMailer() *MemoryMailer {
	return &MemoryMailer{tokens: map[string]string{}}
}

func (m *MemoryMailer) SendVerification(_ context.Context, email string, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.tokens[normalizeEmail(email)] = token
	return nil
}

func (m *MemoryMailer) VerificationToken(email string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	token, ok := m.tokens[normalizeEmail(email)]
	return token, ok
}

type LogMailer struct {
	writer io.Writer
}

func NewLogMailer(writer io.Writer) *LogMailer {
	return &LogMailer{writer: writer}
}

func (m *LogMailer) SendVerification(_ context.Context, email string, token string) error {
	_, err := fmt.Fprintf(m.writer, "verification email=%s token=%s\n", email, token)
	return err
}

type SMTPOptions struct {
	Addr      string
	Username  string
	Password  string
	From      string
	PublicURL string
}

type SMTPMailer struct {
	opts SMTPOptions
}

func NewSMTPMailer(opts SMTPOptions) (*SMTPMailer, error) {
	if strings.TrimSpace(opts.Addr) == "" {
		return nil, fmt.Errorf("smtp addr is required")
	}
	if strings.TrimSpace(opts.From) == "" {
		return nil, fmt.Errorf("smtp from is required")
	}
	return &SMTPMailer{opts: opts}, nil
}

func (m *SMTPMailer) SendVerification(_ context.Context, email string, token string) error {
	host := m.opts.Addr
	if parts := strings.Split(m.opts.Addr, ":"); len(parts) > 0 {
		host = parts[0]
	}

	var auth smtp.Auth
	if m.opts.Username != "" {
		auth = smtp.PlainAuth("", m.opts.Username, m.opts.Password, host)
	}

	body := verificationEmailText(m.opts.PublicURL, token)
	message := strings.Join([]string{
		"From: " + m.opts.From,
		"To: " + email,
		"Subject: Verify your Roundtable account",
		"Content-Type: text/plain; charset=utf-8",
		"",
		body,
	}, "\r\n")

	return smtp.SendMail(m.opts.Addr, auth, m.opts.From, []string{email}, []byte(message))
}

type MailgunOptions struct {
	APIBaseURL string
	Domain     string
	APIKey     string
	From       string
	PublicURL  string
	HTTPClient *http.Client
}

type MailgunMailer struct {
	opts   MailgunOptions
	client *http.Client
}

func NewMailgunMailer(opts MailgunOptions) (*MailgunMailer, error) {
	if strings.TrimSpace(opts.Domain) == "" {
		return nil, fmt.Errorf("mailgun domain is required")
	}
	if strings.TrimSpace(opts.APIKey) == "" {
		return nil, fmt.Errorf("mailgun api key is required")
	}
	if strings.TrimSpace(opts.From) == "" {
		return nil, fmt.Errorf("mailgun from is required")
	}
	apiBaseURL := strings.TrimSpace(opts.APIBaseURL)
	if apiBaseURL == "" {
		apiBaseURL = "https://api.mailgun.net"
	}
	parsedBaseURL, err := url.ParseRequestURI(apiBaseURL)
	if err != nil || parsedBaseURL.Scheme == "" || parsedBaseURL.Host == "" {
		return nil, fmt.Errorf("mailgun api base url is invalid")
	}
	opts.APIBaseURL = strings.TrimRight(apiBaseURL, "/")
	opts.Domain = strings.TrimSpace(opts.Domain)
	opts.APIKey = strings.TrimSpace(opts.APIKey)
	opts.From = strings.TrimSpace(opts.From)

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &MailgunMailer{opts: opts, client: client}, nil
}

func (m *MailgunMailer) SendVerification(ctx context.Context, email string, token string) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("from", m.opts.From); err != nil {
		return err
	}
	if err := writer.WriteField("to", email); err != nil {
		return err
	}
	if err := writer.WriteField("subject", "Verify your Roundtable account"); err != nil {
		return err
	}
	if err := writer.WriteField("text", verificationEmailText(m.opts.PublicURL, token)); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.messagesURL(), &body)
	if err != nil {
		return err
	}
	req.SetBasicAuth("api", m.opts.APIKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		detail := strings.TrimSpace(string(responseBody))
		if detail != "" {
			return fmt.Errorf("mailgun send verification failed: status %d: %s", resp.StatusCode, detail)
		}
		return fmt.Errorf("mailgun send verification failed: status %d", resp.StatusCode)
	}
	return nil
}

func (m *MailgunMailer) messagesURL() string {
	return m.opts.APIBaseURL + "/v3/" + url.PathEscape(m.opts.Domain) + "/messages"
}

func verificationEmailText(publicURL string, token string) string {
	body := "Use this token to verify your Roundtable account:\n\n" + token + "\n"
	if publicURL != "" {
		verifyURL, err := url.JoinPath(strings.TrimRight(publicURL, "/"), "/verify")
		if err == nil {
			body += "\nVerification URL: " + verifyURL + "?token=" + url.QueryEscape(token) + "\n"
		}
	}
	return body
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
