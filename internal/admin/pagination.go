package admin

import "strings"

// Pagination backs the reusable "pagination" template partial
// (templates/partials.html) — any handler with a paged list builds one via
// newPagination and passes it to its template as .Pagination.
type Pagination struct {
	Page       int
	TotalPages int
	Total      int
	BaseURL    string // e.g. "/logs" or "/logs?status=error"
	PageSep    string // "?" or "&", whichever BaseURL needs before "page=N"
	HasPrev    bool
	HasNext    bool
	PrevPage   int
	NextPage   int
}

// newPagination clamps page into [1, totalPages] (a stale/out-of-range
// ?page= value, e.g. after the log is cleared, falls back to a valid page
// instead of rendering an empty one) and computes everything the partial
// needs to render without doing arithmetic in the template itself. baseURL
// may already carry its own query string (e.g. "/logs?status=error" for a
// filtered page) — PageSep picks "&" in that case so the partial's own
// "page=N" doesn't clobber it.
func newPagination(page, total, pageSize int, baseURL string) Pagination {
	if pageSize < 1 {
		pageSize = 1
	}
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages < 1 {
		totalPages = 1
	}
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}
	pageSep := "?"
	if strings.Contains(baseURL, "?") {
		pageSep = "&"
	}
	return Pagination{
		Page:       page,
		TotalPages: totalPages,
		Total:      total,
		BaseURL:    baseURL,
		PageSep:    pageSep,
		HasPrev:    page > 1,
		HasNext:    page < totalPages,
		PrevPage:   page - 1,
		NextPage:   page + 1,
	}
}

// Offset is the SQL OFFSET for this page, given the same pageSize passed to
// newPagination.
func (p Pagination) Offset(pageSize int) int {
	return (p.Page - 1) * pageSize
}
