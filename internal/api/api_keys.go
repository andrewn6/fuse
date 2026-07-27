package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/folsomintel/fuse/internal/apikeys"
	"github.com/go-chi/chi/v5"
)

// APIKeyStore is the subset of the orchestrator's API key store that the
// REST handlers depend on. It is an interface so handlers can be tested
// with a stub and so the api package does not couple to *sql.DB. It also
// satisfies APIKeyAuthenticator (via Authenticate), so the same value
// wires into BearerAuth.
type APIKeyStore interface {
	Authenticate(ctx context.Context, rawToken string) (string, bool)
	Create(ctx context.Context, label string, now time.Time) (apikeys.APIKeyRecord, string, error)
	List(ctx context.Context) ([]apikeys.APIKeyRecord, error)
	Revoke(ctx context.Context, id string, now time.Time) error
}

// requireMaster writes a 403 and returns false unless the request
// authenticated with the master token. Key management is operator-only: a
// caller holding an API key must not be able to mint, list, or revoke
// keys. In insecure mode (no principal set) the request is treated as
// master, matching BearerAuth's open pass-through.
func (h *Handler) requireMaster(w http.ResponseWriter, r *http.Request) bool {
	return h.requireMasterToken(w, r, "API key management requires the master token")
}

// requireMasterToken is requireMaster with a caller-supplied message, so
// operator-only endpoints other than key management can explain themselves.
func (h *Handler) requireMasterToken(w http.ResponseWriter, r *http.Request, msg string) bool {
	p, ok := PrincipalFromContext(r.Context())
	if !ok || p.Master {
		return true // insecure mode, or genuine master
	}
	writeError(w, http.StatusForbidden, CodeUnauthorized, msg, nil)
	return false
}

func toAPIKey(rec apikeys.APIKeyRecord) APIKey {
	return APIKey{
		ID:         rec.ID,
		Label:      rec.Label,
		CreatedAt:  rec.CreatedAt,
		LastUsedAt: rec.LastUsedAt,
		RevokedAt:  rec.RevokedAt,
	}
}

// createAPIKey mints a new API key and returns the raw secret exactly once.
//
//	@Summary		Create API key
//	@Description	Mints a new revocable API key. The raw key is returned once and cannot be recovered.
//	@Tags			api-keys
//	@Accept			json
//	@Produce		json
//	@Param			body	body		CreateAPIKeyRequest	false	"Optional label"
//	@Success		201		{object}	CreateAPIKeyResponse
//	@Failure		401		{object}	Error
//	@Failure		403		{object}	Error
//	@Router			/v1/api-keys [post]
func (h *Handler) createAPIKey(w http.ResponseWriter, r *http.Request) {
	if !h.requireMaster(w, r) {
		return
	}

	var req CreateAPIKeyRequest
	// Body is optional; only reject genuinely malformed or oversized JSON.
	if !decodeOptionalJSON(w, r, MaxJSONBodyBytes, &req) {
		return
	}

	rec, rawKey, err := h.APIKeys.Create(r.Context(), req.Label, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal,
			"could not create api key", nil)
		return
	}

	writeJSON(w, http.StatusCreated, CreateAPIKeyResponse{
		APIKey: toAPIKey(rec),
		Key:    rawKey,
	})
}

// listAPIKeys returns metadata for all keys. It never returns key secrets.
//
//	@Summary		List API keys
//	@Description	Lists API key metadata (never the keys themselves).
//	@Tags			api-keys
//	@Produce		json
//	@Success		200	{object}	APIKeyList
//	@Failure		401	{object}	Error
//	@Failure		403	{object}	Error
//	@Router			/v1/api-keys [get]
func (h *Handler) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	if !h.requireMaster(w, r) {
		return
	}

	recs, err := h.APIKeys.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal,
			"could not list api keys", nil)
		return
	}

	keys := make([]APIKey, 0, len(recs))
	for _, rec := range recs {
		keys = append(keys, toAPIKey(rec))
	}
	writeJSON(w, http.StatusOK, APIKeyList{APIKeys: keys})
}

// revokeAPIKey revokes the key with the given id.
//
//	@Summary		Revoke API key
//	@Description	Revokes an API key by id. Idempotent.
//	@Tags			api-keys
//	@Param			id	path	string	true	"API key id"
//	@Success		204
//	@Failure		401	{object}	Error
//	@Failure		403	{object}	Error
//	@Failure		404	{object}	Error
//	@Router			/v1/api-keys/{id} [delete]
func (h *Handler) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	if !h.requireMaster(w, r) {
		return
	}

	id := chi.URLParam(r, "id")
	err := h.APIKeys.Revoke(r.Context(), id, time.Now().UTC())
	switch {
	case errors.Is(err, apikeys.ErrAPIKeyNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, "api key not found", nil)
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, CodeInternal,
			"could not revoke api key", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
