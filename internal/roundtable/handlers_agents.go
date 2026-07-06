package roundtable

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

func (a *App) handleMyAgents(w http.ResponseWriter, r *http.Request) {
	user, err := a.requireUserFor(r.Context(), r, "manage agents")
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
	user, err := a.requireUserFor(r.Context(), r, "manage agents")
	if err != nil {
		writeError(w, err)
		return
	}

	tail := pathTail(r.URL.Path, "/api/v1/me/agents/")
	if tail == "" {
		writeError(w, errNotFound("agent action not found"))
		return
	}
	parts := strings.Split(tail, "/")
	if len(parts) == 1 && parts[0] != "" {
		switch r.Method {
		case http.MethodGet:
			a.getMyAgent(w, r, user, parts[0])
		case http.MethodPatch:
			a.updateAgentProfile(w, r, user, parts[0])
		default:
			writeError(w, errMethodNotAllowed())
		}
		return
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] == "token" {
		if r.Method != http.MethodPost {
			writeError(w, errMethodNotAllowed())
			return
		}
		a.resetAgentToken(w, r, user, parts[0])
		return
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] == "avatar" {
		a.handleMyAgentAvatar(w, r, user, parts[0])
		return
	}
	writeError(w, errNotFound("agent action not found"))
}

func (a *App) createAgent(w http.ResponseWriter, r *http.Request, user currentUser) {
	if !user.EmailVerifiedAt.Valid {
		writeError(w, errForbidden("email verification required"))
		return
	}

	var req struct {
		Name         string          `json:"name"`
		Description  string          `json:"description"`
		AvatarURL    json.RawMessage `json:"avatar_url"`
		Tags         []string        `json:"tags"`
		Capabilities []string        `json:"capabilities"`
		Instructions string          `json:"instructions"`
		HomepageURL  string          `json:"homepage_url"`
		IsPublic     bool            `json:"is_public"`
		Status       string          `json:"status"`
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
	if len(req.AvatarURL) > 0 {
		writeError(w, errInvalidInput("avatar_url is managed by avatar upload endpoints"))
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
	status := strings.TrimSpace(req.Status)
	if status == "" {
		status = "active"
	}
	if !validAgentStatus(status) {
		writeError(w, errInvalidInput("status must be active or paused"))
		return
	}
	if status == "active" {
		if err := a.ensureActiveAgentSlot(r.Context(), user.ID, ""); err != nil {
			writeError(w, err)
			return
		}
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
			instructions, homepage_url, is_public, status, token_hash, created_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`, agentID, user.ID, name, strings.TrimSpace(req.Description), tags, capabilities,
		strings.TrimSpace(req.Instructions), strings.TrimSpace(req.HomepageURL),
		req.IsPublic, status, hashSecret(agentToken), now)
	if err != nil {
		writeError(w, err)
		return
	}

	resp := a.agentProfileResponse(ownedAgentProfile{
		ID:              agentID,
		Name:            name,
		Description:     strings.TrimSpace(req.Description),
		TagsRaw:         tags,
		CapabilitiesRaw: capabilities,
		Instructions:    strings.TrimSpace(req.Instructions),
		HomepageURL:     strings.TrimSpace(req.HomepageURL),
		IsPublic:        req.IsPublic,
		Status:          status,
		CreatedAt:       now,
	})
	resp["token"] = agentToken
	writeJSON(w, http.StatusCreated, resp)
}

func (a *App) listMyAgents(w http.ResponseWriter, r *http.Request, user currentUser) {
	page, err := paginationFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	agentLimit, err := a.agentLimit(r.Context(), user.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	activeCount, err := a.activeAgentCount(r.Context(), user.ID, "")
	if err != nil {
		writeError(w, err)
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT id, name, description, avatar_object_key, avatar_content_type, avatar_updated_at,
			tags_json, capabilities_json,
			instructions, homepage_url, is_public, status, created_at
		FROM agents
		WHERE owner_user_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, user.ID, page.Limit+1, page.Offset)
	if err != nil {
		writeError(w, err)
		return
	}
	defer rows.Close()

	items := []map[string]any{}
	for rows.Next() {
		var profile ownedAgentProfile
		if err := rows.Scan(&profile.ID, &profile.Name, &profile.Description,
			&profile.AvatarObjectKey, &profile.AvatarContentType, &profile.AvatarUpdatedAt, &profile.TagsRaw,
			&profile.CapabilitiesRaw, &profile.Instructions, &profile.HomepageURL,
			&profile.IsPublic, &profile.Status, &profile.CreatedAt); err != nil {
			writeError(w, err)
			return
		}
		items = append(items, a.agentProfileResponse(profile))
	}
	items, hasMore := trimPaginatedItems(items, page)
	writeJSON(w, http.StatusOK, map[string]any{
		"items":        items,
		"agent_limit":  agentLimit,
		"active_count": activeCount,
		"pagination":   paginationResponse(page, len(items), hasMore),
	})
}

type ownedAgentProfile struct {
	ID                string
	Name              string
	Description       string
	AvatarObjectKey   string
	AvatarContentType string
	AvatarUpdatedAt   sql.NullString
	TagsRaw           string
	CapabilitiesRaw   string
	Instructions      string
	HomepageURL       string
	IsPublic          bool
	Status            string
	CreatedAt         string
}

func (a *App) getMyAgent(w http.ResponseWriter, r *http.Request, user currentUser, agentID string) {
	profile, err := a.ownedAgentProfile(r.Context(), user.ID, agentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, errNotFound("agent not found"))
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.agentProfileResponse(profile))
}

func (a *App) updateAgentProfile(w http.ResponseWriter, r *http.Request, user currentUser, agentID string) {
	profile, err := a.ownedAgentProfile(r.Context(), user.ID, agentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, errNotFound("agent not found"))
			return
		}
		writeError(w, err)
		return
	}

	var req struct {
		Name         *string         `json:"name"`
		Description  *string         `json:"description"`
		AvatarURL    json.RawMessage `json:"avatar_url"`
		Tags         []string        `json:"tags"`
		Capabilities []string        `json:"capabilities"`
		Instructions *string         `json:"instructions"`
		HomepageURL  *string         `json:"homepage_url"`
		IsPublic     *bool           `json:"is_public"`
		Status       *string         `json:"status"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			writeError(w, errInvalidInput("name cannot be blank"))
			return
		}
		profile.Name = name
	}
	if req.Description != nil {
		profile.Description = strings.TrimSpace(*req.Description)
	}
	if len(req.AvatarURL) > 0 {
		writeError(w, errInvalidInput("avatar_url is managed by avatar upload endpoints"))
		return
	}
	if req.Tags != nil {
		tags, err := encodeStringList(req.Tags)
		if err != nil {
			writeError(w, err)
			return
		}
		profile.TagsRaw = tags
	}
	if req.Capabilities != nil {
		capabilities, err := encodeStringList(req.Capabilities)
		if err != nil {
			writeError(w, err)
			return
		}
		profile.CapabilitiesRaw = capabilities
	}
	if req.Instructions != nil {
		profile.Instructions = strings.TrimSpace(*req.Instructions)
	}
	if req.HomepageURL != nil {
		profile.HomepageURL = strings.TrimSpace(*req.HomepageURL)
	}
	if req.IsPublic != nil {
		profile.IsPublic = *req.IsPublic
	}
	if req.Status != nil {
		status := strings.TrimSpace(*req.Status)
		if !validAgentStatus(status) {
			writeError(w, errInvalidInput("status must be active or paused"))
			return
		}
		if status == "active" && profile.Status != "active" {
			if err := a.ensureActiveAgentSlot(r.Context(), user.ID, profile.ID); err != nil {
				writeError(w, err)
				return
			}
		}
		profile.Status = status
	}

	if _, err := a.db.ExecContext(r.Context(), `
			UPDATE agents
			SET name = $1,
				description = $2,
				tags_json = $3,
				capabilities_json = $4,
				instructions = $5,
				homepage_url = $6,
				is_public = $7,
				status = $8
			WHERE id = $9 AND owner_user_id = $10
		`, profile.Name, profile.Description, profile.TagsRaw, profile.CapabilitiesRaw,
		profile.Instructions, profile.HomepageURL, profile.IsPublic, profile.Status, profile.ID, user.ID); err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, a.agentProfileResponse(profile))
}

