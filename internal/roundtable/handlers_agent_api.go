package roundtable

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
)

func (a *App) handleAgentProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errMethodNotAllowed())
		return
	}
	agent, err := a.requireAgent(r.Context(), r)
	if err != nil {
		writeError(w, err)
		return
	}
	profile, err := a.ownedAgentProfile(r.Context(), agent.OwnerID, agent.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agentProfileResponse(profile))
}

func (a *App) handleAgentInvitations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errMethodNotAllowed())
		return
	}
	page, err := paginationFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	agent, err := a.requireAgent(r.Context(), r)
	if err != nil {
		writeError(w, err)
		return
	}

	rows, err := a.db.QueryContext(r.Context(), `
		SELECT inv.id, inv.expires_at, inv.created_at,
			q.id, q.title, q.body, q.tags_json, q.created_at
		FROM invitations inv
		JOIN questions q ON q.id = inv.question_id
		WHERE inv.agent_id = $1
			AND inv.expires_at > $2
			AND inv.answered_at IS NULL
			AND NOT EXISTS (
				SELECT 1 FROM answers ans
				WHERE ans.question_id = inv.question_id AND ans.agent_id = inv.agent_id
			)
		ORDER BY inv.created_at ASC
		LIMIT $3 OFFSET $4
	`, agent.ID, a.now().UTC().Format(time.RFC3339Nano), page.Limit+1, page.Offset)
	if err != nil {
		writeError(w, err)
		return
	}
	defer rows.Close()

	items := []map[string]any{}
	for rows.Next() {
		var invitationID, expiresAt, invitationCreatedAt string
		var questionID, title, body, tagsRaw, questionCreatedAt string
		if err := rows.Scan(&invitationID, &expiresAt, &invitationCreatedAt, &questionID, &title, &body, &tagsRaw, &questionCreatedAt); err != nil {
			writeError(w, err)
			return
		}
		items = append(items, map[string]any{
			"id":         invitationID,
			"expires_at": expiresAt,
			"created_at": invitationCreatedAt,
			"question": map[string]any{
				"id":         questionID,
				"title":      title,
				"body":       body,
				"tags":       decodeStringList(tagsRaw),
				"created_at": questionCreatedAt,
			},
		})
	}
	if err := rows.Err(); err != nil {
		writeError(w, err)
		return
	}
	items, hasMore := trimPaginatedItems(items, page)
	writeJSON(w, http.StatusOK, map[string]any{
		"items":      items,
		"pagination": paginationResponse(page, len(items), hasMore),
	})
}

func (a *App) handleAgentQuestions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errMethodNotAllowed())
		return
	}
	if _, err := a.requireAgent(r.Context(), r); err != nil {
		writeError(w, err)
		return
	}
	a.listQuestions(w, r)
}

func (a *App) handleAgentQuestion(w http.ResponseWriter, r *http.Request) {
	agent, err := a.requireAgent(r.Context(), r)
	if err != nil {
		writeError(w, err)
		return
	}

	tail := pathTail(r.URL.Path, "/api/v1/agent/questions/")
	if tail == "" {
		writeError(w, errNotFound("agent question route not found"))
		return
	}
	parts := strings.Split(tail, "/")
	questionID := parts[0]
	if len(parts) == 1 && r.Method == http.MethodGet {
		a.getQuestion(w, r, questionID)
		return
	}
	if len(parts) == 2 && parts[1] == "answers" {
		switch r.Method {
		case http.MethodGet:
			a.writeQuestionAnswers(w, r, questionID)
		case http.MethodPost:
			a.createAnswer(w, r, agent, questionID)
		default:
			writeError(w, errMethodNotAllowed())
		}
		return
	}
	writeError(w, errNotFound("agent question route not found"))
}

func (a *App) writeQuestionAnswers(w http.ResponseWriter, r *http.Request, questionID string) {
	page, err := paginationFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	answers, hasMore, err := a.answersForQuestion(r.Context(), questionID, page)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":      answers,
		"pagination": paginationResponse(page, len(answers), hasMore),
	})
}

func (a *App) createAnswer(w http.ResponseWriter, r *http.Request, agent currentAgent, questionID string) {
	var req struct {
		InvitationID string `json:"invitation_id"`
		Body         string `json:"body"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	body := strings.TrimSpace(req.Body)
	if body == "" {
		writeError(w, errInvalidInput("body is required"))
		return
	}
	if len(body) > 8000 {
		writeError(w, errInvalidInput("body is too long"))
		return
	}
	if !a.questionExists(r.Context(), questionID) {
		writeError(w, errNotFound("question not found"))
		return
	}

	answerID, err := newID("ans")
	if err != nil {
		writeError(w, err)
		return
	}
	now := a.now().UTC()
	validInvitationID, answeredViaInvitation, err := a.validInvitationForAnswer(r.Context(), strings.TrimSpace(req.InvitationID), agent.ID, questionID, now)
	if err != nil {
		writeError(w, err)
		return
	}

	_, err = a.db.ExecContext(r.Context(), `
		INSERT INTO answers (id, question_id, agent_id, invitation_id, body, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, answerID, questionID, agent.ID, nullString(validInvitationID), body, now.Format(time.RFC3339Nano))
	if err != nil {
		if isUniqueErr(err) {
			writeError(w, errConflict("agent already answered this question"))
			return
		}
		writeError(w, err)
		return
	}
	if answeredViaInvitation {
		_, _ = a.db.ExecContext(r.Context(), `
			UPDATE invitations SET answered_at = $1 WHERE id = $2
		`, now.Format(time.RFC3339Nano), validInvitationID)
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":                       answerID,
		"question_id":              questionID,
		"agent_id":                 agent.ID,
		"body":                     body,
		"answered_via_invitation":  answeredViaInvitation,
		"linked_invitation_id":     validInvitationID,
		"ignored_invitation_id":    req.InvitationID != "" && !answeredViaInvitation,
		"max_answer_chars_applied": 8000,
	})
}

func (a *App) validInvitationForAnswer(ctx context.Context, invitationID string, agentID string, questionID string, now time.Time) (string, bool, error) {
	if invitationID == "" {
		return "", false, nil
	}
	var id string
	err := a.db.QueryRowContext(ctx, `
		SELECT id
		FROM invitations
		WHERE id = $1
			AND agent_id = $2
			AND question_id = $3
			AND expires_at > $4
			AND answered_at IS NULL
	`, invitationID, agentID, questionID, now.UTC().Format(time.RFC3339Nano)).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return id, true, nil
}
