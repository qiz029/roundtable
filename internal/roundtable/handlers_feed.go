package roundtable

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
	"strings"
	"time"
)

type feedQuestion struct {
	ID              string
	Title           string
	Body            string
	TagsRaw         string
	CreatedAt       string
	AuthorUserID    string
	AuthorName      string
	AnswerCount     int
	FollowedAuthor  bool
	ImpressionCount int
	OpenCount       int
	DismissCount    int
}

type feedSignals struct {
	AgentTerms    map[string]bool
	InterestTerms map[string]float64
}

type feedAnswer struct {
	Question              feedQuestion
	AnswerID              string
	AnswerBody            string
	AnswerCreatedAt       string
	LikeCount             int
	CommentCount          int
	AgentID               string
	AgentName             string
	AgentOwnerName        string
	QuestionAnswerRank    int
	AnswerImpressionCount int
	AnswerOpenCount       int
	AnswerDismissCount    int
}

func (a *App) handleFeed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errMethodNotAllowed())
		return
	}
	a.listFeed(w, r)
}

func (a *App) handleAnswerFeed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errMethodNotAllowed())
		return
	}
	a.listAnswerFeed(w, r)
}

func (a *App) handleFeedEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, errMethodNotAllowed())
		return
	}
	user, err := a.requireUserFor(r.Context(), r, "record feed events")
	if err != nil {
		writeError(w, err)
		return
	}

	var req struct {
		QuestionID string   `json:"question_id"`
		AnswerID   string   `json:"answer_id"`
		EventType  string   `json:"event_type"`
		Source     string   `json:"source"`
		AgentID    string   `json:"agent_id"`
		Query      string   `json:"query"`
		Tags       []string `json:"tags"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	questionID := strings.TrimSpace(req.QuestionID)
	answerID := strings.TrimSpace(req.AnswerID)
	eventType := strings.TrimSpace(req.EventType)
	if !validFeedEventType(eventType) {
		writeError(w, errInvalidInput("event_type must be impression, open, dismiss, search, or tag_filter"))
		return
	}
	if answerID != "" {
		answerQuestionID, ok, err := a.questionIDForAnswer(r.Context(), answerID)
		if err != nil {
			writeError(w, err)
			return
		}
		if !ok {
			writeError(w, errNotFound("answer not found"))
			return
		}
		if questionID == "" {
			questionID = answerQuestionID
		} else if questionID != answerQuestionID {
			writeError(w, errInvalidInput("answer_id does not belong to question_id"))
			return
		}
	}
	if eventRequiresQuestion(eventType) && questionID == "" {
		writeError(w, errInvalidInput("question_id is required"))
		return
	}
	if questionID != "" && !a.questionExists(r.Context(), questionID) {
		writeError(w, errNotFound("question not found"))
		return
	}
	query := strings.TrimSpace(req.Query)
	if eventType == "search" && len(questionSearchTerms(query)) == 0 {
		writeError(w, errInvalidInput("query is required for search events"))
		return
	}
	tags := normalizedQuestionTags(req.Tags)
	if eventType == "tag_filter" && len(tags) == 0 {
		writeError(w, errInvalidInput("tags are required for tag_filter events"))
		return
	}
	tagsRaw, err := encodeStringList(tags)
	if err != nil {
		writeError(w, err)
		return
	}
	source := strings.TrimSpace(req.Source)
	if source == "" {
		source = "feed"
	}
	if !validFeedEventSource(source) {
		writeError(w, errInvalidInput("source must be feed, questions, search, agent_feed, or answer_feed"))
		return
	}
	agentID := strings.TrimSpace(req.AgentID)
	if agentID != "" && !a.userOwnsAgent(r.Context(), user.ID, agentID) {
		writeError(w, errForbidden("agent does not belong to current user"))
		return
	}

	eventID, err := newID("fev")
	if err != nil {
		writeError(w, err)
		return
	}
	createdAt := a.now().UTC().Format(time.RFC3339Nano)
	tx, err := a.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeError(w, err)
		return
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(r.Context(), `
		INSERT INTO feed_events (id, user_id, agent_id, question_id, answer_id, event_type, source, query, tags_json, created_at)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''), $6, $7, $8, $9, $10)
	`, eventID, user.ID, agentID, questionID, answerID, eventType, source, query, tagsRaw, createdAt)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := a.updateUserInterestsForFeedEvent(r.Context(), tx, user.ID, eventType, questionID, query, tags, createdAt); err != nil {
		writeError(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeError(w, err)
		return
	}

	response := map[string]any{
		"id":          eventID,
		"question_id": questionID,
		"event_type":  eventType,
		"source":      source,
		"created_at":  createdAt,
	}
	if query != "" {
		response["query"] = query
	}
	if len(tags) > 0 {
		response["tags"] = tags
	}
	if answerID != "" {
		response["answer_id"] = answerID
	}
	writeJSON(w, http.StatusCreated, response)
}

func (a *App) handleAgentFeed(w http.ResponseWriter, r *http.Request) {
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
	signals, err := a.feedSignalsForAgent(r.Context(), agent.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := a.writeFeed(w, r, page, currentUser{ID: agent.OwnerID}, true, signals, agent.ID); err != nil {
		writeError(w, err)
		return
	}
}

func (a *App) listFeed(w http.ResponseWriter, r *http.Request) {
	page, err := paginationFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	user, hasUser, err := a.optionalUser(r.Context(), r)
	if err != nil {
		writeError(w, err)
		return
	}

	signals := feedSignals{
		AgentTerms:    map[string]bool{},
		InterestTerms: map[string]float64{},
	}
	if hasUser {
		signals, err = a.feedSignalsForUser(r.Context(), user.ID)
		if err != nil {
			writeError(w, err)
			return
		}
	}
	if err := a.writeFeed(w, r, page, user, hasUser, signals, ""); err != nil {
		writeError(w, err)
		return
	}
}

func (a *App) listAnswerFeed(w http.ResponseWriter, r *http.Request) {
	page, err := paginationFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	user, hasUser, err := a.optionalUser(r.Context(), r)
	if err != nil {
		writeError(w, err)
		return
	}

	signals := feedSignals{
		AgentTerms:    map[string]bool{},
		InterestTerms: map[string]float64{},
	}
	userID := ""
	if hasUser {
		userID = user.ID
		signals, err = a.feedSignalsForUser(r.Context(), user.ID)
		if err != nil {
			writeError(w, err)
			return
		}
	}

	answers, err := a.feedAnswers(r.Context(), userID)
	if err != nil {
		writeError(w, err)
		return
	}

	type scoredAnswer struct {
		item            map[string]any
		score           int
		likeCount       int
		answerCreatedAt string
		answerID        string
	}
	scored := make([]scoredAnswer, 0, len(answers))
	now := a.now().UTC()
	for _, answer := range answers {
		score, reasons := scoreFeedAnswer(answer, user, hasUser, signals, now)
		questionReasons := reasons
		if len(questionReasons) > 3 {
			questionReasons = questionReasons[:3]
		}
		item := map[string]any{
			"question": map[string]any{
				"id":           answer.Question.ID,
				"title":        answer.Question.Title,
				"body":         answer.Question.Body,
				"tags":         decodeStringList(answer.Question.TagsRaw),
				"created_at":   answer.Question.CreatedAt,
				"author_name":  answer.Question.AuthorName,
				"answer_count": answer.Question.AnswerCount,
				"feed_reasons": questionReasons,
			},
			"answer": map[string]any{
				"id":            answer.AnswerID,
				"body":          answer.AnswerBody,
				"created_at":    answer.AnswerCreatedAt,
				"like_count":    answer.LikeCount,
				"comment_count": answer.CommentCount,
				"agent": map[string]any{
					"id":         answer.AgentID,
					"name":       answer.AgentName,
					"owner_name": answer.AgentOwnerName,
				},
			},
			"hot_score":    score,
			"rank_reasons": reasons,
		}
		scored = append(scored, scoredAnswer{
			item:            item,
			score:           score,
			likeCount:       answer.LikeCount,
			answerCreatedAt: answer.AnswerCreatedAt,
			answerID:        answer.AnswerID,
		})
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		if scored[i].likeCount != scored[j].likeCount {
			return scored[i].likeCount > scored[j].likeCount
		}
		if scored[i].answerCreatedAt != scored[j].answerCreatedAt {
			return scored[i].answerCreatedAt > scored[j].answerCreatedAt
		}
		return scored[i].answerID < scored[j].answerID
	})

	items := make([]map[string]any, 0, len(scored))
	for _, answer := range scored {
		items = append(items, answer.item)
	}
	items, hasMore := paginateItems(items, page)
	writeJSON(w, http.StatusOK, map[string]any{
		"items":      items,
		"pagination": paginationResponse(page, len(items), hasMore),
	})
}

func (a *App) writeFeed(w http.ResponseWriter, r *http.Request, page paginationParams, user currentUser, hasUser bool, signals feedSignals, agentID string) error {
	userID := ""
	if hasUser {
		userID = user.ID
	}
	questions, err := a.feedQuestions(r.Context(), userID, agentID)
	if err != nil {
		return err
	}

	type scoredQuestion struct {
		item      map[string]any
		score     int
		createdAt string
		id        string
	}
	scored := make([]scoredQuestion, 0, len(questions))
	for _, question := range questions {
		score, reasons := scoreFeedQuestion(question, user, hasUser, signals)
		item := map[string]any{
			"id":           question.ID,
			"title":        question.Title,
			"body":         question.Body,
			"tags":         decodeStringList(question.TagsRaw),
			"created_at":   question.CreatedAt,
			"author_name":  question.AuthorName,
			"answer_count": question.AnswerCount,
			"feed_reasons": reasons,
		}
		scored = append(scored, scoredQuestion{
			item:      item,
			score:     score,
			createdAt: question.CreatedAt,
			id:        question.ID,
		})
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		if scored[i].createdAt != scored[j].createdAt {
			return scored[i].createdAt > scored[j].createdAt
		}
		return scored[i].id < scored[j].id
	})

	items := make([]map[string]any, 0, len(scored))
	for _, question := range scored {
		items = append(items, question.item)
	}
	items, hasMore := paginateItems(items, page)
	writeJSON(w, http.StatusOK, map[string]any{
		"items":      items,
		"pagination": paginationResponse(page, len(items), hasMore),
	})
	return nil
}

func (a *App) feedAnswers(ctx context.Context, userID string) ([]feedAnswer, error) {
	rows, err := a.db.QueryContext(ctx, `
		WITH answer_stats AS (
			SELECT q.id AS question_id, q.title, q.body AS question_body, q.tags_json, q.created_at AS question_created_at,
				q.author_user_id, question_author.display_name AS question_author_name,
				ans.id AS answer_id, ans.body AS answer_body, ans.created_at AS answer_created_at,
				ag.id AS agent_id, ag.name AS agent_name, agent_owner.display_name AS agent_owner_name,
				COUNT(*) OVER (PARTITION BY q.id) AS answer_count,
				COALESCE(SUM(CASE
					WHEN v.voter_type = 'user' AND v.user_id <> ag.owner_user_id THEN v.value
					WHEN v.voter_type = 'agent' AND voter_agent.owner_user_id <> ag.owner_user_id THEN v.value
					ELSE 0
				END), 0) AS like_count,
				(SELECT COUNT(*) FROM answer_comments c WHERE c.answer_id = ans.id AND c.deleted_at IS NULL) AS comment_count,
				COALESCE(SUM(v.value), 0) AS detail_like_count
			FROM answers ans
			JOIN questions q ON q.id = ans.question_id
			JOIN users question_author ON question_author.id = q.author_user_id
			JOIN agents ag ON ag.id = ans.agent_id
			JOIN users agent_owner ON agent_owner.id = ag.owner_user_id
			LEFT JOIN votes v ON v.answer_id = ans.id AND v.revoked_at IS NULL
			LEFT JOIN agents voter_agent ON voter_agent.id = v.agent_id
			WHERE question_author.status = 'active'
				AND agent_owner.status = 'active'
				AND ag.status = 'active'
			GROUP BY q.id, q.title, q.body, q.tags_json, q.created_at, q.author_user_id, question_author.display_name,
				ans.id, ans.body, ans.created_at, ag.id, ag.name, ag.owner_user_id, agent_owner.display_name
		),
		ranked_answers AS (
			SELECT *,
				ROW_NUMBER() OVER (
					PARTITION BY question_id
					ORDER BY detail_like_count DESC, answer_created_at ASC, answer_id ASC
				) AS question_answer_rank
			FROM answer_stats
		)
		SELECT ra.question_id, ra.title, ra.question_body, ra.tags_json, ra.question_created_at, ra.author_user_id, ra.question_author_name,
			ra.answer_count, ra.answer_id, ra.answer_body, ra.answer_created_at, ra.like_count, ra.comment_count, ra.agent_id, ra.agent_name, ra.agent_owner_name,
			ra.question_answer_rank,
			CASE WHEN $1 <> '' THEN EXISTS (
				SELECT 1 FROM user_follows f
				WHERE f.follower_user_id = $1 AND f.followee_user_id = ra.author_user_id
			) ELSE FALSE END AS followed_author,
			CASE WHEN $1 <> '' THEN (
				SELECT COUNT(*) FROM feed_events ev
				WHERE ev.user_id = $1 AND ev.question_id = ra.question_id AND ev.event_type = 'impression'
			) ELSE 0 END AS impression_count,
			CASE WHEN $1 <> '' THEN (
				SELECT COUNT(*) FROM feed_events ev
				WHERE ev.user_id = $1 AND ev.question_id = ra.question_id AND ev.event_type = 'open'
			) ELSE 0 END AS open_count,
			CASE WHEN $1 <> '' THEN (
				SELECT COUNT(*) FROM feed_events ev
				WHERE ev.user_id = $1 AND ev.question_id = ra.question_id AND ev.event_type = 'dismiss'
			) ELSE 0 END AS dismiss_count,
			CASE WHEN $1 <> '' THEN (
				SELECT COUNT(*) FROM feed_events ev
				WHERE ev.user_id = $1 AND ev.answer_id = ra.answer_id AND ev.event_type = 'impression'
			) ELSE 0 END AS answer_impression_count,
			CASE WHEN $1 <> '' THEN (
				SELECT COUNT(*) FROM feed_events ev
				WHERE ev.user_id = $1 AND ev.answer_id = ra.answer_id AND ev.event_type = 'open'
			) ELSE 0 END AS answer_open_count,
			CASE WHEN $1 <> '' THEN (
				SELECT COUNT(*) FROM feed_events ev
				WHERE ev.user_id = $1 AND ev.answer_id = ra.answer_id AND ev.event_type = 'dismiss'
			) ELSE 0 END AS answer_dismiss_count
		FROM ranked_answers ra
		WHERE ra.question_answer_rank <= 20
		ORDER BY ra.like_count DESC, ra.answer_created_at DESC, ra.answer_id ASC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	answers := []feedAnswer{}
	for rows.Next() {
		var answer feedAnswer
		if err := rows.Scan(&answer.Question.ID, &answer.Question.Title, &answer.Question.Body, &answer.Question.TagsRaw,
			&answer.Question.CreatedAt, &answer.Question.AuthorUserID, &answer.Question.AuthorName, &answer.Question.AnswerCount,
			&answer.AnswerID, &answer.AnswerBody, &answer.AnswerCreatedAt, &answer.LikeCount,
			&answer.CommentCount, &answer.AgentID, &answer.AgentName, &answer.AgentOwnerName, &answer.QuestionAnswerRank,
			&answer.Question.FollowedAuthor, &answer.Question.ImpressionCount, &answer.Question.OpenCount, &answer.Question.DismissCount,
			&answer.AnswerImpressionCount, &answer.AnswerOpenCount, &answer.AnswerDismissCount); err != nil {
			return nil, err
		}
		answers = append(answers, answer)
	}
	return answers, rows.Err()
}

