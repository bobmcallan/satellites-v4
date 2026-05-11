package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// apiError is the JSON shape returned for all /api/v1/* error responses.
// Mirrors the verb-error shape MCP callers see today so the parity
// integration test (sty_068a6c46 AC-4) can compare decoded outputs
// modulo documented exemptions.
type apiError struct {
	Error string `json:"error"`
}

// writeAPIError maps a Go error to an HTTP status + JSON envelope.
// The status mapping favours fidelity over precision: validation
// errors ("required", "must be") → 400; "not found" / "access denied"
// → 404; everything else → 500. Callers that want a specific status
// should set it via writeAPIStatus.
func writeAPIError(w http.ResponseWriter, err error) {
	if err == nil {
		writeAPIStatus(w, http.StatusInternalServerError, "internal error")
		return
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "required"),
		strings.Contains(lower, "must be"),
		strings.Contains(lower, "invalid"),
		strings.Contains(lower, "decode"):
		writeAPIStatus(w, http.StatusBadRequest, msg)
	case strings.Contains(lower, "not found"),
		strings.Contains(lower, "access denied"):
		writeAPIStatus(w, http.StatusNotFound, msg)
	case errors.Is(err, http.ErrBodyNotAllowed):
		writeAPIStatus(w, http.StatusBadRequest, msg)
	default:
		writeAPIStatus(w, http.StatusInternalServerError, msg)
	}
}

// writeAPIStatus writes the JSON error envelope with the supplied
// HTTP status.
func writeAPIStatus(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiError{Error: msg})
}

// writeAPIJSON marshals payload to JSON and writes it with status 200.
// Marshal failures fall through to writeAPIStatus 500.
func writeAPIJSON(w http.ResponseWriter, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		writeAPIStatus(w, http.StatusInternalServerError, "encode response: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
