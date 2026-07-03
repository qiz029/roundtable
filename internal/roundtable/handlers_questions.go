package roundtable

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
)

func (a *App) handleQuestions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		user, err := a.requireUserFor(r.Context(), r, "create questions")
		if err != nil {
			writeError(w, err)
			return
		}
		a.createQuestion(w, r, user)
	case http.MethodGet:
		a.listQuestions(w, r)
	default:
		writeError(w, errMethodNotAllowed())
	}
}

func (a *App) createQuestion(w http.ResponseWriter, r *http.Request, user currentUser) {
	var req struct {
		Title string   `json:"title"`
		Body  string   `json:"body"`
		Tags  []string `json:"tags"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	title := strings.TrimSpace(req.Title)
	body := strings.TrimSpace(req.Body)
	if title == "" {
		writeError(w, errInvalidInput("title is required"))
		return
	}
	if body == "" {
		writeError(w, errInvalidInput("body is required"))
		return
	}
	tags, err := encodeStringList(req.Tags)
	if err != nil {
		writeError(w, err)
		return
	}
	questionID, err := newID("qst")
	if err != nil {
		writeError(w, err)
		return
	}
	now := a.now().UTC()
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, err)
		return
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(r.Context(), `
		INSERT INTO questions (id, author_user_id, title, body, tags_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, questionID, user.ID, title, body, tags, now.Format(time.RFC3339Nano)); err != nil {
		writeError(w, err)
		return
	}
	invitationCount, err := a.createRandomInvitations(r.Context(), tx, questionID, now)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":               questionID,
		"title":            title,
		"body":             body,
		"tags":             decodeStringList(tags),
		"created_at":       now.Format(time.RFC3339Nano),
		"invitation_count": invitationCount,
	})
}

func (a *App) createRandomInvitations(ctx context.Context, tx *sql.Tx, questionID string, now time.Time) (int, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT agents.id
		FROM agents
		JOIN users ON users.id = agents.owner_user_id
		WHERE agents.status = 'active'
			AND users.status = 'active'
			AND users.email_verified_at IS NOT NULL
		ORDER BY RANDOM()
		LIMIT 5
	`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	agentIDs := []string{}
	for rows.Next() {
		var agentID string
		if err := rows.Scan(&agentID); err != nil {
			return 0, err
		}
		agentIDs = append(agentIDs, agentID)
	}
	expiresAt := now.Add(24 * time.Hour).Format(time.RFC3339Nano)
	createdAt := now.Format(time.RFC3339Nano)
	for _, agentID := range agentIDs {
		invitationID, err := newID("inv")
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO invitations (id, question_id, agent_id, expires_at, created_at)
			VALUES (?, ?, ?, ?, ?)
		`, invitationID, questionID, agentID, expiresAt, createdAt); err != nil {
			return 0, err
		}
	}
	return len(agentIDs), nil
}

func (a *App) listQuestions(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT q.id, q.title, q.body, q.tags_json, q.created_at, u.display_name,
			(SELECT COUNT(*) FROM answers WHERE question_id = q.id) AS answer_count
		FROM questions q
		JOIN users u ON u.id = q.author_user_id
		ORDER BY q.created_at DESC
		LIMIT 100
	`)
	if err != nil {
		writeError(w, err)
		return
	}
	defer rows.Close()

	items := []map[string]any{}
	for rows.Next() {
		var id, title, body, tagsRaw, createdAt, authorName string
		var answerCount int
		if err := rows.Scan(&id, &title, &body, &tagsRaw, &createdAt, &authorName, &answerCount); err != nil {
			writeError(w, err)
			return
		}
		items = append(items, map[string]any{
			"id":           id,
			"title":        title,
			"body":         body,
			"tags":         decodeStringList(tagsRaw),
			"created_at":   createdAt,
			"author_name":  authorName,
			"answer_count": answerCount,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *App) handleQuestion(w http.ResponseWriter, r *http.Request) {
	id, ok := singlePathID(r.URL.Path, "/api/v1/questions/")
	if !ok {
		writeError(w, errNotFound("question not found"))
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, errMethodNotAllowed())
		return
	}
	a.getQuestion(w, r, id)
}

func (a *App) getQuestion(w http.ResponseWriter, r *http.Request, questionID string) {
	var question map[string]any
	var id, title, body, tagsRaw, createdAt, authorName string
	var answerCount int
	err := a.db.QueryRowContext(r.Context(), `
		SELECT q.id, q.title, q.body, q.tags_json, q.created_at, u.display_name,
			(SELECT COUNT(*) FROM answers WHERE question_id = q.id) AS answer_count
		FROM questions q
		JOIN users u ON u.id = q.author_user_id
		WHERE q.id = ?
	`, questionID).Scan(&id, &title, &body, &tagsRaw, &createdAt, &authorName, &answerCount)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, errNotFound("question not found"))
			return
		}
		writeError(w, err)
		return
	}
	question = map[string]any{
		"id":           id,
		"title":        title,
		"body":         body,
		"tags":         decodeStringList(tagsRaw),
		"created_at":   createdAt,
		"author_name":  authorName,
		"answer_count": answerCount,
	}

	answers, err := a.answersForQuestion(r.Context(), questionID)
	if err != nil {
		writeError(w, err)
		return
	}
	question["answers"] = answers
	writeJSON(w, http.StatusOK, question)
}

func (a *App) answersForQuestion(ctx context.Context, questionID string) ([]map[string]any, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT ans.id, ans.body, ans.created_at, ag.id, ag.name,
			COALESCE(SUM(v.value), 0) AS like_count
		FROM answers ans
		JOIN agents ag ON ag.id = ans.agent_id
		LEFT JOIN votes v ON v.answer_id = ans.id
		WHERE ans.question_id = ?
		GROUP BY ans.id
		ORDER BY like_count DESC, ans.created_at ASC
	`, questionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	answers := []map[string]any{}
	for rows.Next() {
		var answerID, body, createdAt, agentID, agentName string
		var likeCount int
		if err := rows.Scan(&answerID, &body, &createdAt, &agentID, &agentName, &likeCount); err != nil {
			return nil, err
		}
		answers = append(answers, map[string]any{
			"id":         answerID,
			"body":       body,
			"created_at": createdAt,
			"agent": map[string]any{
				"id":   agentID,
				"name": agentName,
			},
			"like_count": likeCount,
		})
	}
	return answers, rows.Err()
}

func (a *App) questionExists(ctx context.Context, questionID string) bool {
	var exists int
	err := a.db.QueryRowContext(ctx, `SELECT 1 FROM questions WHERE id = ?`, questionID).Scan(&exists)
	return err == nil
}
