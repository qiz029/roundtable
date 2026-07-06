package roundtable

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
)

func (a *App) handleTranslations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, errMethodNotAllowed())
		return
	}

	var req struct {
		ResourceType   string `json:"resource_type"`
		ResourceID     string `json:"resource_id"`
		TargetLanguage string `json:"target_language"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	resourceType := strings.TrimSpace(req.ResourceType)
	resourceID := strings.TrimSpace(req.ResourceID)
	targetLanguage := strings.TrimSpace(req.TargetLanguage)
	if !validResourceType(resourceType) {
		writeError(w, errInvalidInput("resource_type must be question or answer"))
		return
	}
	if resourceID == "" {
		writeError(w, errInvalidInput("resource_id is required"))
		return
	}
	if !validLanguage(targetLanguage) {
		writeError(w, errInvalidInput("target_language must be en or zh-CN"))
		return
	}

	resource, err := a.loadTranslatableResource(r.Context(), resourceType, resourceID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, errNotFound("resource not found"))
			return
		}
		writeError(w, err)
		return
	}
	record, err := a.translationByCacheKey(r.Context(), resource, targetLanguage)
	if err == nil {
		writeJSON(w, http.StatusOK, translationRecordResponse(record))
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		writeError(w, err)
		return
	}

	if _, ok, err := a.optionalUser(r.Context(), r); err != nil {
		writeError(w, err)
		return
	} else if !ok {
		writeError(w, errNotFound("translation not found"))
		return
	}
	if err := a.enqueueTranslationJob(r.Context(), resource, targetLanguage); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, pendingTranslationResponse(resource, targetLanguage))
}
