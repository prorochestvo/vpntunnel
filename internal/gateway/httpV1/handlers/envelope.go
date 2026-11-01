package handlers

import (
	"encoding/json"
	"net/http"
)

// WriteError emits the standard JSON error envelope used by every v1 API route.
// It sets Content-Type, X-Proxy-Error, and (when non-empty) X-Request-Id BEFORE
// calling WriteHeader so the headers are part of the response. The body is
// {"error": code, "request_id": requestID}.
func WriteError(w http.ResponseWriter, code string, status int, requestID string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Proxy-Error", code)
	if requestID != "" {
		w.Header().Set("X-Request-Id", requestID)
	}
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]string{
		"error":      code,
		"request_id": requestID,
	})
	_, _ = w.Write(body)
}
