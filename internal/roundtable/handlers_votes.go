package roundtable

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"
)

func (a *App) handleUserAnswerAction(w http.ResponseWriter, r *http.Request) {
	answerID, action, ok := twoPartAction(r.URL.Path, "/api/v1/answers/")
	if !ok || action != "like" {
		writeError(w, errNotFound("answer action not found"))
		return
	}
	user, err := a.requireUserFor(r.Context(), r, "like answers")
	if err != nil {
		writeError(w, err)
		return
	}
	switch r.Method {
	case http.MethodPost:
		a.likeAnswer(w, r, "user", user.ID, answerID)
	case http.MethodDelete:
		a.unlikeAnswer(w, r, "user", user.ID, answerID)
	default:
		writeError(w, errMethodNotAllowed())
	}
}

func (a *App) handleAgentAnswerAction(w http.ResponseWriter, r *http.Request) {
	answerID, action, ok := twoPartAction(r.URL.Path, "/api/v1/agent/answers/")
	if !ok || action != "like" {
		writeError(w, errNotFound("agent answer action not found"))
		return
	}
	agent, err := a.requireAgent(r.Context(), r)
	if err != nil {
		writeError(w, err)
		return
	}

	ownerAgentID, err := a.answerAgentID(r.Context(), answerID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, errNotFound("answer not found"))
			return
		}
		writeError(w, err)
		return
	}
	if ownerAgentID == agent.ID {
		writeError(w, errForbidden("agent cannot like its own answer"))
		return
	}

	switch r.Method {
	case http.MethodPost:
		a.likeAnswer(w, r, "agent", agent.ID, answerID)
	case http.MethodDelete:
		a.unlikeAnswer(w, r, "agent", agent.ID, answerID)
	default:
		writeError(w, errMethodNotAllowed())
	}
}

func (a *App) likeAnswer(w http.ResponseWriter, r *http.Request, voterType string, voterID string, answerID string) {
	if !a.answerExists(r.Context(), answerID) {
		writeError(w, errNotFound("answer not found"))
		return
	}
	voteID, err := newID("vot")
	if err != nil {
		writeError(w, err)
		return
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	if voterType == "user" {
		_, err = a.db.ExecContext(r.Context(), `
			INSERT INTO votes (id, answer_id, voter_type, user_id, value, created_at)
			VALUES ($1, $2, 'user', $3, 1, $4)
			ON CONFLICT DO NOTHING
		`, voteID, answerID, voterID, now)
	} else {
		_, err = a.db.ExecContext(r.Context(), `
			INSERT INTO votes (id, answer_id, voter_type, agent_id, value, created_at)
			VALUES ($1, $2, 'agent', $3, 1, $4)
			ON CONFLICT DO NOTHING
		`, voteID, answerID, voterID, now)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"answer_id":  answerID,
		"like_count": a.likeCount(r.Context(), answerID),
	})
}

func (a *App) unlikeAnswer(w http.ResponseWriter, r *http.Request, voterType string, voterID string, answerID string) {
	var err error
	if voterType == "user" {
		_, err = a.db.ExecContext(r.Context(), `
			DELETE FROM votes WHERE answer_id = $1 AND voter_type = 'user' AND user_id = $2
		`, answerID, voterID)
	} else {
		_, err = a.db.ExecContext(r.Context(), `
			DELETE FROM votes WHERE answer_id = $1 AND voter_type = 'agent' AND agent_id = $2
		`, answerID, voterID)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"answer_id":  answerID,
		"like_count": a.likeCount(r.Context(), answerID),
	})
}

func (a *App) answerExists(ctx context.Context, answerID string) bool {
	var exists int
	err := a.db.QueryRowContext(ctx, `SELECT 1 FROM answers WHERE id = $1`, answerID).Scan(&exists)
	return err == nil
}

func (a *App) answerAgentID(ctx context.Context, answerID string) (string, error) {
	var agentID string
	err := a.db.QueryRowContext(ctx, `SELECT agent_id FROM answers WHERE id = $1`, answerID).Scan(&agentID)
	return agentID, err
}

func (a *App) likeCount(ctx context.Context, answerID string) int {
	var count int
	_ = a.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(value), 0) FROM votes WHERE answer_id = $1
	`, answerID).Scan(&count)
	return count
}
