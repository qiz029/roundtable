package roundtable

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

func (a *App) handleMyAvatar(w http.ResponseWriter, r *http.Request) {
	user, err := a.requireUserFor(r.Context(), r, "manage avatar")
	if err != nil {
		writeError(w, err)
		return
	}
	switch r.Method {
	case http.MethodPost:
		a.uploadUserAvatar(w, r, user)
	case http.MethodDelete:
		a.deleteUserAvatar(w, r, user)
	default:
		writeError(w, errMethodNotAllowed())
	}
}

func (a *App) handleMyAgentAvatar(w http.ResponseWriter, r *http.Request, user currentUser, agentID string) {
	switch r.Method {
	case http.MethodPost:
		a.uploadAgentAvatar(w, r, user, agentID)
	case http.MethodDelete:
		a.deleteAgentAvatar(w, r, user, agentID)
	default:
		writeError(w, errMethodNotAllowed())
	}
}

func (a *App) handleAvatarMedia(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, errMethodNotAllowed())
		return
	}
	if a.avatarStore == nil {
		writeError(w, errAvatarStorageUnavailable())
		return
	}
	opaqueID, ok := singlePathID(r.URL.Path, "/api/v1/media/avatars/")
	if !ok {
		writeError(w, errNotFound("avatar not found"))
		return
	}
	objectKey, err := avatarObjectKeyFromOpaqueID(opaqueID)
	if err != nil {
		writeError(w, errNotFound("avatar not found"))
		return
	}
	avatar, err := a.avatarStore.Get(r.Context(), objectKey)
	if err != nil {
		if errors.Is(err, errAvatarNotFound) {
			writeError(w, errNotFound("avatar not found"))
			return
		}
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", safeAvatarContentType(avatar.ContentType))
	w.Header().Set("Content-Disposition", `inline; filename="avatar.jpg"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(avatar.Content)
}

func (a *App) uploadUserAvatar(w http.ResponseWriter, r *http.Request, user currentUser) {
	body, contentType, err := readAvatarUpload(w, r)
	if err != nil {
		writeError(w, err)
		return
	}
	oldKey, err := a.userAvatarObjectKey(r.Context(), user.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := a.putAvatarObject(r.Context(), body, contentType, func(ctx context.Context, key string, contentType string, now string) error {
		_, err := a.db.ExecContext(ctx, `
			UPDATE users
			SET avatar_object_key = $1,
				avatar_content_type = $2,
				avatar_updated_at = $3,
				avatar_url = ''
			WHERE id = $4
		`, key, contentType, now, user.ID)
		return err
	}, oldKey); err != nil {
		writeError(w, err)
		return
	}
	profile, err := a.userProfileByID(r.Context(), user.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.privateUserProfileResponse(profile))
}

func (a *App) deleteUserAvatar(w http.ResponseWriter, r *http.Request, user currentUser) {
	oldKey, err := a.userAvatarObjectKey(r.Context(), user.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	if _, err := a.db.ExecContext(r.Context(), `
		UPDATE users
		SET avatar_object_key = '',
			avatar_content_type = '',
			avatar_updated_at = NULL,
			avatar_url = ''
		WHERE id = $1
	`, user.ID); err != nil {
		writeError(w, err)
		return
	}
	a.deleteAvatarObjectBestEffort(r.Context(), oldKey)
	profile, err := a.userProfileByID(r.Context(), user.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.privateUserProfileResponse(profile))
}

func (a *App) uploadAgentAvatar(w http.ResponseWriter, r *http.Request, user currentUser, agentID string) {
	body, contentType, err := readAvatarUpload(w, r)
	if err != nil {
		writeError(w, err)
		return
	}
	profile, err := a.ownedAgentProfile(r.Context(), user.ID, agentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, errNotFound("agent not found"))
			return
		}
		writeError(w, err)
		return
	}
	oldKey := profile.AvatarObjectKey
	if err := a.putAvatarObject(r.Context(), body, contentType, func(ctx context.Context, key string, contentType string, now string) error {
		_, err := a.db.ExecContext(ctx, `
			UPDATE agents
			SET avatar_object_key = $1,
				avatar_content_type = $2,
				avatar_updated_at = $3
			WHERE id = $4 AND owner_user_id = $5
		`, key, contentType, now, agentID, user.ID)
		return err
	}, oldKey); err != nil {
		writeError(w, err)
		return
	}
	updated, err := a.ownedAgentProfile(r.Context(), user.ID, agentID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.agentProfileResponse(updated))
}

func (a *App) deleteAgentAvatar(w http.ResponseWriter, r *http.Request, user currentUser, agentID string) {
	profile, err := a.ownedAgentProfile(r.Context(), user.ID, agentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, errNotFound("agent not found"))
			return
		}
		writeError(w, err)
		return
	}
	if _, err := a.db.ExecContext(r.Context(), `
		UPDATE agents
		SET avatar_object_key = '',
			avatar_content_type = '',
			avatar_updated_at = NULL
		WHERE id = $1 AND owner_user_id = $2
	`, agentID, user.ID); err != nil {
		writeError(w, err)
		return
	}
	a.deleteAvatarObjectBestEffort(r.Context(), profile.AvatarObjectKey)
	updated, err := a.ownedAgentProfile(r.Context(), user.ID, agentID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.agentProfileResponse(updated))
}

func readAvatarUpload(w http.ResponseWriter, r *http.Request) ([]byte, string, error) {
	contentType := r.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(contentType), "multipart/form-data") {
		return nil, "", errInvalidInput("avatar upload must be multipart/form-data")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAvatarUploadBytes+1024*1024)
	if err := r.ParseMultipartForm(maxAvatarUploadBytes); err != nil {
		return nil, "", errInvalidInput("avatar file cannot exceed 2MB")
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, _, err := r.FormFile("avatar")
	if err != nil {
		return nil, "", errInvalidInput("avatar file is required")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxAvatarUploadBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(raw) > maxAvatarUploadBytes {
		return nil, "", errInvalidInput("avatar file cannot exceed 2MB")
	}
	return normalizeAvatarImage(raw)
}

func (a *App) putAvatarObject(ctx context.Context, body []byte, contentType string, update func(context.Context, string, string, string) error, oldKey string) error {
	if a.avatarStore == nil {
		return errAvatarStorageUnavailable()
	}
	key, err := newAvatarObjectKey()
	if err != nil {
		return err
	}
	if err := a.avatarStore.Put(ctx, key, contentType, body); err != nil {
		return err
	}
	now := a.now().UTC().Format(time.RFC3339Nano)
	if err := update(ctx, key, contentType, now); err != nil {
		a.deleteAvatarObjectBestEffort(ctx, key)
		return err
	}
	a.deleteAvatarObjectBestEffort(ctx, oldKey)
	return nil
}

func (a *App) deleteAvatarObjectBestEffort(ctx context.Context, key string) {
	if key == "" || a.avatarStore == nil {
		return
	}
	_ = a.avatarStore.Delete(ctx, key)
}

func (a *App) userAvatarObjectKey(ctx context.Context, userID string) (string, error) {
	var key string
	err := a.db.QueryRowContext(ctx, `
		SELECT avatar_object_key FROM users WHERE id = $1 AND status = 'active'
	`, userID).Scan(&key)
	return key, err
}

func (a *App) avatarURL(objectKey string) string {
	if objectKey == "" {
		return ""
	}
	if a.avatarPublicBaseURL != "" {
		return a.avatarPublicBaseURL + "/" + objectKey
	}
	mediaPath := "/api/v1/media/avatars/" + avatarOpaqueID(objectKey)
	if a.avatarMediaBaseURL != "" {
		return a.avatarMediaBaseURL + mediaPath
	}
	return mediaPath
}

func (a *App) agentIdentityResponse(agentID string, agentName string, ownerName string, avatarObjectKey string) map[string]any {
	return map[string]any{
		"id":         agentID,
		"name":       agentName,
		"avatar_url": a.avatarURL(avatarObjectKey),
		"owner_name": ownerName,
	}
}

func (a *App) userIdentityResponse(userID string, displayName string, avatarObjectKey string) map[string]any {
	return map[string]any{
		"id":           userID,
		"display_name": displayName,
		"avatar_url":   a.avatarURL(avatarObjectKey),
	}
}

func safeAvatarContentType(contentType string) string {
	if strings.EqualFold(strings.TrimSpace(contentType), normalizedAvatarContentType) {
		return normalizedAvatarContentType
	}
	return normalizedAvatarContentType
}

func errAvatarStorageUnavailable() apiError {
	return apiError{Status: http.StatusServiceUnavailable, Code: "avatar_storage_unavailable", Message: "avatar storage is not configured"}
}
