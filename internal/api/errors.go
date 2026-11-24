package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

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

// RetryAfterSeconds is the Retry-After a retry-later 503 carries.
const RetryAfterSeconds = 5

// writeRetryLater writes a 503 the caller should retry (no capacity, no
// host, restate down, too many operations), with a Retry-After the sdks wait
// at least, plus jitter. a 503 that a retry cannot change (the guest's "no
// display") goes through writeError and carries none.
func writeRetryLater(w http.ResponseWriter, code, message string, details map[string]string) {
	w.Header().Set("Retry-After", strconv.Itoa(RetryAfterSeconds))
	writeError(w, http.StatusServiceUnavailable, code, message, details)
}

// retryLater reports whether a fleet error answered with status is a 503 a
// retry can fix. the guest having no display (ErrGuestUnavailable) is not,
// and neither is a hung guest (ErrGuestTimeout), where a retry only stacks
// another full wait.
func retryLater(status int, err error) bool {
	return status == http.StatusServiceUnavailable &&
		!errors.Is(err, orchestrator.ErrGuestUnavailable) &&
		!errors.Is(err, orchestrator.ErrGuestTimeout)
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
	case errors.Is(err, orchestrator.ErrTaskAlreadyAssigned),
		errors.Is(err, orchestrator.ErrBuildConflict),
		// a later create of the same task id holds the vm on its host, or
		// replaced it while this request waited.
		errors.Is(err, orchestrator.ErrOtherIncarnation),
		orchestrator.IsStaleEpoch(err):
		return http.StatusConflict, CodeConflict
	case errors.Is(err, orchestrator.ErrEnvLocked):
		return http.StatusLocked, CodeLocked
	case errors.Is(err, orchestrator.ErrNotTakeoverHolder):
		return http.StatusForbidden, CodeForbidden
	case errors.Is(err, orchestrator.ErrVMNotFound),
		errors.Is(err, orchestrator.ErrTaskNotFound),
		errors.Is(err, orchestrator.ErrHostNotFound),
		errors.Is(err, orchestrator.ErrSnapshotNotFound),
		errors.Is(err, orchestrator.ErrInterventionNotFound),
		errors.Is(err, orchestrator.ErrRecordingNotFound),
		errors.Is(err, orchestrator.ErrBuildNotFound),
		errors.Is(err, orchestrator.ErrImageNotFound):
		return http.StatusNotFound, CodeNotFound
	case errors.Is(err, orchestrator.ErrNoCapacity),
		errors.Is(err, orchestrator.ErrNoHosts),
		errors.Is(err, orchestrator.ErrHostPinUnschedulable),
		errors.Is(err, orchestrator.ErrNoLabelMatch),
		errors.Is(err, orchestrator.ErrGuestUnavailable),
		errors.Is(err, orchestrator.ErrGuestTimeout),
		errors.Is(err, orchestrator.ErrBuildNotStarted),
		errors.Is(err, orchestrator.ErrImageHostFailed),
		errors.Is(err, orchestrator.ErrRestateUnavailable):
		return http.StatusServiceUnavailable, CodeUnavailable
	case errors.Is(err, orchestrator.ErrImageCacheFull):
		// the host is out of room until something is destroyed, the same
		// retry-later shape as unavailable.
		return http.StatusInsufficientStorage, CodeUnavailable
	case errors.Is(err, orchestrator.ErrSnapshotQuotaExceeded),
		errors.Is(err, orchestrator.ErrSnapshotInvalidState),
		errors.Is(err, orchestrator.ErrSnapshotHasChildren),
		errors.Is(err, orchestrator.ErrSnapshotNotSeedable),
		errors.Is(err, orchestrator.ErrLiveMigrateRefused),
		errors.Is(err, orchestrator.ErrVMNotRunning),
		errors.Is(err, orchestrator.ErrGPUUnsupported),
		errors.Is(err, orchestrator.ErrHostHasVMs),
		errors.Is(err, orchestrator.ErrInterventionResolved),
		errors.Is(err, orchestrator.ErrNoTakeover),
		errors.Is(err, orchestrator.ErrTakeoverHeld),
		errors.Is(err, orchestrator.ErrImageNotReady):
		return http.StatusConflict, CodeConflict
	case errors.Is(err, orchestrator.ErrExecUnsupported),
		errors.Is(err, orchestrator.ErrAttachUnsupported),
		errors.Is(err, orchestrator.ErrSnapshotUnsupported),
		errors.Is(err, orchestrator.ErrComputerUnsupported):
		return http.StatusNotImplemented, CodeUnimplemented
	case errors.Is(err, secrets.ErrSecretsValidation),
		errors.Is(err, orchestrator.ErrStartupScriptTimeout),
		errors.Is(err, orchestrator.ErrStartupScriptTimeoutTooLarge),
		errors.Is(err, orchestrator.ErrInvalidCursor),
		errors.Is(err, orchestrator.ErrHitlNotDeclared),
		errors.Is(err, orchestrator.ErrHitlSecretMissing),
		errors.Is(err, orchestrator.ErrHitlNeedsKey),
		errors.Is(err, orchestrator.ErrInvalidIntervention),
		errors.Is(err, orchestrator.ErrInvalidImage),
		errors.Is(err, orchestrator.ErrImageUnsupported):
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
	markSettled(w, err)
	status, code := classifyFleetError(err)
	var details map[string]string
	var locked *orchestrator.LockedError
	if errors.As(err, &locked) {
		// the sdks wait on this intervention, then retry the call.
		details = map[string]string{"intervention_id": locked.InterventionID, "kind": string(locked.Kind)}
	}
	if retryLater(status, err) {
		writeRetryLater(w, code, err.Error(), details)
		return
	}
	writeError(w, status, code, err.Error(), details)
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
	markSettled(w, err)
	status, code := classifyFleetError(err)
	msg := secrets.RedactSecretValues(err.Error(), secretMap)
	if retryLater(status, err) {
		writeRetryLater(w, code, msg, nil)
		return
	}
	writeError(w, status, code, msg, nil)
}
