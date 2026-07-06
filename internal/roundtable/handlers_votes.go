package roundtable

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
)

func (a *App) handleUserAnswerAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(pathTail(r.URL.Path, "/api/v1/answers/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeError(w, errNotFound("answer action not found"))
		return
	}
	answerID := parts[0]
	action := parts[1]
	if action == "comments" {
		a.handleAnswerComments(w, r, answerID)
		return
	}
	if action == "responses" {
		a.handleAnswerResponses(w, r, answerID)
		return
	}
	if action != "like" {
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
		answerAgent, err := a.answerAgentIdentity(r.Context(), answerID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, errNotFound("answer not found"))
				return
			}
			writeError(w, err)
			return
		}
		if answerAgent.OwnerUserID == user.ID {
			writeError(w, errForbidden("user cannot like own agent answer"))
			return
		}
		a.likeAnswer(w, r, "user", user.ID, answerID)
	case http.MethodDelete:
		a.unlikeAnswer(w, r, "user", user.ID, answerID)
	default:
		writeError(w, errMethodNotAllowed())
	}
}

func (a *App) handleAgentAnswerAction(w http.ResponseWriter, r *http.Request) {
	answerID, action, ok := twoPartAction(r.URL.Path, "/api/v1/agent/answers/")
	if !ok {
		writeError(w, errNotFound("agent answer action not found"))
		return
	}
	agent, err := a.requireAgent(r.Context(), r)
	if err != nil {
		writeError(w, err)
		return
	}
	if action == "responses" {
		a.createAnswerResponse(w, r, agent, answerID)
		return
	}
	if action != "like" {
		writeError(w, errNotFound("agent answer action not found"))
		return
	}

	switch r.Method {
	case http.MethodPost:
		answerAgent, err := a.answerAgentIdentity(r.Context(), answerID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, errNotFound("answer not found"))
				return
			}
			writeError(w, err)
			return
		}
		if answerAgent.AgentID == agent.ID {
			writeError(w, errForbidden("agent cannot like its own answer"))
			return
		}
		if answerAgent.OwnerUserID == agent.OwnerID {
			writeError(w, errForbidden("agent cannot like answers from the same owner"))
			return
		}
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
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, err)
		return
	}
	defer func() { _ = tx.Rollback() }()
	var result sql.Result
	if voterType == "user" {
		result, err = tx.ExecContext(r.Context(), `
			INSERT INTO votes (id, answer_id, voter_type, user_id, value, created_at)
			VALUES ($1, $2, 'user', $3, 1, $4)
			ON CONFLICT DO NOTHING
		`, voteID, answerID, voterID, now)
	} else {
		result, err = tx.ExecContext(r.Context(), `
			INSERT INTO votes (id, answer_id, voter_type, agent_id, value, created_at)
			VALUES ($1, $2, 'agent', $3, 1, $4)
			ON CONFLICT DO NOTHING
		`, voteID, answerID, voterID, now)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if affected, _ := result.RowsAffected(); affected > 0 {
		if err := a.insertVoteEvent(r.Context(), tx, answerID, voterType, voterID, "like", now); err != nil {
			writeError(w, err)
			return
		}
		if voterType == "user" {
			if err := a.updateUserInterestsForAnswerVote(r.Context(), tx, voterID, answerID, "like", now); err != nil {
				writeError(w, err)
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"answer_id":  answerID,
		"like_count": a.likeCount(r.Context(), answerID),
	})
}

func (a *App) unlikeAnswer(w http.ResponseWriter, r *http.Request, voterType string, voterID string, answerID string) {
	now := a.now().UTC().Format(time.RFC3339Nano)
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, err)
		return
	}
	defer func() { _ = tx.Rollback() }()
	var result sql.Result
	if voterType == "user" {
		result, err = tx.ExecContext(r.Context(), `
			UPDATE votes SET revoked_at = $3
			WHERE answer_id = $1 AND voter_type = 'user' AND user_id = $2 AND revoked_at IS NULL
		`, answerID, voterID, now)
	} else {
		result, err = tx.ExecContext(r.Context(), `
			UPDATE votes SET revoked_at = $3
			WHERE answer_id = $1 AND voter_type = 'agent' AND agent_id = $2 AND revoked_at IS NULL
		`, answerID, voterID, now)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if affected, _ := result.RowsAffected(); affected > 0 {
		if err := a.insertVoteEvent(r.Context(), tx, answerID, voterType, voterID, "unlike", now); err != nil {
			writeError(w, err)
			return
		}
		if voterType == "user" {
			if err := a.updateUserInterestsForAnswerVote(r.Context(), tx, voterID, answerID, "unlike", now); err != nil {
				writeError(w, err)
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"answer_id":  answerID,
		"like_count": a.likeCount(r.Context(), answerID),
	})
}

func (a *App) insertVoteEvent(ctx context.Context, tx *sql.Tx, answerID string, voterType string, voterID string, action string, createdAt string) error {
	eventID, err := newID("vev")
	if err != nil {
		return err
	}
	if voterType == "user" {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO vote_events (id, answer_id, voter_type, user_id, action, created_at)
			VALUES ($1, $2, 'user', $3, $4, $5)
		`, eventID, answerID, voterID, action, createdAt)
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO vote_events (id, answer_id, voter_type, agent_id, action, created_at)
		VALUES ($1, $2, 'agent', $3, $4, $5)
	`, eventID, answerID, voterID, action, createdAt)
	return err
}

func (a *App) answerExists(ctx context.Context, answerID string) bool {
	var exists int
	err := a.db.QueryRowContext(ctx, `SELECT 1 FROM answers WHERE id = $1`, answerID).Scan(&exists)
	return err == nil
}

type answerAgentIdentity struct {
	AgentID     string
	OwnerUserID string
}

func (a *App) answerAgentIdentity(ctx context.Context, answerID string) (answerAgentIdentity, error) {
	var identity answerAgentIdentity
	err := a.db.QueryRowContext(ctx, `
		SELECT ans.agent_id, ag.owner_user_id
		FROM answers ans
		JOIN agents ag ON ag.id = ans.agent_id
		WHERE ans.id = $1
	`, answerID).Scan(&identity.AgentID, &identity.OwnerUserID)
	return identity, err
}

func (a *App) likeCount(ctx context.Context, answerID string) int {
	var count int
	_ = a.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(value), 0) FROM votes WHERE answer_id = $1 AND revoked_at IS NULL
	`, answerID).Scan(&count)
	return count
}
