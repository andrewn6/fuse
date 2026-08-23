package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/folsomintel/fuse/internal/orchestrator"
	"github.com/folsomintel/fuse/internal/secrets"
)

// writeJSON writes v as a JSON body with the given status code. Any
// encoding error after headers have been sent is non-recoverable —
// we log nothing because the server should not depend on a logger for
// this hot path.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes an Error envelope with the given HTTP status and
// code/message. Details are optional and should only carry stable,
// non-sensitive metadata (e.g. ids, counts).
func writeError(w http.ResponseWriter, status int, code, message string, details map[string]string) {
	writeJSON(w, status, Error{Error: ErrorBody{
		Code:    code,
		Message: message,
		Details: details,
	}})
}

// classifyFleetError maps an orchestrator-level error to an HTTP
// status code and machine-readable error code.
func classifyFleetError(err error) (status int, code string) {
	switch {
	case err == nil:
		return http.StatusOK, ""
	case errors.Is(err, orchestrator.ErrTaskAlreadyAssigned):
		return http.StatusConflict, CodeConflict
	case errors.Is(err, orchestrator.ErrVMNotFound),
		errors.Is(err, orchestrator.ErrTaskNotFound),
		errors.Is(err, orchestrator.ErrHostNotFound),
		errors.Is(err, orchestrator.ErrSnapshotNotFound):
		return http.StatusNotFound, CodeNotFound
	case errors.Is(err, orchestrator.ErrNoCapacity),
		errors.Is(err, orchestrator.ErrNoHosts),
		errors.Is(err, orchestrator.ErrHostPinUnschedulable),
		errors.Is(err, orchestrator.ErrNoLabelMatch),
		errors.Is(err, orchestrator.ErrGuestUnavailable):
		return http.StatusServiceUnavailable, CodeUnavailable
	case errors.Is(err, orchestrator.ErrSnapshotQuotaExceeded),
		errors.Is(err, orchestrator.ErrSnapshotInvalidState),
		errors.Is(err, orchestrator.ErrSnapshotHasChildren),
		errors.Is(err, orchestrator.ErrSnapshotNotSeedable),
		errors.Is(err, orchestrator.ErrVMNotRunning),
		errors.Is(err, orchestrator.ErrGPUUnsupported),
		errors.Is(err, orchestrator.ErrHostHasVMs):
		return http.StatusConflict, CodeConflict
	case errors.Is(err, orchestrator.ErrExecUnsupported),
		errors.Is(err, orchestrator.ErrAttachUnsupported),
		errors.Is(err, orchestrator.ErrSnapshotUnsupported),
		errors.Is(err, orchestrator.ErrComputerUnsupported):
		return http.StatusNotImplemented, CodeUnimplemented
	case errors.Is(err, secrets.ErrSecretsValidation),
		errors.Is(err, orchestrator.ErrStartupScriptTimeout),
		errors.Is(err, orchestrator.ErrStartupScriptTimeoutTooLarge),
		errors.Is(err, orchestrator.ErrInvalidCursor):
		return http.StatusBadRequest, CodeInvalidArgument
	default:
		return http.StatusInternalServerError, CodeInternal
	}
}

// writeFleetError maps an orchestrator-level error to the appropriate
// HTTP status and error envelope. Callers that have classified the
// error themselves should use writeError directly.
func writeFleetError(w http.ResponseWriter, err error) {
	if err == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	status, code := classifyFleetError(err)
	writeError(w, status, code, err.Error(), nil)
}

// writeFleetErrorRedacted is like writeFleetError but replaces any
// occurrence of secret values in the error message with [REDACTED].
// Used by handlers that process secrets to prevent leaking values
// in HTTP responses.
func writeFleetErrorRedacted(w http.ResponseWriter, err error, secretMap map[string]string) {
	if err == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	status, code := classifyFleetError(err)
	msg := secrets.RedactSecretValues(err.Error(), secretMap)
	writeError(w, status, code, msg, nil)
}