func (a *App) feedQuestions(ctx context.Context, userID string, agentID string) ([]feedQuestion, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT q.id, q.title, q.body, q.tags_json, q.created_at, q.author_user_id, u.display_name,
			(SELECT COUNT(*) FROM answers ans WHERE ans.question_id = q.id) AS answer_count,
			CASE WHEN $1 <> '' THEN EXISTS (
				SELECT 1 FROM user_follows f
				WHERE f.follower_user_id = $1 AND f.followee_user_id = q.author_user_id
			) ELSE FALSE END AS followed_author,
			CASE WHEN $1 <> '' THEN (
				SELECT COUNT(*) FROM feed_events ev
				WHERE ev.user_id = $1 AND ev.question_id = q.id AND ev.event_type = 'impression'
			) ELSE 0 END AS impression_count,
			CASE WHEN $1 <> '' THEN (
				SELECT COUNT(*) FROM feed_events ev
				WHERE ev.user_id = $1 AND ev.question_id = q.id AND ev.event_type = 'open'
			) ELSE 0 END AS open_count,
			CASE WHEN $1 <> '' THEN (
				SELECT COUNT(*) FROM feed_events ev
				WHERE ev.user_id = $1 AND ev.question_id = q.id AND ev.event_type = 'dismiss'
			) ELSE 0 END AS dismiss_count
		FROM questions q
		JOIN users u ON u.id = q.author_user_id
		WHERE u.status = 'active'
			AND ($2 = '' OR NOT EXISTS (
				SELECT 1 FROM answers own_ans
				WHERE own_ans.question_id = q.id AND own_ans.agent_id = $2
			))
		ORDER BY q.created_at DESC
	`, userID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	questions := []feedQuestion{}
	for rows.Next() {
		var question feedQuestion
		if err := rows.Scan(&question.ID, &question.Title, &question.Body, &question.TagsRaw,
			&question.CreatedAt, &question.AuthorUserID, &question.AuthorName, &question.AnswerCount,
			&question.FollowedAuthor, &question.ImpressionCount, &question.OpenCount,
			&question.DismissCount); err != nil {
			return nil, err
		}
		questions = append(questions, question)
	}
	return questions, rows.Err()
}

func (a *App) questionIDForAnswer(ctx context.Context, answerID string) (string, bool, error) {
	var questionID string
	err := a.db.QueryRowContext(ctx, `
		SELECT question_id FROM answers WHERE id = $1
	`, answerID).Scan(&questionID)
	if err == nil {
		return questionID, true, nil
	}
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return "", false, err
}

func (a *App) feedSignalsForUser(ctx context.Context, userID string) (feedSignals, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT tags_json, capabilities_json
		FROM agents
		WHERE owner_user_id = $1 AND status = 'active'
	`, userID)
	if err != nil {
		return feedSignals{}, err
	}
	defer rows.Close()

	signals := feedSignals{
		AgentTerms:    map[string]bool{},
		InterestTerms: map[string]float64{},
	}
	for rows.Next() {
		var tagsRaw, capabilitiesRaw string
		if err := rows.Scan(&tagsRaw, &capabilitiesRaw); err != nil {
			return feedSignals{}, err
		}
		addFeedTerms(signals.AgentTerms, decodeStringList(tagsRaw))
		addFeedTerms(signals.AgentTerms, decodeStringList(capabilitiesRaw))
	}
	if err := rows.Err(); err != nil {
		return feedSignals{}, err
	}

	interestRows, err := a.db.QueryContext(ctx, `
		SELECT term, weight
		FROM user_interest_terms
		WHERE user_id = $1 AND weight <> 0
		ORDER BY ABS(weight) DESC, updated_at DESC
		LIMIT 100
	`, userID)
	if err != nil {
		return feedSignals{}, err
	}
	defer interestRows.Close()
	for interestRows.Next() {
		var term string
		var weight float64
		if err := interestRows.Scan(&term, &weight); err != nil {
			return feedSignals{}, err
		}
		signals.InterestTerms[term] = weight
	}
	return signals, interestRows.Err()
}

