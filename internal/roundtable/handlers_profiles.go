package roundtable

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type socialLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

type userProfile struct {
	ID                string
	Email             string
	DisplayName       string
	FullName          string
	Bio               string
	Background        string
	AvatarURL         string
	AvatarObjectKey   string
	AvatarContentType string
	AvatarUpdatedAt   sql.NullString
	WebsiteURL        string
	SocialLinksRaw    string
	IsSeedUser        bool
	EmailVerified     bool
	FollowerCount     int
	FollowingCount    int
}

type userProfileScanner interface {
	Scan(dest ...any) error
}

func (a *App) handleMyProfile(w http.ResponseWriter, r *http.Request) {
	user, err := a.requireUserFor(r.Context(), r, "manage profile")
	if err != nil {
		writeError(w, err)
		return
	}

	switch r.Method {
	case http.MethodGet:
		a.getMyProfile(w, r, user)
	case http.MethodPatch:
		a.updateMyProfile(w, r, user)
	default:
		writeError(w, errMethodNotAllowed())
	}
}

func (a *App) handleUserProfile(w http.ResponseWriter, r *http.Request) {
	userID, action, ok := twoPartAction(r.URL.Path, "/api/v1/users/")
	if !ok {
		writeError(w, errNotFound("user action not found"))
		return
	}

	switch action {
	case "profile":
		if r.Method != http.MethodGet {
			writeError(w, errMethodNotAllowed())
			return
		}
		a.getPublicProfile(w, r, userID)
	case "follow":
		a.handleUserFollow(w, r, userID)
	case "followers":
		if r.Method != http.MethodGet {
			writeError(w, errMethodNotAllowed())
			return
		}
		a.listUserFollowers(w, r, userID)
	case "following":
		if r.Method != http.MethodGet {
			writeError(w, errMethodNotAllowed())
			return
		}
		a.listUserFollowing(w, r, userID)
	case "scores":
		a.handleUserScore(w, r, userID)
	default:
		writeError(w, errNotFound("user action not found"))
	}
}

func (a *App) getMyProfile(w http.ResponseWriter, r *http.Request, user currentUser) {
	profile, err := a.userProfileByID(r.Context(), user.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.privateUserProfileResponse(profile))
}

func (a *App) getPublicProfile(w http.ResponseWriter, r *http.Request, userID string) {
	profile, err := a.userProfileByID(r.Context(), userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, errNotFound("user profile not found"))
			return
		}
		writeError(w, err)
		return
	}
	viewerFollowing, err := a.viewerFollowing(r.Context(), r, profile.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	resp := a.publicUserProfileResponse(profile)
	resp["viewer_following"] = viewerFollowing
	writeJSON(w, http.StatusOK, resp)
}

