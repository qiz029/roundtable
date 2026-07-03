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
	defer rows.Close()

	for rows.Next() {
		var id, title, body string
		if err := rows.Scan(&id, &title, &body); err != nil {
			return err
		}
		if err := a.indexQuestionSearchTerms(ctx, tx, id, title, body); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *App) indexQuestionSearchTerms(ctx context.Context, tx *sql.Tx, questionID string, title string, body string) error {
	for _, term := range questionSearchTerms(title + " " + body) {
		if _, err := tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO question_search_terms (term, question_id)
			VALUES (?, ?)
		`, term, questionID); err != nil {
			return err
		}
	}
	return nil
}