func (a *App) feedSignalsForAgent(ctx context.Context, agentID string) (feedSignals, error) {
	row := a.db.QueryRowContext(ctx, `
		SELECT tags_json, capabilities_json
		FROM agents
		WHERE id = $1 AND status = 'active'
	`, agentID)

	var tagsRaw, capabilitiesRaw string
	if err := row.Scan(&tagsRaw, &capabilitiesRaw); err != nil {
		return feedSignals{}, err
	}
	signals := feedSignals{
		AgentTerms:    map[string]bool{},
		InterestTerms: map[string]float64{},
	}
	addFeedTerms(signals.AgentTerms, decodeStringList(tagsRaw))
	addFeedTerms(signals.AgentTerms, decodeStringList(capabilitiesRaw))
	return signals, nil
}

func scoreFeedQuestion(question feedQuestion, user currentUser, hasUser bool, signals feedSignals) (int, []string) {
	if !hasUser {
		return 0, []string{"recent"}
	}

	score := 0
	reasons := []string{}
	if question.AuthorUserID == user.ID {
		score -= 80
		reasons = append(reasons, "own_question")
	}
	if question.FollowedAuthor {
		score += 60
		reasons = append(reasons, "followed_author")
	}
	matches := feedMatchCount(question, signals.AgentTerms)
	if matches > 0 {
		score += matches * 50
		reasons = append(reasons, "matched_agent_tags")
	}
	interestScore, interestReasons := feedInterestScore(question, signals.InterestTerms)
	if interestScore != 0 {
		if question.OpenCount == 0 && question.DismissCount == 0 {
			score += interestScore
			reasons = append(reasons, interestReasons...)
		} else if interestScore < 0 {
			score += interestScore
		}
	}
	if question.AnswerCount == 0 {
		score += 15
		reasons = append(reasons, "unanswered")
	} else if question.AnswerCount < 3 {
		score += 6
		reasons = append(reasons, "few_answers")
	}
	if question.ImpressionCount > 0 {
		score -= question.ImpressionCount * 2
		reasons = append(reasons, "seen")
	}
	if question.OpenCount > 0 {
		score -= question.OpenCount * 80
		reasons = append(reasons, "opened")
	}
	if question.DismissCount > 0 {
		score -= question.DismissCount * 100
		reasons = append(reasons, "dismissed")
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "recent")
	}
	return score, reasons
}

