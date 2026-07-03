package roundtable

import (
	"context"
	"database/sql"
	"strings"
	"unicode"
)

func questionSearchTerms(input string) []string {
	terms := []string{}
	seen := map[string]bool{}
	var current strings.Builder

	flush := func() {
		if current.Len() == 0 {
			return
		}
		term := current.String()
		current.Reset()
		if seen[term] {
			return
		}
		seen[term] = true
		terms = append(terms, term)
	}

	for _, r := range strings.ToLower(input) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return terms
}

func (a *App) rebuildQuestionSearchIndex(ctx context.Context) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM question_search_terms`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, title, body FROM questions`)
	if err != nil {
		return err
	}

	type indexedQuestion struct {
		id    string
		title string
		body  string
	}
	questions := []indexedQuestion{}
	for rows.Next() {
		var question indexedQuestion
		if err := rows.Scan(&question.id, &question.title, &question.body); err != nil {
			_ = rows.Close()
			return err
		}
		questions = append(questions, question)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, question := range questions {
		if err := a.indexQuestionSearchTerms(ctx, tx, question.id, question.title, question.body); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *App) indexQuestionSearchTerms(ctx context.Context, tx *sql.Tx, questionID string, title string, body string) error {
	for _, term := range questionSearchTerms(title + " " + body) {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO question_search_terms (term, question_id)
			VALUES ($1, $2)
			ON CONFLICT DO NOTHING
		`, term, questionID); err != nil {
			return err
		}
	}
	return nil
}