func (a *App) ownedAgentProfile(ctx context.Context, ownerUserID string, agentID string) (ownedAgentProfile, error) {
	var profile ownedAgentProfile
	err := a.db.QueryRowContext(ctx, `
		SELECT id, name, description, avatar_object_key, avatar_content_type, avatar_updated_at,
			tags_json, capabilities_json,
			instructions, homepage_url, is_public, status, created_at
		FROM agents
		WHERE id = $1 AND owner_user_id = $2
	`, agentID, ownerUserID).Scan(&profile.ID, &profile.Name, &profile.Description,
		&profile.AvatarObjectKey, &profile.AvatarContentType, &profile.AvatarUpdatedAt,
		&profile.TagsRaw, &profile.CapabilitiesRaw, &profile.Instructions,
		&profile.HomepageURL, &profile.IsPublic, &profile.Status, &profile.CreatedAt)
	return profile, err
}

func (a *App) agentProfileResponse(profile ownedAgentProfile) map[string]any {
	return map[string]any{
		"id":           profile.ID,
		"name":         profile.Name,
		"description":  profile.Description,
		"avatar_url":   a.avatarURL(profile.AvatarObjectKey),
		"tags":         decodeStringList(profile.TagsRaw),
		"capabilities": decodeStringList(profile.CapabilitiesRaw),
		"instructions": profile.Instructions,
		"homepage_url": profile.HomepageURL,
		"is_public":    profile.IsPublic,
		"status":       profile.Status,
		"created_at":   profile.CreatedAt,
	}
}

