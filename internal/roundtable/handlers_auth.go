package roundtable

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
)

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errMethodNotAllowed())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, errMethodNotAllowed())
		return
	}

	var req struct {
		Email       string `json:"email"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}

	email := normalizeEmail(req.Email)
	displayName := strings.TrimSpace(req.DisplayName)
	if !strings.Contains(email, "@") {
		writeError(w, errInvalidInput("valid email is required"))
		return
	}
	if displayName == "" {
		writeError(w, errInvalidInput("display_name is required"))
		return
	}
	passwordHash, err := hashPassword(req.Password)
	if err != nil {
		writeError(w, err)
		return
	}

	userID, err := newID("usr")
	if err != nil {
		writeError(w, err)
		return
	}
	verificationToken, err := newSecret("rt_verify")
	if err != nil {
		writeError(w, err)
		return
	}
	now := a.now().UTC().Format(time.RFC3339Nano)

	_, err = a.db.ExecContext(r.Context(), `
		INSERT INTO users (id, email, display_name, password_hash, verification_token_hash, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, userID, email, displayName, passwordHash, hashSecret(verificationToken), now)
	if err != nil {
		if isUniqueErr(err) {
			writeError(w, errConflict("email already registered"))
			return
		}
		writeError(w, err)
		return
	}
	if err := a.mailer.SendVerification(r.Context(), email, verificationToken); err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":             userID,
		"email":          email,
		"display_name":   displayName,
		"email_verified": false,
	})
}

func (a *App) handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, errMethodNotAllowed())
		return
	}

	var req struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	token := strings.TrimSpace(req.Token)
	if token == "" {
		writeError(w, errInvalidInput("token is required"))
		return
	}

	now := a.now().UTC().Format(time.RFC3339Nano)
	result, err := a.db.ExecContext(r.Context(), `
		UPDATE users
		SET email_verified_at = ?, verification_token_hash = NULL
		WHERE verification_token_hash = ? AND email_verified_at IS NULL
	`, now, hashSecret(token))
	if err != nil {
		writeError(w, err)
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		writeError(w, errInvalidInput("invalid verification token"))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"verified": true})
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, errMethodNotAllowed())
		return
	}

	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}

	user, passwordHash, err := a.userByEmail(r.Context(), normalizeEmail(req.Email))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, errUnauthorized())
			return
		}
		writeError(w, err)
		return
	}
	if user.Status != "active" || !verifyPassword(passwordHash, req.Password) {
		writeError(w, errUnauthorized())
		return
	}

	sessionID, err := newID("ses")
	if err != nil {
		writeError(w, err)
		return
	}
	sessionToken, err := newSecret("rt_session")
	if err != nil {
		writeError(w, err)
		return
	}
	now := a.now().UTC()
	expiresAt := now.Add(30 * 24 * time.Hour)
	if _, err := a.db.ExecContext(r.Context(), `
		INSERT INTO sessions (id, user_id, token_hash, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, sessionID, user.ID, hashSecret(sessionToken), expiresAt.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		writeError(w, err)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cookieSecure,
		SameSite: http.SameSiteLaxMode,
		Expires:  expiresAt,
	})
	writeJSON(w, http.StatusOK, userResponse(user))
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, errMethodNotAllowed())
		return
	}
	if _, err := a.requireUserFor(r.Context(), r, "log out"); err != nil {
		writeError(w, err)
		return
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err == nil && cookie.Value != "" {
		_, _ = a.db.ExecContext(r.Context(), `DELETE FROM sessions WHERE token_hash = ?`, hashSecret(cookie.Value))
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   a.cookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errMethodNotAllowed())
		return
	}
	user, err := a.requireUserFor(r.Context(), r, "view current user")
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, userResponse(user))
}

func (a *App) userByEmail(ctx context.Context, email string) (currentUser, string, error) {
	var user currentUser
	var passwordHash string
	err := a.db.QueryRowContext(ctx, `
		SELECT id, email, display_name, password_hash, email_verified_at, status
		FROM users
		WHERE email = ?
	`, email).Scan(&user.ID, &user.Email, &user.DisplayName, &passwordHash, &user.EmailVerifiedAt, &user.Status)
	return user, passwordHash, err
}

func (a *App) requireUser(ctx context.Context, r *http.Request) (currentUser, error) {
	return a.requireUserFor(ctx, r, "")
}

func (a *App) requireUserFor(ctx context.Context, r *http.Request, action string) (currentUser, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return currentUser{}, errLoginRequired(action)
	}
	var user currentUser
	err = a.db.QueryRowContext(ctx, `
		SELECT u.id, u.email, u.display_name, u.email_verified_at, u.status
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ?
			AND s.expires_at > ?
			AND u.status = 'active'
	`, hashSecret(cookie.Value), a.now().UTC().Format(time.RFC3339Nano)).
		Scan(&user.ID, &user.Email, &user.DisplayName, &user.EmailVerifiedAt, &user.Status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return currentUser{}, errLoginRequired(action)
		}
		return currentUser{}, err
	}
	return user, nil
}

func userResponse(user currentUser) map[string]any {
	return map[string]any{
		"id":             user.ID,
		"email":          user.Email,
		"display_name":   user.DisplayName,
		"email_verified": user.EmailVerifiedAt.Valid,
	}
}