func (a *App) updateMyProfile(w http.ResponseWriter, r *http.Request, user currentUser) {
	profile, err := a.userProfileByID(r.Context(), user.ID)
	if err != nil {
		writeError(w, err)
		return
	}

	var req struct {
		DisplayName *string         `json:"display_name"`
		FullName    *string         `json:"full_name"`
		Bio         *string         `json:"bio"`
		Background  *string         `json:"background"`
		AvatarURL   json.RawMessage `json:"avatar_url"`
		WebsiteURL  *string         `json:"website_url"`
		SocialLinks []socialLink    `json:"social_links"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}

	if req.DisplayName != nil {
		displayName := strings.TrimSpace(*req.DisplayName)
		if displayName == "" {
			writeError(w, errInvalidInput("display_name cannot be blank"))
			return
		}
		profile.DisplayName = displayName
	}
	if req.FullName != nil {
		profile.FullName = strings.TrimSpace(*req.FullName)
	}
	if req.Bio != nil {
		profile.Bio = strings.TrimSpace(*req.Bio)
	}
	if req.Background != nil {
		profile.Background = strings.TrimSpace(*req.Background)
	}
	if len(req.AvatarURL) > 0 {
		writeError(w, errInvalidInput("avatar_url is managed by avatar upload endpoints"))
		return
	}
	if req.WebsiteURL != nil {
		profile.WebsiteURL = strings.TrimSpace(*req.WebsiteURL)
	}
	if req.SocialLinks != nil {
		linksRaw, err := encodeSocialLinks(req.SocialLinks)
		if err != nil {
			writeError(w, err)
			return
		}
		profile.SocialLinksRaw = linksRaw
	}

	if _, err := a.db.ExecContext(r.Context(), `
		UPDATE users
		SET display_name = $1,
			full_name = $2,
			bio = $3,
			background = $4,
			avatar_url = '',
			website_url = $5,
			social_links_json = $6
		WHERE id = $7
	`, profile.DisplayName, profile.FullName, profile.Bio, profile.Background,
		profile.WebsiteURL, profile.SocialLinksRaw, profile.ID); err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, a.privateUserProfileResponse(profile))
}

func (a *App) handleUserFollow(w http.ResponseWriter, r *http.Request, followeeUserID string) {
	user, err := a.requireUserFor(r.Context(), r, "follow users")
	if err != nil {
		writeError(w, err)
		return
	}
	if user.ID == followeeUserID {
		writeError(w, errInvalidInput("cannot follow yourself"))
		return
	}
	if !a.activeUserExists(r.Context(), followeeUserID) {
		writeError(w, errNotFound("user profile not found"))
		return
	}

	switch r.Method {
	case http.MethodPost:
		if _, err := a.db.ExecContext(r.Context(), `
			INSERT INTO user_follows (follower_user_id, followee_user_id, created_at)
			VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING
		`, user.ID, followeeUserID, a.now().UTC().Format(time.RFC3339Nano)); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, a.followResponse(r.Context(), followeeUserID, true))
	case http.MethodDelete:
		if _, err := a.db.ExecContext(r.Context(), `
			DELETE FROM user_follows WHERE follower_user_id = $1 AND followee_user_id = $2
		`, user.ID, followeeUserID); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, a.followResponse(r.Context(), followeeUserID, false))
	default:
		writeError(w, errMethodNotAllowed())
	}
}

func (a *App) listUserFollowers(w http.ResponseWriter, r *http.Request, userID string) {
	page, err := paginationFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if !a.activeUserExists(r.Context(), userID) {
		writeError(w, errNotFound("user profile not found"))
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT u.id, u.email, u.display_name, u.full_name, u.bio, u.background,
			u.avatar_url, u.avatar_object_key, u.avatar_content_type, u.avatar_updated_at,
			u.website_url, u.social_links_json, u.is_seed_user, u.email_verified_at,
			(SELECT COUNT(*) FROM user_follows WHERE followee_user_id = u.id) AS follower_count,
			(SELECT COUNT(*) FROM user_follows WHERE follower_user_id = u.id) AS following_count
		FROM user_follows f
		JOIN users u ON u.id = f.follower_user_id
		WHERE f.followee_user_id = $1 AND u.status = 'active'
		ORDER BY f.created_at DESC
		LIMIT $2 OFFSET $3
	`, userID, page.Limit+1, page.Offset)
	if err != nil {
		writeError(w, err)
		return
	}
	defer rows.Close()

	items, err := a.scanPublicUserProfiles(rows)
	if err != nil {
		writeError(w, err)
		return
	}
	items, hasMore := trimPaginatedItems(items, page)
	writeJSON(w, http.StatusOK, map[string]any{
		"items":      items,
		"pagination": paginationResponse(page, len(items), hasMore),
	})
}

