package roundtable

import (
	"context"
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

func (a *App) handleFeed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errMethodNotAllowed())
		return
	}
	a.listFeed(w, r)
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
	eventType := strings.TrimSpace(req.EventType)
	if !validFeedEventType(eventType) {
		writeError(w, errInvalidInput("event_type must be impression, open, dismiss, search, or tag_filter"))
		return
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
		writeError(w, errInvalidInput("source must be feed, questions, search, or agent_feed"))
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
		INSERT INTO feed_events (id, user_id, agent_id, question_id, event_type, source, query, tags_json, created_at)
		VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), $5, $6, $7, $8, $9)
	`, eventID, user.ID, agentID, questionID, eventType, source, query, tagsRaw, createdAt)
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
	case "feed", "questions", "search", "agent_feed":
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