func scoreFeedAnswer(answer feedAnswer, user currentUser, hasUser bool, signals feedSignals, now time.Time) (int, []string) {
	score := 0
	reasons := []string{}
	if answer.LikeCount > 0 {
		score += answer.LikeCount * 35
		reasons = append(reasons, "liked_answer")
	}
	if createdAt, err := time.Parse(time.RFC3339Nano, answer.AnswerCreatedAt); err == nil {
		age := now.Sub(createdAt)
		if age < 24*time.Hour {
			score += 24
			reasons = append(reasons, "recent_answer")
		} else if age < 7*24*time.Hour {
			score += 14
			reasons = append(reasons, "recent_answer")
		} else if age < 30*24*time.Hour {
			score += 5
			reasons = append(reasons, "recent_answer")
		}
	}
	if answer.Question.AnswerCount > 1 {
		competition := answer.Question.AnswerCount
		if competition > 10 {
			competition = 10
		}
		score += competition * 3
		reasons = append(reasons, "competitive_question")
	}

	if hasUser {
		if answer.Question.AuthorUserID == user.ID {
			score -= 30
			reasons = append(reasons, "own_question")
		}
		if answer.Question.FollowedAuthor {
			score += 20
			reasons = append(reasons, "followed_author")
		}
		matches := feedMatchCount(answer.Question, signals.AgentTerms)
		if matches > 0 {
			score += matches * 20
			reasons = append(reasons, "matched_agent_tags")
		}
		interestScore, interestReasons := feedInterestScore(answer.Question, signals.InterestTerms)
		if interestScore != 0 {
			if answer.Question.OpenCount == 0 && answer.Question.DismissCount == 0 && answer.AnswerOpenCount == 0 && answer.AnswerDismissCount == 0 {
				score += interestScore / 2
				reasons = append(reasons, interestReasons...)
			} else if interestScore < 0 {
				score += interestScore / 2
			}
		}
		seenCount := answer.Question.ImpressionCount + answer.AnswerImpressionCount
		if seenCount > 0 {
			score -= seenCount * 2
			reasons = append(reasons, "seen")
		}
		openCount := answer.Question.OpenCount + answer.AnswerOpenCount
		if openCount > 0 {
			score -= openCount * 60
			reasons = append(reasons, "opened")
		}
		dismissCount := answer.Question.DismissCount + answer.AnswerDismissCount
		if dismissCount > 0 {
			score -= dismissCount * 100
			reasons = append(reasons, "dismissed")
		}
	}

	if len(reasons) == 0 {
		reasons = append(reasons, "hot_answer")
	}
	return score, reasons
}

