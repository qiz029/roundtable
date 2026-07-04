package roundtable

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
		VALUES ($1, $2, $3, $4, $5, $6)
	`, questionID, user.ID, title, body, tags, now.Format(time.RFC3339Nano)); err != nil {
		writeError(w, err)
		return
	}
	if err := a.indexQuestionSearchTerms(r.Context(), tx, questionID, title, body); err != nil {
		writeError(w, err)
		return
	}
	if err := a.indexQuestionTags(r.Context(), tx, questionID, decodeStringList(tags)); err != nil {
		writeError(w, err)
		return
	}
	invitationCount, err := a.createRandomInvitations(r.Context(), tx, questionID, decodeStringList(tags), now)
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

func (a *App) createRandomInvitations(ctx context.Context, tx *sql.Tx, questionID string, questionTags []string, now time.Time) (int, error) {
	agentIDs, err := a.invitationAgentIDs(ctx, tx, questionTags)
	if err != nil {
		return 0, err
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
			VALUES ($1, $2, $3, $4, $5)
		`, invitationID, questionID, agentID, expiresAt, createdAt); err != nil {
			return 0, err
		}
	}
	return len(agentIDs), nil
}

func (a *App) invitationAgentIDs(ctx context.Context, tx *sql.Tx, questionTags []string) ([]string, error) {
	selected := map[string]bool{}
	agentIDs := []string{}
	if len(questionTags) > 0 {
		if err := appendTopicInvitationCandidate(ctx, tx, &agentIDs, selected, questionTags); err != nil {
			return nil, err
		}
	}
	randomTarget := len(agentIDs) + 2
	if randomTarget > 5 {
		randomTarget = 5
	}
	if err := appendInvitationCandidates(ctx, tx, &agentIDs, selected, randomTarget, `
			SELECT agents.id
			FROM agents
			JOIN users ON users.id = agents.owner_user_id
			WHERE agents.status = 'active'
				AND users.status = 'active'
				AND users.email_verified_at IS NOT NULL
			ORDER BY RANDOM()
			LIMIT 10
		`); err != nil {
		return nil, err
	}
	reputationTarget := len(agentIDs) + 2
	if reputationTarget > 5 {
		reputationTarget = 5
	}
	if err := appendInvitationCandidates(ctx, tx, &agentIDs, selected, reputationTarget, `
			SELECT agents.id
			FROM agents
			JOIN users ON users.id = agents.owner_user_id
			WHERE agents.status = 'active'
				AND users.status = 'active'
				AND users.email_verified_at IS NOT NULL
			ORDER BY COALESCE((
				SELECT scores.total_score
				FROM agent_monthly_scores scores
				WHERE scores.agent_id = agents.id
				ORDER BY scores.period DESC
				LIMIT 1
			), 0) DESC, RANDOM()
			LIMIT 10
		`); err != nil {
		return nil, err
	}
	if len(agentIDs) < 5 {
		if err := appendInvitationCandidates(ctx, tx, &agentIDs, selected, 5, `
			SELECT agents.id
			FROM agents
			JOIN users ON users.id = agents.owner_user_id
			WHERE agents.status = 'active'
				AND users.status = 'active'
				AND users.email_verified_at IS NOT NULL
			ORDER BY RANDOM()
			LIMIT 10
		`); err != nil {
			return nil, err
		}
	}
	return agentIDs, nil
}

func appendTopicInvitationCandidate(ctx context.Context, tx *sql.Tx, agentIDs *[]string, selected map[string]bool, questionTags []string) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT agents.id, agents.tags_json, agents.capabilities_json
		FROM agents
		JOIN users ON users.id = agents.owner_user_id
		WHERE agents.status = 'active'
			AND users.status = 'active'
			AND users.email_verified_at IS NOT NULL
		ORDER BY RANDOM()
		LIMIT 50
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var agentID, tagsRaw, capabilitiesRaw string
		if err := rows.Scan(&agentID, &tagsRaw, &capabilitiesRaw); err != nil {
			return err
		}
		if selected[agentID] || !matchesAgentTopic(questionTags, tagsRaw, capabilitiesRaw) {
			continue
		}
		*agentIDs = append(*agentIDs, agentID)
		selected[agentID] = true
		break
	}
	return rows.Err()
}

func matchesAgentTopic(questionTags []string, agentTagsRaw string, capabilitiesRaw string) bool {
	topics := map[string]bool{}
	for _, value := range append(decodeStringList(agentTagsRaw), decodeStringList(capabilitiesRaw)...) {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized != "" {
			topics[normalized] = true
		}
	}
	for _, tag := range questionTags {
		if topics[strings.ToLower(strings.TrimSpace(tag))] {
			return true
		}
	}
	return false
}

func appendInvitationCandidates(ctx context.Context, tx *sql.Tx, agentIDs *[]string, selected map[string]bool, target int, query string) error {
	if len(*agentIDs) >= target {
		return nil
	}
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var agentID string
		if err := rows.Scan(&agentID); err != nil {
			return err
		}
		if selected[agentID] {
			continue
		}
		*agentIDs = append(*agentIDs, agentID)
		selected[agentID] = true
		if len(*agentIDs) >= target {
			break
		}
	}
	return rows.Err()
}

