// Package api is the owner-scoped HTTP integration surface for the publisher.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"socialpublisher/internal/account"
	"socialpublisher/internal/model"
	"socialpublisher/internal/ops"
	"socialpublisher/internal/provider"
	"socialpublisher/internal/security"
	"socialpublisher/internal/store"
)

type Server struct {
	Store           store.Store
	Adapters        provider.Registry
	JWT             *security.JWT
	Lifecycle       *LifecycleGrant
	Vault           *security.Vault
	Tokens          *account.Manager
	Media           *security.MediaFetcher
	PublicBaseURL   string
	SecureCookies   bool
	Logger          *slog.Logger
	Now             func() time.Time
	AdmissionLimits store.AdmissionLimits
	OperationsToken string
	Operations      interface {
		Snapshot(context.Context) (ops.Snapshot, error)
	}
}
type principalKey struct{}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
func owner(r *http.Request) model.Principal {
	return r.Context().Value(principalKey{}).(model.Principal)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if s.Store.Ping(ctx) != nil {
			writeError(w, 503, "not_ready", "Database is unavailable.")
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("GET /oauth/{platform}/callback", s.oauthCallback)
	mux.HandleFunc("GET /internal/ops", s.operations)
	private := http.NewServeMux()
	private.HandleFunc("GET /v1/platforms", s.platforms)
	private.HandleFunc("POST /v1/connections/{platform}/authorize", s.oauthStart)
	private.HandleFunc("GET /v1/accounts", s.listAccounts)
	private.HandleFunc("DELETE /v1/accounts/{id}", s.disconnect)
	private.HandleFunc("GET /v1/accounts/{id}/capabilities", s.capabilities)
	private.HandleFunc("POST /v1/posts", s.createPost)
	private.HandleFunc("GET /v1/posts", s.listPosts)
	private.HandleFunc("GET /v1/post-receipts/{key}", s.getPostReceipt)
	private.HandleFunc("GET /v1/posts/{id}", s.getPost)
	private.HandleFunc("GET /v1/posts/{id}/events", s.listEvents)
	private.HandleFunc("POST /v1/posts/{id}/cancel", s.cancelPost)
	private.HandleFunc("POST /v1/posts/{id}/reschedule", s.reschedulePost)
	private.HandleFunc("POST /v1/posts/{id}/targets/{target}/retry", s.retryTarget)
	private.HandleFunc("POST /v1/posts/{id}/targets/{target}/resolve", s.resolveTarget)
	mux.Handle("/v1/lifecycle/", s.lifecycleHandler())
	mux.Handle("/v1/", s.authenticate(private))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")
		if s.SecureCookies {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		defer func() {
			if recover() != nil {
				if s.Logger != nil {
					s.Logger.Error("request_panic")
				}
				writeError(w, 500, "internal_error", "The request could not be completed.")
			}
		}()
		// Deliberately no raw URL/access logging: callbacks contain codes and state.
		mux.ServeHTTP(w, r)
	})
}
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Values("Authorization")
		if len(h) != 1 || !strings.HasPrefix(h[0], "Bearer ") {
			writeError(w, 401, "unauthorized", "A valid application access token is required.")
			return
		}
		p, err := s.JWT.Verify(strings.TrimPrefix(h[0], "Bearer "), s.now())
		if err != nil {
			writeError(w, 401, "unauthorized", "A valid application access token is required.")
			return
		}
		release, err := s.admitRequest(r.Context(), p)
		if err != nil {
			s.fail(w, err)
			return
		}
		defer release()
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	})
}
func decodeJSON(w http.ResponseWriter, r *http.Request, value any) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return errors.New("request must use application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(value); err != nil {
		return errors.New("invalid JSON request or unsupported fields")
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return errors.New("request must contain one JSON object")
	}
	return nil
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
func (s *Server) fail(w http.ResponseWriter, err error) {
	var admission *store.AdmissionError
	switch {
	case errors.As(err, &admission):
		writeAdmissionError(w, admission)
	case errors.Is(err, store.ErrAdmissionUnavailable):
		w.Header().Set("Retry-After", "1")
		writeError(w, 503, "admission_unavailable", "The service cannot admit this request right now. Please try again shortly.")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, 404, "not_found", "The resource was not found.")
	case errors.Is(err, store.ErrOAuthState):
		writeError(w, 400, "invalid_oauth_state", "Restart the connection flow in the same browser.")
	case errors.Is(err, store.ErrIdempotency):
		writeError(w, 409, "idempotency_conflict", "Use the same content for this Idempotency-Key or choose a new key.")
	case errors.Is(err, store.ErrConflict):
		writeError(w, 409, "state_conflict", "The resource changed or this action is unavailable in its current state.")
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, 504, "upstream_timeout", "The request timed out; check its status before retrying.")
	default:
		var pe *provider.Error
		if errors.As(err, &pe) {
			status := 422
			if pe.Retryable {
				status = 503
			}
			if pe.AuthExpired {
				status = 409
			}
			message := pe.Message
			if message == "" {
				message = "The destination rejected this operation; check account access and post settings."
			}
			writeError(w, status, pe.Code, message)
			return
		}
		if s.Logger != nil {
			s.Logger.Error("request_failed")
		}
		writeError(w, 500, "internal_error", "The request could not be completed.")
	}
}
func (s *Server) platforms(w http.ResponseWriter, r *http.Request) {
	results := []map[string]any{}
	for _, p := range []model.Platform{model.TikTok, model.X, model.Instagram} {
		_, configured := s.Adapters[p]
		results = append(results, map[string]any{"platform": p, "configured": configured})
	}
	writeJSON(w, 200, map[string]any{"platforms": results})
}