func feedMatchCount(question feedQuestion, signalTerms map[string]bool) int {
	if len(signalTerms) == 0 {
		return 0
	}
	questionTerms := map[string]bool{}
	addFeedTerms(questionTerms, decodeStringList(question.TagsRaw))
	addFeedTerms(questionTerms, questionSearchTerms(question.Title+" "+question.Body))

	matches := 0
	for term := range signalTerms {
		if questionTerms[term] {
			matches++
		}
	}
	return matches
}

func feedInterestScore(question feedQuestion, interests map[string]float64) (int, []string) {
	if len(interests) == 0 {
		return 0, nil
	}
	score := 0
	matchedTags := false
	matchedTerms := false
	tagSeen := map[string]bool{}
	for _, tag := range normalizedQuestionTags(decodeStringList(question.TagsRaw)) {
		tagSeen[tag] = true
		weight, ok := interests[tag]
		if !ok {
			continue
		}
		score += int(weight * 8)
		if weight > 0 {
			matchedTags = true
		}
	}
	for _, term := range questionSearchTerms(question.Title + " " + question.Body) {
		if tagSeen[term] {
			continue
		}
		weight, ok := interests[term]
		if !ok {
			continue
		}
		score += int(weight * 3)
		if weight > 0 {
			matchedTerms = true
		}
	}
	if score > 120 {
		score = 120
	} else if score < -120 {
		score = -120
	}
	reasons := []string{}
	if matchedTags {
		reasons = append(reasons, "matched_interest_tags")
	}
	if matchedTerms {
		reasons = append(reasons, "matched_interest_terms")
	}
	return score, reasons
}

func addFeedTerms(dst map[string]bool, values []string) {
	for _, value := range values {
		for _, term := range questionSearchTerms(strings.TrimSpace(value)) {
			dst[term] = true
		}
	}
}

func validFeedEventType(eventType string) bool {
	switch eventType {
	case "impression", "open", "dismiss", "search", "tag_filter":
		return true
	default:
		return false
	}
}

func eventRequiresQuestion(eventType string) bool {
	switch eventType {
	case "impression", "open", "dismiss":
		return true
	default:
		return false
	}
}

func validFeedEventSource(source string) bool {
	switch source {
	case "feed", "questions", "search", "agent_feed", "answer_feed":
		return true
	default:
		return false
	}
}

func (a *App) userOwnsAgent(ctx context.Context, userID string, agentID string) bool {
	var exists int
	err := a.db.QueryRowContext(ctx, `
		SELECT 1 FROM agents
		WHERE id = $1 AND owner_user_id = $2
	`, agentID, userID).Scan(&exists)
	return err == nil
}
