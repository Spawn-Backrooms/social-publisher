package store

import (
	"errors"
	"time"
)

// AdmissionLimits apply to one verified tenant/subject pair. Zero fields use
// the service defaults; negative values are invalid, never unlimited. Limits
// are maxima: a contended admission check can refuse earlier without waiting.
type AdmissionLimits struct {
	RequestsPerMinute  int
	OwnerConcurrency   int
	AccountConcurrency int
}

func (l AdmissionLimits) normalized() (AdmissionLimits, error) {
	if l.RequestsPerMinute < 0 || l.OwnerConcurrency < 0 || l.AccountConcurrency < 0 {
		return l, ErrConflict
	}
	if l.RequestsPerMinute == 0 {
		l.RequestsPerMinute = 60
	}
	if l.OwnerConcurrency == 0 {
		l.OwnerConcurrency = 4
	}
	if l.AccountConcurrency == 0 {
		l.AccountConcurrency = 1
	}
	return l, nil
}

// AdmissionLease is issued using database time. HTTP requests are capped at
// 90 seconds, so an unrenewed 95-second lease cannot expire while an admitted
// request still has a live context. Process death releases capacity by expiry.
type AdmissionLease struct {
	Token     string
	ExpiresAt time.Time
}

const admissionLeaseDuration = 95 * time.Second

type AdmissionError struct {
	Reason     string
	RetryAfter time.Duration
}

func (e *AdmissionError) Error() string { return "admission denied: " + e.Reason }

// ErrAdmissionUnavailable identifies a failed admission dependency. The API
// returns a controlled 503 and never invokes the protected handler.
var ErrAdmissionUnavailable = errors.New("admission store unavailable")
