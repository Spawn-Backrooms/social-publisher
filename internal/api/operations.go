package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"time"
)

func (s *Server) operations(w http.ResponseWriter, r *http.Request) {
	headers := r.Header.Values("X-Operations-Token")
	if len(headers) != 1 || s.OperationsToken == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "A valid operations token is required.")
		return
	}
	// Compare fixed-length digests to avoid both content and token-length
	// timing leaks. The application bearer JWT has no authority on this route.
	provided := sha256.Sum256([]byte(headers[0]))
	expected := sha256.Sum256([]byte(s.OperationsToken))
	if subtle.ConstantTimeCompare(provided[:], expected[:]) != 1 {
		writeError(w, http.StatusUnauthorized, "unauthorized", "A valid operations token is required.")
		return
	}
	if s.Operations == nil {
		writeError(w, http.StatusServiceUnavailable, "operations_unavailable", "Operational diagnostics are unavailable.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	snapshot, err := s.Operations.Snapshot(ctx)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "operations_unavailable", "Operational diagnostics are unavailable.")
		return
	}
	status := http.StatusOK
	if !snapshot.Healthy {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, snapshot)
}
