package roundtable

import (
	"context"
	"database/sql"
)

const maxInterestWeight = 100.0

func (a *App) updateUserInterestsForFeedEvent(ctx context.Context, tx *sql.Tx, userID string, eventType string, questionID string, query string, tags []string, updatedAt string) error {
	switch eventType {
	case "open":
		return a.updateQuestionInterests(ctx, tx, userID, questionID, "open", 4, 1, updatedAt)
	case "dismiss":
		return a.updateQuestionInterests(ctx, tx, userID, questionID, "dismiss", -8, -2, updatedAt)
	case "search":
		return a.applyInterestDeltas(ctx, tx, userID, "search", interestDeltasForTerms(questionSearchTerms(query), 3), updatedAt)
	case "tag_filter":
		return a.applyInterestDeltas(ctx, tx, userID, "tag_filter", interestDeltasForTerms(normalizedQuestionTags(tags), 6), updatedAt)
	default:
		return nil
	}
}

func (a *App) updateUserInterestsForAnswerVote(ctx context.Context, tx *sql.Tx, userID string, answerID string, action string, updatedAt string) error {
	var questionID string
	if err := tx.QueryRowContext(ctx, `SELECT question_id FROM answers WHERE id = $1`, answerID).Scan(&questionID); err != nil {
		return err
	}
	switch action {
	case "like":
		return a.updateQuestionInterests(ctx, tx, userID, questionID, "answer_like", 8, 2, updatedAt)
	case "unlike":
		return a.updateQuestionInterests(ctx, tx, userID, questionID, "answer_unlike", -4, -1, updatedAt)
	default:
		return nil
	}
}

func (a *App) updateQuestionInterests(ctx context.Context, tx *sql.Tx, userID string, questionID string, source string, tagDelta float64, termDelta float64, updatedAt string) error {
	if questionID == "" {
		return nil
	}
	var title, body, tagsRaw string
	if err := tx.QueryRowContext(ctx, `
		SELECT title, body, tags_json
		FROM questions
		WHERE id = $1
	`, questionID).Scan(&title, &body, &tagsRaw); err != nil {
		return err
	}
	deltas := questionInterestDeltas(title, body, decodeStringList(tagsRaw), tagDelta, termDelta)
	return a.applyInterestDeltas(ctx, tx, userID, source, deltas, updatedAt)
}

func questionInterestDeltas(title string, body string, tags []string, tagDelta float64, termDelta float64) map[string]float64 {
	deltas := map[string]float64{}
	tagTerms := normalizedQuestionTags(tags)
	tagSeen := map[string]bool{}
	for _, tag := range tagTerms {
		tagSeen[tag] = true
		deltas[tag] += tagDelta
	}

	terms := questionSearchTerms(title + " " + body)
	if len(terms) > 40 {
		terms = terms[:40]
	}
	for _, term := range terms {
		if tagSeen[term] {
			continue
		}
		deltas[term] += termDelta
	}
	return deltas
}

func interestDeltasForTerms(terms []string, delta float64) map[string]float64 {
	deltas := map[string]float64{}
	for _, term := range terms {
		if term == "" {
			continue
		}
		deltas[term] += delta
	}
	return deltas
}

func (a *App) applyInterestDeltas(ctx context.Context, tx *sql.Tx, userID string, source string, deltas map[string]float64, updatedAt string) error {
	for term, delta := range deltas {
		if term == "" || delta == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO user_interest_terms (user_id, term, weight, source, updated_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (user_id, term) DO UPDATE SET
				weight = LEAST($6, GREATEST($7, user_interest_terms.weight + EXCLUDED.weight)),
				source = EXCLUDED.source,
				updated_at = EXCLUDED.updated_at
		`, userID, term, delta, source, updatedAt, maxInterestWeight, -maxInterestWeight); err != nil {
			return err
		}
	}
	return nil
}
