package roundtable

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
)

const maxCommentBodyChars = 2000

func (a *App) handleAnswerComments(w http.ResponseWriter, r *http.Request, answerID string) {
	switch r.Method {
	case http.MethodGet:
		a.listAnswerComments(w, r, answerID)
	case http.MethodPost:
		a.createAnswerComment(w, r, answerID)
	default:
		writeError(w, errMethodNotAllowed())
	}
}

func (a *App) handleCommentAction(w http.ResponseWriter, r *http.Request) {
	commentID, ok := singlePathID(r.URL.Path, "/api/v1/comments/")
	if !ok {
		writeError(w, errNotFound("comment action not found"))
		return
	}
	if r.Method != http.MethodDelete {
		writeError(w, errMethodNotAllowed())
		return
	}
	user, err := a.requireUserFor(r.Context(), r, "delete comments")
	if err != nil {
		writeError(w, err)
		return
	}
	deletedAt := a.now().UTC().Format(time.RFC3339Nano)
	result, err := a.db.ExecContext(r.Context(), `
		UPDATE answer_comments
		SET deleted_at = $1
		WHERE id = $2 AND author_user_id = $3 AND deleted_at IS NULL
	`, deletedAt, commentID, user.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		if a.commentExists(r.Context(), commentID) {
			writeError(w, errForbidden("only the comment author can delete this comment"))
			return
		}
		writeError(w, errNotFound("comment not found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"comment_id": commentID,
		"deleted":    true,
	})
}

func (a *App) listAnswerComments(w http.ResponseWriter, r *http.Request, answerID string) {
	page, err := paginationFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if !a.answerExists(r.Context(), answerID) {
		writeError(w, errNotFound("answer not found"))
		return
	}
	comments, hasMore, err := a.commentsForAnswer(r.Context(), answerID, page)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":      comments,
		"pagination": paginationResponse(page, len(comments), hasMore),
	})
}

func (a *App) createAnswerComment(w http.ResponseWriter, r *http.Request, answerID string) {
	user, err := a.requireUserFor(r.Context(), r, "comment on answers")
	if err != nil {
		writeError(w, err)
		return
	}
	var req struct {
		Body             string `json:"body"`
		ReplyToCommentID string `json:"reply_to_comment_id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	body := strings.TrimSpace(req.Body)
	if body == "" {
		writeError(w, errInvalidInput("body is required"))
		return
	}
	if len([]rune(body)) > maxCommentBodyChars {
		writeError(w, errInvalidInput("body is too long"))
		return
	}
	if !a.answerExists(r.Context(), answerID) {
		writeError(w, errNotFound("answer not found"))
		return
	}
	replyToCommentID := strings.TrimSpace(req.ReplyToCommentID)
	if replyToCommentID != "" {
		if err := a.ensureReplyCommentBelongsToAnswer(r.Context(), answerID, replyToCommentID); err != nil {
			writeError(w, err)
			return
		}
	}

	commentID, err := newID("cmt")
	if err != nil {
		writeError(w, err)
		return
	}
	mentionsRaw, err := encodeStringList(nil)
	if err != nil {
		writeError(w, err)
		return
	}
	createdAt := a.now().UTC().Format(time.RFC3339Nano)
	_, err = a.db.ExecContext(r.Context(), `
		INSERT INTO answer_comments (id, answer_id, author_user_id, reply_to_comment_id, body, mentions_json, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, commentID, answerID, user.ID, nullString(replyToCommentID), body, mentionsRaw, createdAt)
	if err != nil {
		writeError(w, err)
		return
	}
	comment, err := a.commentByID(r.Context(), commentID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, comment)
}

func (a *App) commentsForAnswer(ctx context.Context, answerID string, page paginationParams) ([]map[string]any, bool, error) {
	rows, err := a.db.QueryContext(ctx, `
		SELECT c.id, c.answer_id, c.reply_to_comment_id, c.body, c.mentions_json, c.created_at,
			u.id, u.display_name, u.avatar_object_key
		FROM answer_comments c
		JOIN users u ON u.id = c.author_user_id
		WHERE c.answer_id = $1 AND c.deleted_at IS NULL
		ORDER BY c.created_at ASC, c.id ASC
		LIMIT $2 OFFSET $3
	`, answerID, page.Limit+1, page.Offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	comments := []map[string]any{}
	for rows.Next() {
		comment, err := a.scanAnswerComment(rows)
		if err != nil {
			return nil, false, err
		}
		comments = append(comments, comment)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	comments, hasMore := trimPaginatedItems(comments, page)
	return comments, hasMore, nil
}

type answerCommentScanner interface {
	Scan(dest ...any) error
}

func (a *App) scanAnswerComment(scanner answerCommentScanner) (map[string]any, error) {
	var commentID, answerID, body, mentionsRaw, createdAt, authorID, authorName, authorAvatarObjectKey string
	var replyToCommentID sql.NullString
	if err := scanner.Scan(&commentID, &answerID, &replyToCommentID, &body, &mentionsRaw, &createdAt, &authorID, &authorName, &authorAvatarObjectKey); err != nil {
		return nil, err
	}
	var replyTo any
	if replyToCommentID.Valid {
		replyTo = replyToCommentID.String
	}
	return map[string]any{
		"id":                  commentID,
		"answer_id":           answerID,
		"reply_to_comment_id": replyTo,
		"body":                body,
		"mentions":            decodeStringList(mentionsRaw),
		"created_at":          createdAt,
		"author":              a.userIdentityResponse(authorID, authorName, authorAvatarObjectKey),
	}, nil
}

func (a *App) commentByID(ctx context.Context, commentID string) (map[string]any, error) {
	row := a.db.QueryRowContext(ctx, `
		SELECT c.id, c.answer_id, c.reply_to_comment_id, c.body, c.mentions_json, c.created_at,
			u.id, u.display_name, u.avatar_object_key
		FROM answer_comments c
		JOIN users u ON u.id = c.author_user_id
		WHERE c.id = $1 AND c.deleted_at IS NULL
	`, commentID)
	return a.scanAnswerComment(row)
}

func (a *App) ensureReplyCommentBelongsToAnswer(ctx context.Context, answerID string, commentID string) error {
	var exists int
	err := a.db.QueryRowContext(ctx, `
		SELECT 1
		FROM answer_comments
		WHERE id = $1 AND answer_id = $2 AND deleted_at IS NULL
	`, commentID, answerID).Scan(&exists)
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return errInvalidInput("reply_to_comment_id must reference an active comment on the same answer")
	}
	return err
}

func (a *App) commentExists(ctx context.Context, commentID string) bool {
	var exists int
	err := a.db.QueryRowContext(ctx, `
		SELECT 1 FROM answer_comments WHERE id = $1 AND deleted_at IS NULL
	`, commentID).Scan(&exists)
	return err == nil
}