func (a *App) listQuestions(w http.ResponseWriter, r *http.Request) {
	page, err := paginationFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	terms, searching := questionListSearchTerms(r)
	if searching && len(terms) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"items":      []map[string]any{},
			"pagination": paginationResponse(page, 0, false),
		})
		return
	}

	var rows *sql.Rows
	tagFilters := questionListTagFilters(r)
	if searching || len(tagFilters) > 0 {
		args := make([]any, 0, len(terms)+len(tagFilters)+4)
		joins := []string{}
		if searching {
			start := len(args) + 1
			for _, term := range terms {
				args = append(args, term)
			}
			args = append(args, len(terms))
			joins = append(joins, `
				JOIN (
					SELECT question_id
					FROM question_search_terms
					WHERE term IN (`+placeholders(start, len(terms))+`)
					GROUP BY question_id
					HAVING COUNT(DISTINCT term) = `+placeholder(len(args))+`
				) matches ON matches.question_id = q.id
			`)
		}
		if len(tagFilters) > 0 {
			start := len(args) + 1
			for _, tag := range tagFilters {
				args = append(args, tag)
			}
			args = append(args, len(tagFilters))
			joins = append(joins, `
				JOIN (
					SELECT question_id
					FROM question_tags
					WHERE tag IN (`+placeholders(start, len(tagFilters))+`)
					GROUP BY question_id
					HAVING COUNT(DISTINCT tag) = `+placeholder(len(args))+`
				) tag_matches ON tag_matches.question_id = q.id
			`)
		}
		args = append(args, page.Limit+1, page.Offset)
		limitPlaceholder := placeholder(len(args) - 1)
		offsetPlaceholder := placeholder(len(args))
		rows, err = a.db.QueryContext(r.Context(), `
					SELECT q.id, q.title, q.body, q.tags_json, q.created_at, u.display_name,
						(SELECT COUNT(*) FROM answers WHERE question_id = q.id) AS answer_count
				FROM questions q
				JOIN users u ON u.id = q.author_user_id
				`+strings.Join(joins, "\n")+`
				ORDER BY q.created_at DESC
				LIMIT `+limitPlaceholder+` OFFSET `+offsetPlaceholder+`
			`, args...)
	} else {
		rows, err = a.db.QueryContext(r.Context(), `
		SELECT q.id, q.title, q.body, q.tags_json, q.created_at, u.display_name,
			(SELECT COUNT(*) FROM answers WHERE question_id = q.id) AS answer_count
		FROM questions q
		JOIN users u ON u.id = q.author_user_id
		ORDER BY q.created_at DESC
		LIMIT $1 OFFSET $2
	`, page.Limit+1, page.Offset)
	}
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

func questionListSearchTerms(r *http.Request) ([]string, bool) {
	raw := strings.TrimSpace(r.URL.Query().Get("q"))
	if raw == "" {
		raw = strings.TrimSpace(r.URL.Query().Get("query"))
	}
	if raw == "" {
		return nil, false
	}
	return questionSearchTerms(raw), true
}

func questionListTagFilters(r *http.Request) []string {
	rawValues := r.URL.Query()["tags"]
	values := []string{}
	for _, raw := range rawValues {
		values = append(values, strings.Split(raw, ",")...)
	}
	return normalizedQuestionTags(values)
}

func placeholder(position int) string {
	return fmt.Sprintf("$%d", position)
}

func placeholders(start int, count int) string {
	if count <= 0 {
		return ""
	}
	values := make([]string, count)
	for i := range values {
		values[i] = placeholder(start + i)
	}
	return strings.Join(values, ",")
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
		WHERE q.id = $1
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
	question["answers"] = answers
	question["answers_pagination"] = paginationResponse(page, len(answers), hasMore)
	writeJSON(w, http.StatusOK, question)
}

func (a *App) answersForQuestion(ctx context.Context, questionID string, page paginationParams) ([]map[string]any, bool, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT ans.id, ans.body, ans.created_at, ag.id, ag.name, owner.display_name,
			COALESCE(SUM(v.value), 0) AS like_count
		FROM answers ans
		JOIN agents ag ON ag.id = ans.agent_id
		JOIN users owner ON owner.id = ag.owner_user_id
			LEFT JOIN votes v ON v.answer_id = ans.id AND v.revoked_at IS NULL
		WHERE ans.question_id = $1
		GROUP BY ans.id, ans.body, ans.created_at, ag.id, ag.name, owner.display_name
		ORDER BY like_count DESC, ans.created_at ASC
		LIMIT $2 OFFSET $3
	`, questionID, page.Limit+1, page.Offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	answers := []map[string]any{}
	for rows.Next() {
		var answerID, body, createdAt, agentID, agentName, ownerName string
		var likeCount int
		if err := rows.Scan(&answerID, &body, &createdAt, &agentID, &agentName, &ownerName, &likeCount); err != nil {
			return nil, false, err
		}
		answers = append(answers, map[string]any{
			"id":         answerID,
			"body":       body,
			"created_at": createdAt,
			"agent": map[string]any{
				"id":         agentID,
				"name":       agentName,
				"owner_name": ownerName,
			},
			"like_count": likeCount,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	answers, hasMore := trimPaginatedItems(answers, page)
	return answers, hasMore, nil
}

func (a *App) questionExists(ctx context.Context, questionID string) bool {
	var exists int
	err := a.db.QueryRowContext(ctx, `SELECT 1 FROM questions WHERE id = $1`, questionID).Scan(&exists)
	return err == nil
}
