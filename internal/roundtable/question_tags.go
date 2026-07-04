package roundtable

import (
	"context"
	"database/sql"
	"strings"
)

func normalizeQuestionTag(value string) string {
	tag := strings.ToLower(strings.TrimSpace(value))
	tag = strings.TrimPrefix(tag, "#")
	return strings.TrimSpace(tag)
}

func normalizedQuestionTags(values []string) []string {
	tags := []string{}
	seen := map[string]bool{}
	for _, value := range values {
		tag := normalizeQuestionTag(value)
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		tags = append(tags, tag)
	}
	return tags
}

func (a *App) rebuildQuestionTagIndex(ctx context.Context) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM question_tags`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, tags_json FROM questions`)
	if err != nil {
		return err
	}

	type indexedQuestion struct {
		id      string
		tagsRaw string
	}
	questions := []indexedQuestion{}
	for rows.Next() {
		var question indexedQuestion
		if err := rows.Scan(&question.id, &question.tagsRaw); err != nil {
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
		if err := a.indexQuestionTags(ctx, tx, question.id, decodeStringList(question.tagsRaw)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *App) indexQuestionTags(ctx context.Context, tx *sql.Tx, questionID string, tags []string) error {
	for _, tag := range normalizedQuestionTags(tags) {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO question_tags (question_id, tag)
			VALUES ($1, $2)
			ON CONFLICT DO NOTHING
		`, questionID, tag); err != nil {
			return err
		}
	}
	return nil
}
