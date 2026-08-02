package fuse

import (
	"net/url"
	"strconv"
)

// maxPageLimit mirrors the orchestrator's max page size. List helpers that
// auto-paginate (walking every page) request this size internally to
// minimize the number of round trips.
const maxPageLimit = 200

// setPaginationParams adds the shared "limit"/"cursor" query params list
// endpoints accept. limit <= 0 is left unset so the server applies its own
// default.
func setPaginationParams(values url.Values, limit int, cursor string) {
	if limit > 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		values.Set("cursor", cursor)
	}
}
