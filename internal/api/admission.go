package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"socialpublisher/internal/model"
	"socialpublisher/internal/store"
)

func admissionFailure(err error) error {
	var denied *store.AdmissionError
	if errors.As(err, &denied) || errors.Is(err, store.ErrNotFound) {
		return err
	}
	return store.ErrAdmissionUnavailable
}

func (s *Server) admissionRelease(ctx context.Context, owner model.Principal, lease store.AdmissionLease) func() {
	return func() {
		// Request cancellation must not retain a lease until expiry when the
		// database is healthy. A failed cleanup still expires in database time.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		if err := s.Store.ReleaseAdmission(cleanup, owner, lease.Token); err != nil && s.Logger != nil {
			s.Logger.Warn("admission_release_failed")
		}
	}
}

func (s *Server) admitRequest(ctx context.Context, owner model.Principal) (func(), error) {
	lease, err := s.Store.AdmitRequest(ctx, owner, s.AdmissionLimits)
	if err != nil {
		return nil, admissionFailure(err)
	}
	return s.admissionRelease(ctx, owner, lease), nil
}

func (s *Server) admitProvider(ctx context.Context, owner model.Principal, platform model.Platform, providerID string) (func(), error) {
	lease, err := s.Store.AdmitProviderOperation(ctx, owner, platform, providerID, s.AdmissionLimits)
	if err != nil {
		return nil, admissionFailure(err)
	}
	return s.admissionRelease(ctx, owner, lease), nil
}

func writeAdmissionError(w http.ResponseWriter, err *store.AdmissionError) {
	seconds := int64((err.RetryAfter + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	message := "The request limit was reached. Please wait before trying again."
	if err.Reason == "queue_capacity" {
		message = "The pending destination limit was reached. Wait for deliveries to finish or cancel queued work before adding more."
	} else if err.Reason == "account_concurrency" {
		message = "Another request is using this destination. Please try again shortly."
	} else if err.Reason == "owner_concurrency" {
		message = "Too many requests are in progress for this owner. Please try again shortly."
	} else if err.Reason == "admission_busy" {
		message = "Another admission check is in progress. Please try again shortly."
	}
	writeError(w, http.StatusTooManyRequests, err.Reason, message)
}