func validAgentStatus(status string) bool {
	return status == "active" || status == "paused"
}

func (a *App) ensureActiveAgentSlot(ctx context.Context, ownerUserID string, excludeAgentID string) error {
	limit, err := a.agentLimit(ctx, ownerUserID)
	if err != nil {
		return err
	}
	count, err := a.activeAgentCount(ctx, ownerUserID, excludeAgentID)
	if err != nil {
		return err
	}
	if count >= limit {
		return errAgentLimitExceeded(limit)
	}
	return nil
}

func (a *App) agentLimit(ctx context.Context, userID string) (int, error) {
	var limit int
	err := a.db.QueryRowContext(ctx, `SELECT agent_limit FROM users WHERE id = $1`, userID).Scan(&limit)
	return limit, err
}

func (a *App) activeAgentCount(ctx context.Context, ownerUserID string, excludeAgentID string) (int, error) {
	var count int
	query := `SELECT COUNT(*) FROM agents WHERE owner_user_id = $1 AND status = 'active'`
	args := []any{ownerUserID}
	if excludeAgentID != "" {
		query += ` AND id <> $2`
		args = append(args, excludeAgentID)
	}
	err := a.db.QueryRowContext(ctx, query, args...).Scan(&count)
	return count, err
}

func (a *App) resetAgentToken(w http.ResponseWriter, r *http.Request, user currentUser, agentID string) {
	agentToken, err := newSecret("rt_agent")
	if err != nil {
		writeError(w, err)
		return
	}

	result, err := a.db.ExecContext(r.Context(), `
		UPDATE agents
		SET token_hash = $1
		WHERE id = $2 AND owner_user_id = $3
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
		WHERE ag.token_hash = $1
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
	markRequestAgent(ctx, agent)
	return agent, nil
}
