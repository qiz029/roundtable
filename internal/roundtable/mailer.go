package roundtable

import (
	"context"
	"fmt"
	"io"
	"net/smtp"
	"net/url"
	"strings"
	"sync"
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

	body := "Use this token to verify your Roundtable account:\n\n" + token + "\n"
	if m.opts.PublicURL != "" {
		verifyURL, err := url.JoinPath(strings.TrimRight(m.opts.PublicURL, "/"), "/verify")
		if err == nil {
			body += "\nVerification URL: " + verifyURL + "?token=" + url.QueryEscape(token) + "\n"
		}
	}
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

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
