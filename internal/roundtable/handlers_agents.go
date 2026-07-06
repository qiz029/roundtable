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

type publicAgentProfile struct {
	ID              string
	Name            string
	Description     string
	AvatarObjectKey string
	OwnerName       string
	TagsRaw         string
	CapabilitiesRaw string
	HomepageURL     string
	IsPublic        bool
	Status          string
	CreatedAt       string
	AnswerCount     int
}

func (a *App) handlePublicAgent(w http.ResponseWriter, r *http.Request) {
	tail := pathTail(r.URL.Path, "/api/v1/agents/")
	if tail == "" {
		writeError(w, errNotFound("agent action not found"))
		return
	}
	parts := strings.Split(tail, "/")
	if len(parts) == 1 && parts[0] != "" {
		if r.Method != http.MethodGet {
			writeError(w, errMethodNotAllowed())
			return
		}
		a.getPublicAgent(w, r, parts[0])
		return
	}
	if len(parts) == 2 && parts[0] != "" {
		switch parts[1] {
		case "answers":
			if r.Method != http.MethodGet {
				writeError(w, errMethodNotAllowed())
				return
			}
			a.listPublicAgentAnswers(w, r, parts[0])
			return
		case "scores":
			a.handlePublicAgentScore(w, r, parts[0])
			return
		}
	}
	writeError(w, errNotFound("agent action not found"))
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

func (a *App) publicAgentProfile(ctx context.Context, agentID string) (publicAgentProfile, error) {
	var profile publicAgentProfile
	err := a.db.QueryRowContext(ctx, `
		SELECT ag.id, ag.name, ag.description, ag.avatar_object_key, owner.display_name,
			ag.tags_json, ag.capabilities_json, ag.homepage_url, ag.is_public, ag.status, ag.created_at,
			(
				SELECT COUNT(*)
				FROM answers ans
				JOIN questions q ON q.id = ans.question_id
				JOIN users question_author ON question_author.id = q.author_user_id
				WHERE ans.agent_id = ag.id
					AND question_author.status = 'active'
			) AS answer_count
		FROM agents ag
		JOIN users owner ON owner.id = ag.owner_user_id
		WHERE ag.id = $1
			AND ag.is_public = TRUE
			AND owner.status = 'active'
	`, agentID).Scan(&profile.ID, &profile.Name, &profile.Description, &profile.AvatarObjectKey,
		&profile.OwnerName, &profile.TagsRaw, &profile.CapabilitiesRaw, &profile.HomepageURL,
		&profile.IsPublic, &profile.Status, &profile.CreatedAt, &profile.AnswerCount)
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

func (a *App) publicAgentProfileResponse(profile publicAgentProfile) map[string]any {
	return map[string]any{
		"id":           profile.ID,
		"name":         profile.Name,
		"description":  profile.Description,
		"avatar_url":   a.avatarURL(profile.AvatarObjectKey),
		"owner_name":   profile.OwnerName,
		"tags":         decodeStringList(profile.TagsRaw),
		"capabilities": decodeStringList(profile.CapabilitiesRaw),
		"homepage_url": profile.HomepageURL,
		"is_public":    profile.IsPublic,
		"status":       profile.Status,
		"created_at":   profile.CreatedAt,
		"answer_count": profile.AnswerCount,
	}
}

func (a *App) getPublicAgent(w http.ResponseWriter, r *http.Request, agentID string) {
	profile, err := a.publicAgentProfile(r.Context(), agentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, errNotFound("agent not found"))
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.publicAgentProfileResponse(profile))
}

func (a *App) listPublicAgentAnswers(w http.ResponseWriter, r *http.Request, agentID string) {
	if _, err := a.publicAgentProfile(r.Context(), agentID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, errNotFound("agent not found"))
			return
		}
		writeError(w, err)
		return
	}
	page, err := paginationFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	items, hasMore, err := a.publicAgentAnswers(r.Context(), agentID, page)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":      items,
		"pagination": paginationResponse(page, len(items), hasMore),
	})
}

func (a *App) publicAgentAnswers(ctx context.Context, agentID string, page paginationParams) ([]map[string]any, bool, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT q.id, q.title, q.body, q.tags_json, q.created_at, question_author.display_name,
			(SELECT COUNT(*) FROM answers qans WHERE qans.question_id = q.id) AS answer_count,
			ans.id, ans.body, ans.created_at,
			COALESCE(SUM(v.value), 0) AS like_count,
			(SELECT COUNT(*) FROM answer_comments c WHERE c.answer_id = ans.id AND c.deleted_at IS NULL) AS comment_count,
			ag.id, ag.name, ag.avatar_object_key, owner.display_name
		FROM answers ans
		JOIN questions q ON q.id = ans.question_id
		JOIN users question_author ON question_author.id = q.author_user_id
		JOIN agents ag ON ag.id = ans.agent_id
		JOIN users owner ON owner.id = ag.owner_user_id
		LEFT JOIN votes v ON v.answer_id = ans.id AND v.revoked_at IS NULL
		WHERE ag.id = $1
			AND ag.is_public = TRUE
			AND question_author.status = 'active'
			AND owner.status = 'active'
		GROUP BY q.id, q.title, q.body, q.tags_json, q.created_at, question_author.display_name,
			ans.id, ans.body, ans.created_at, ag.id, ag.name, ag.avatar_object_key, owner.display_name
		ORDER BY ans.created_at DESC, ans.id ASC
		LIMIT $2 OFFSET $3
	`, agentID, page.Limit+1, page.Offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	items := []map[string]any{}
	for rows.Next() {
		var questionID, questionTitle, questionBody, questionTagsRaw, questionCreatedAt, questionAuthorName string
		var questionAnswerCount int
		var answerID, answerBody, answerCreatedAt string
		var likeCount, commentCount int
		var answerAgentID, answerAgentName, answerAgentAvatarObjectKey, answerAgentOwnerName string
		if err := rows.Scan(&questionID, &questionTitle, &questionBody, &questionTagsRaw, &questionCreatedAt, &questionAuthorName,
			&questionAnswerCount, &answerID, &answerBody, &answerCreatedAt, &likeCount, &commentCount,
			&answerAgentID, &answerAgentName, &answerAgentAvatarObjectKey, &answerAgentOwnerName); err != nil {
			return nil, false, err
		}
		items = append(items, map[string]any{
			"question": map[string]any{
				"id":           questionID,
				"title":        questionTitle,
				"body":         questionBody,
				"tags":         decodeStringList(questionTagsRaw),
				"created_at":   questionCreatedAt,
				"author_name":  questionAuthorName,
				"answer_count": questionAnswerCount,
			},
			"answer": map[string]any{
				"id":            answerID,
				"body":          answerBody,
				"created_at":    answerCreatedAt,
				"like_count":    likeCount,
				"comment_count": commentCount,
				"agent":         a.agentIdentityResponse(answerAgentID, answerAgentName, answerAgentOwnerName, answerAgentAvatarObjectKey),
			},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	items, hasMore := trimPaginatedItems(items, page)
	return items, hasMore, nil
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
