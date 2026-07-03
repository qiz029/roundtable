package roundtable

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
)

func (a *App) handleMyAgents(w http.ResponseWriter, r *http.Request) {
	user, err := a.requireUser(r.Context(), r)
	if err != nil {
		writeError(w, err)
		return
	}

	switch r.Method {
	case http.MethodPost:
		a.createAgent(w, r, user)
	case http.MethodGet:
		a.listMyAgents(w, r, user)
	default:
		writeError(w, errMethodNotAllowed())
	}
}

func (a *App) handleMyAgent(w http.ResponseWriter, r *http.Request) {
	user, err := a.requireUser(r.Context(), r)
	if err != nil {
		writeError(w, err)
		return
	}

	agentID, action, ok := twoPartAction(r.URL.Path, "/api/v1/me/agents/")
	if !ok || action != "token" {
		writeError(w, errNotFound("agent action not found"))
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, errMethodNotAllowed())
		return
	}
	a.resetAgentToken(w, r, user, agentID)
}

func (a *App) createAgent(w http.ResponseWriter, r *http.Request, user currentUser) {
	if !user.EmailVerifiedAt.Valid {
		writeError(w, errForbidden("email verification required"))
		return
	}

	var req struct {
		Name         string   `json:"name"`
		Description  string   `json:"description"`
		Tags         []string `json:"tags"`
		Capabilities []string `json:"capabilities"`
		Instructions string   `json:"instructions"`
		HomepageURL  string   `json:"homepage_url"`
		IsPublic     bool     `json:"is_public"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, errInvalidInput("name is required"))
		return
	}
	tags, err := encodeStringList(req.Tags)
	if err != nil {
		writeError(w, err)
		return
	}
	capabilities, err := encodeStringList(req.Capabilities)
	if err != nil {
		writeError(w, err)
		return
	}
	agentID, err := newID("agt")
	if err != nil {
		writeError(w, err)
		return
	}
	agentToken, err := newSecret("rt_agent")
	if err != nil {
		writeError(w, err)
		return
	}
	now := a.now().UTC().Format(time.RFC3339Nano)

	_, err = a.db.ExecContext(r.Context(), `
		INSERT INTO agents (
			id, owner_user_id, name, description, tags_json, capabilities_json,
			instructions, homepage_url, is_public, token_hash, created_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, agentID, user.ID, name, strings.TrimSpace(req.Description), tags, capabilities,
		strings.TrimSpace(req.Instructions), strings.TrimSpace(req.HomepageURL),
		boolToInt(req.IsPublic), hashSecret(agentToken), now)
	if err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":           agentID,
		"name":         name,
		"description":  strings.TrimSpace(req.Description),
		"tags":         decodeStringList(tags),
		"capabilities": decodeStringList(capabilities),
		"is_public":    req.IsPublic,
		"token":        agentToken,
	})
}

func (a *App) listMyAgents(w http.ResponseWriter, r *http.Request, user currentUser) {
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT id, name, description, tags_json, capabilities_json, is_public, status, created_at
		FROM agents
		WHERE owner_user_id = ?
		ORDER BY created_at DESC
	`, user.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	defer rows.Close()

	items := []map[string]any{}
	for rows.Next() {
		var id, name, description, tagsRaw, capabilitiesRaw, status, createdAt string
		var isPublic int
		if err := rows.Scan(&id, &name, &description, &tagsRaw, &capabilitiesRaw, &isPublic, &status, &createdAt); err != nil {
			writeError(w, err)
			return
		}
		items = append(items, map[string]any{
			"id":           id,
			"name":         name,
			"description":  description,
			"tags":         decodeStringList(tagsRaw),
			"capabilities": decodeStringList(capabilitiesRaw),
			"is_public":    isPublic == 1,
			"status":       status,
			"created_at":   createdAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) resetAgentToken(w http.ResponseWriter, r *http.Request, user currentUser, agentID string) {
	agentToken, err := newSecret("rt_agent")
	if err != nil {
		writeError(w, err)
		return
	}

	result, err := a.db.ExecContext(r.Context(), `
		UPDATE agents
		SET token_hash = ?
		WHERE id = ? AND owner_user_id = ?
	`, hashSecret(agentToken), agentID, user.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		writeError(w, errNotFound("agent not found"))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":    agentID,
		"token": agentToken,
	})
}

func (a *App) requireAgent(ctx context.Context, r *http.Request) (currentAgent, error) {
	token, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		return currentAgent{}, errUnauthorized()
	}
	var agent currentAgent
	err := a.db.QueryRowContext(ctx, `
		SELECT ag.id, ag.owner_user_id, ag.name, ag.description
		FROM agents ag
		JOIN users u ON u.id = ag.owner_user_id
		WHERE ag.token_hash = ?
			AND ag.status = 'active'
			AND u.status = 'active'
			AND u.email_verified_at IS NOT NULL
	`, hashSecret(token)).Scan(&agent.ID, &agent.OwnerID, &agent.Name, &agent.Description)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return currentAgent{}, errUnauthorized()
		}
		return currentAgent{}, err
	}
	return agent, nil
}
