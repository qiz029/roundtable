package roundtable

import (
	"net/http"
	"strconv"
)

const (
	defaultPageLimit = 100
	maxPageLimit     = 100
)

type paginationParams struct {
	Limit  int
	Offset int
}

func paginationFromRequest(r *http.Request) (paginationParams, error) {
	page := paginationParams{
		Limit:  defaultPageLimit,
		Offset: 0,
	}
	query := r.URL.Query()

	if raw := query.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > maxPageLimit {
			return paginationParams{}, errInvalidInput("limit must be an integer between 1 and 100")
		}
		page.Limit = limit
	}
	if raw := query.Get("offset"); raw != "" {
		offset, err := strconv.Atoi(raw)
		if err != nil || offset < 0 {
			return paginationParams{}, errInvalidInput("offset must be a non-negative integer")
		}
		page.Offset = offset
	}

	return page, nil
}

func trimPaginatedItems(items []map[string]any, page paginationParams) ([]map[string]any, bool) {
	if len(items) <= page.Limit {
		return items, false
	}
	return items[:page.Limit], true
}

func paginateItems(items []map[string]any, page paginationParams) ([]map[string]any, bool) {
	if page.Offset >= len(items) {
		return []map[string]any{}, false
	}
	end := page.Offset + page.Limit
	if end >= len(items) {
		return items[page.Offset:], false
	}
	return items[page.Offset:end], true
}

func paginationResponse(page paginationParams, itemCount int, hasMore bool) map[string]any {
	var nextOffset any
	if hasMore {
		nextOffset = page.Offset + itemCount
	}
	return map[string]any{
		"limit":       page.Limit,
		"offset":      page.Offset,
		"has_more":    hasMore,
		"next_offset": nextOffset,
	}
}
