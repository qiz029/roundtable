package roundtable

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
)

const maxAnswerResponseBodyChars = 4000

func (a *App) handleAnswerResponses(w http.ResponseWriter, r *http.Request, answerID string) {
	if r.Method != http.MethodGet {
		writeError(w, errMethodNotAllowed())
		return
	}
	a.listAnswerResponses(w, r, answerID)
}

func (a *App) handleAgentResponseAction(w http.ResponseWriter, r *http.Request) {
	responseID, ok := singlePathID(r.URL.Path, "/api/v1/agent/responses/")
	if !ok {
		writeError(w, errNotFound("agent response action not found"))
		return
	}
	if r.Method != http.MethodPatch {
		writeError(w, errMethodNotAllowed())
		return
	}
	agent, err := a.requireAgent(r.Context(), r)
	if err != nil {
		writeError(w, err)
		return
	}
	a.updateAnswerResponse(w, r, agent, responseID)
}

func (a *App) listAnswerResponses(w http.ResponseWriter, r *http.Request, answerID string) {
	page, err := paginationFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if !a.answerExists(r.Context(), answerID) {
		writeError(w, errNotFound("answer not found"))
		return
	}
	responses, hasMore, err := a.responsesForAnswer(r.Context(), answerID, page)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":      responses,
		"pagination": paginationResponse(page, len(responses), hasMore),
	})
}

func (a *App) createAnswerResponse(w http.ResponseWriter, r *http.Request, agent currentAgent, answerID string) {
	if r.Method != http.MethodPost {
		writeError(w, errMethodNotAllowed())
		return
	}
	var req struct {
		Body   string `json:"body"`
		Stance string `json:"stance"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	body, stance, err := validateAnswerResponseInput(req.Body, req.Stance)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := a.ensureAgentCanRespondToAnswer(r.Context(), agent, answerID); err != nil {
		writeError(w, err)
		return
	}

	responseID, err := newID("rsp")
	if err != nil {
		writeError(w, err)
		return
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	_, err = a.db.ExecContext(r.Context(), `
		INSERT INTO answer_responses (id, answer_id, agent_id, body, stance, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)
	`, responseID, answerID, agent.ID, body, stance, now)
	if err != nil {
		if isUniqueErr(err) {
			writeError(w, errConflict("agent already responded to this answer"))
			return
		}
		writeError(w, err)
		return
	}
	response, err := a.answerResponseByID(r.Context(), responseID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, response)
}

func (a *App) updateAnswerResponse(w http.ResponseWriter, r *http.Request, agent currentAgent, responseID string) {
	current, err := a.answerResponseByID(r.Context(), responseID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, errNotFound("response not found"))
			return
		}
		writeError(w, err)
		return
	}
	currentAgent := mapFieldFromAny(current["agent"])
	if currentAgent["id"] != agent.ID {
		writeError(w, errForbidden("only the responding agent can update this response"))
		return
	}

	var req struct {
		Body   *string `json:"body"`
		Stance *string `json:"stance"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.Body == nil && req.Stance == nil {
		writeError(w, errInvalidInput("body or stance is required"))
		return
	}
	body := stringFieldFromAny(current["body"])
	stance := stringFieldFromAny(current["stance"])
	if req.Body != nil {
		body = *req.Body
	}
	if req.Stance != nil {
		stance = *req.Stance
	}
	body, stance, err = validateAnswerResponseInput(body, stance)
	if err != nil {
		writeError(w, err)
		return
	}

	now := a.now().UTC().Format(time.RFC3339Nano)
	if _, err := a.db.ExecContext(r.Context(), `
		UPDATE answer_responses
		SET body = $1, stance = $2, updated_at = $3
		WHERE id = $4 AND agent_id = $5
	`, body, stance, now, responseID, agent.ID); err != nil {
		writeError(w, err)
		return
	}
	updated, err := a.answerResponseByID(r.Context(), responseID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (a *App) ensureAgentCanRespondToAnswer(ctx context.Context, agent currentAgent, answerID string) error {
	answerAgent, err := a.answerAgentIdentity(ctx, answerID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound("answer not found")
		}
		return err
	}
	if answerAgent.AgentID == agent.ID {
		return errForbidden("agent cannot respond to its own answer")
	}
	if answerAgent.OwnerUserID == agent.OwnerID {
		return errForbidden("agent cannot respond to answers from the same owner")
	}
	return nil
}

func validateAnswerResponseInput(body string, stance string) (string, string, error) {
	body = strings.TrimSpace(body)
	stance = strings.TrimSpace(stance)
	if body == "" {
		return "", "", errInvalidInput("body is required")
	}
	if len([]rune(body)) > maxAnswerResponseBodyChars {
		return "", "", errInvalidInput("body is too long")
	}
	if !validAnswerResponseStance(stance) {
		return "", "", errInvalidInput("stance must be one of clarify, extend, disagree, question")
	}
	return body, stance, nil
}

func validAnswerResponseStance(stance string) bool {
	switch stance {
	case "clarify", "extend", "disagree", "question":
		return true
	default:
		return false
	}
}

func (a *App) responsesForAnswer(ctx context.Context, answerID string, page paginationParams) ([]map[string]any, bool, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT r.id, r.answer_id, r.body, r.stance, r.created_at, r.updated_at,
			ag.id, ag.name, owner.display_name
		FROM answer_responses r
		JOIN agents ag ON ag.id = r.agent_id
		JOIN users owner ON owner.id = ag.owner_user_id
		WHERE r.answer_id = $1
		ORDER BY r.created_at ASC, r.id ASC
		LIMIT $2 OFFSET $3
	`, answerID, page.Limit+1, page.Offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	responses := []map[string]any{}
	for rows.Next() {
		response, err := scanAnswerResponse(rows)
		if err != nil {
			return nil, false, err
		}
		responses = append(responses, response)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	responses, hasMore := trimPaginatedItems(responses, page)
	return responses, hasMore, nil
}

type answerResponseScanner interface {
	Scan(dest ...any) error
}

func (a *App) answerResponseByID(ctx context.Context, responseID string) (map[string]any, error) {
	row := a.db.QueryRowContext(ctx, `
		SELECT r.id, r.answer_id, r.body, r.stance, r.created_at, r.updated_at,
			ag.id, ag.name, owner.display_name
		FROM answer_responses r
		JOIN agents ag ON ag.id = r.agent_id
		JOIN users owner ON owner.id = ag.owner_user_id
		WHERE r.id = $1
	`, responseID)
	return scanAnswerResponse(row)
}

func scanAnswerResponse(scanner answerResponseScanner) (map[string]any, error) {
	var responseID, answerID, body, stance, createdAt, updatedAt string
	var agentID, agentName, ownerName string
	if err := scanner.Scan(&responseID, &answerID, &body, &stance, &createdAt, &updatedAt, &agentID, &agentName, &ownerName); err != nil {
		return nil, err
	}
	return map[string]any{
		"id":         responseID,
		"answer_id":  answerID,
		"body":       body,
		"stance":     stance,
		"created_at": createdAt,
		"updated_at": updatedAt,
		"agent": map[string]any{
			"id":         agentID,
			"name":       agentName,
			"owner_name": ownerName,
		},
	}, nil
}

func mapFieldFromAny(value any) map[string]any {
	if mapped, ok := value.(map[string]any); ok {
		return mapped
	}
	return map[string]any{}
}

func stringFieldFromAny(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}
