package api

import (
	"net/http"
	"strconv"
)

// parsePagination reads the shared "limit"/"cursor" query params list
// endpoints accept. limit is left as the caller passed it (including <=0
// or unset, which comes back as 0) — clamping to the default/max page size
// is the orchestrator's job, not the HTTP layer's, so there is exactly one
// place that owns those constants. A limit that fails to parse as an
// integer is treated as unset rather than rejected, since it is just a
// display-size hint.
func parsePagination(r *http.Request) (limit int, cursor string) {
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	cursor = r.URL.Query().Get("cursor")
	return limit, cursor
}