func (a *App) listUserFollowing(w http.ResponseWriter, r *http.Request, userID string) {
	page, err := paginationFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	if !a.activeUserExists(r.Context(), userID) {
		writeError(w, errNotFound("user profile not found"))
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT u.id, u.email, u.display_name, u.full_name, u.bio, u.background,
			u.avatar_url, u.avatar_object_key, u.avatar_content_type, u.avatar_updated_at,
			u.website_url, u.social_links_json, u.is_seed_user, u.email_verified_at,
			(SELECT COUNT(*) FROM user_follows WHERE followee_user_id = u.id) AS follower_count,
			(SELECT COUNT(*) FROM user_follows WHERE follower_user_id = u.id) AS following_count
		FROM user_follows f
		JOIN users u ON u.id = f.followee_user_id
		WHERE f.follower_user_id = $1 AND u.status = 'active'
		ORDER BY f.created_at DESC
		LIMIT $2 OFFSET $3
	`, userID, page.Limit+1, page.Offset)
	if err != nil {
		writeError(w, err)
		return
	}
	defer rows.Close()

	items, err := a.scanPublicUserProfiles(rows)
	if err != nil {
		writeError(w, err)
		return
	}
	items, hasMore := trimPaginatedItems(items, page)
	writeJSON(w, http.StatusOK, map[string]any{
		"items":      items,
		"pagination": paginationResponse(page, len(items), hasMore),
	})
}

func (a *App) userProfileByID(ctx context.Context, userID string) (userProfile, error) {
	row := a.db.QueryRowContext(ctx, `
		SELECT u.id, u.email, u.display_name, u.full_name, u.bio, u.background,
			u.avatar_url, u.avatar_object_key, u.avatar_content_type, u.avatar_updated_at,
			u.website_url, u.social_links_json, u.is_seed_user, u.email_verified_at,
			(SELECT COUNT(*) FROM user_follows WHERE followee_user_id = u.id) AS follower_count,
			(SELECT COUNT(*) FROM user_follows WHERE follower_user_id = u.id) AS following_count
		FROM users u
		WHERE u.id = $1 AND u.status = 'active'
	`, userID)
	return scanUserProfile(row)
}

func (a *App) scanPublicUserProfiles(rows *sql.Rows) ([]map[string]any, error) {
	items := []map[string]any{}
	for rows.Next() {
		profile, err := scanUserProfile(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, a.publicUserProfileResponse(profile))
	}
	return items, rows.Err()
}

func scanUserProfile(scanner userProfileScanner) (userProfile, error) {
	var profile userProfile
	var emailVerifiedAt sql.NullString
	err := scanner.Scan(&profile.ID, &profile.Email, &profile.DisplayName, &profile.FullName,
		&profile.Bio, &profile.Background, &profile.AvatarURL, &profile.AvatarObjectKey,
		&profile.AvatarContentType, &profile.AvatarUpdatedAt, &profile.WebsiteURL,
		&profile.SocialLinksRaw, &profile.IsSeedUser, &emailVerifiedAt, &profile.FollowerCount, &profile.FollowingCount)
	profile.EmailVerified = emailVerifiedAt.Valid
	return profile, err
}

func (a *App) privateUserProfileResponse(profile userProfile) map[string]any {
	resp := a.publicUserProfileResponse(profile)
	resp["email"] = profile.Email
	resp["email_verified"] = profile.EmailVerified
	return resp
}

func (a *App) publicUserProfileResponse(profile userProfile) map[string]any {
	return map[string]any{
		"id":              profile.ID,
		"display_name":    profile.DisplayName,
		"is_seed_user":    profile.IsSeedUser,
		"full_name":       profile.FullName,
		"bio":             profile.Bio,
		"background":      profile.Background,
		"avatar_url":      a.avatarURL(profile.AvatarObjectKey),
		"website_url":     profile.WebsiteURL,
		"social_links":    decodeSocialLinks(profile.SocialLinksRaw),
		"follower_count":  profile.FollowerCount,
		"following_count": profile.FollowingCount,
	}
}

func (a *App) activeUserExists(ctx context.Context, userID string) bool {
	var exists int
	err := a.db.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = $1 AND status = 'active'`, userID).Scan(&exists)
	return err == nil
}

func (a *App) followResponse(ctx context.Context, followeeUserID string, following bool) map[string]any {
	return map[string]any{
		"user_id":        followeeUserID,
		"following":      following,
		"follower_count": a.followerCount(ctx, followeeUserID),
	}
}

func (a *App) followerCount(ctx context.Context, userID string) int {
	var count int
	if err := a.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM user_follows WHERE followee_user_id = $1
	`, userID).Scan(&count); err != nil {
		return 0
	}
	return count
}

func (a *App) userFollows(ctx context.Context, followerUserID string, followeeUserID string) bool {
	var exists int
	err := a.db.QueryRowContext(ctx, `
		SELECT 1 FROM user_follows WHERE follower_user_id = $1 AND followee_user_id = $2
	`, followerUserID, followeeUserID).Scan(&exists)
	return err == nil
}

func (a *App) optionalUser(ctx context.Context, r *http.Request) (currentUser, bool, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return currentUser{}, false, nil
	}
	user, err := a.requireUser(ctx, r)
	if err != nil {
		var apiErr apiError
		if errors.As(err, &apiErr) {
			return currentUser{}, false, nil
		}
		return currentUser{}, false, err
	}
	return user, true, nil
}

func (a *App) viewerFollowing(ctx context.Context, r *http.Request, followeeUserID string) (bool, error) {
	user, ok, err := a.optionalUser(ctx, r)
	if err != nil || !ok {
		return false, err
	}
	if user.ID == followeeUserID {
		return false, nil
	}
	return a.userFollows(ctx, user.ID, followeeUserID), nil
}

func encodeSocialLinks(links []socialLink) (string, error) {
	if links == nil {
		links = []socialLink{}
	}
	if len(links) > 20 {
		return "", errInvalidInput("social_links cannot contain more than 20 entries")
	}

	normalized := make([]socialLink, 0, len(links))
	for _, link := range links {
		label := strings.TrimSpace(link.Label)
		url := strings.TrimSpace(link.URL)
		if label == "" || url == "" {
			return "", errInvalidInput("social_links entries require label and url")
		}
		normalized = append(normalized, socialLink{
			Label: label,
			URL:   url,
		})
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("marshal social links: %w", err)
	}
	return string(raw), nil
}

func decodeSocialLinks(raw string) []socialLink {
	var links []socialLink
	if raw == "" {
		return []socialLink{}
	}
	if err := json.Unmarshal([]byte(raw), &links); err != nil {
		return []socialLink{}
	}
	if links == nil {
		return []socialLink{}
	}
	return links
}
