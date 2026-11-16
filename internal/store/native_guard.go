package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"socialpublisher/internal/model"
)

func nativeIdentityKey(platform model.Platform, providerID string) string {
	quote := func(value string) string {
		var b strings.Builder
		b.WriteByte('"')
		for _, r := range value {
			switch r {
			case '"', '\\':
				b.WriteByte('\\')
				b.WriteRune(r)
			case '\b':
				b.WriteString(`\b`)
			case '\f':
				b.WriteString(`\f`)
			case '\n':
				b.WriteString(`\n`)
			case '\r':
				b.WriteString(`\r`)
			case '\t':
				b.WriteString(`\t`)
			default:
				if r < 0x20 {
					fmt.Fprintf(&b, `\u%04x`, r)
				} else {
					b.WriteRune(r)
				}
			}
		}
		b.WriteByte('"')
		return b.String()
	}
	// Match JSON.stringify exactly, including literal HTML and U+2028/U+2029.
	return "identity:[" + quote(string(platform)) + "," + quote(providerID) + "]"
}

func (s *Native) cleanupUncertainGuard(admission context.Context, key, token string) {
	budget := 2 * time.Second
	if deadline, ok := admission.Deadline(); ok && time.Until(deadline) < budget {
		budget = time.Until(deadline)
	}
	if budget <= 0 {
		s.quarantine(errNativeQuarantined)
		return
	}
	cleanup, stop := context.WithTimeout(context.Background(), budget)
	defer stop()
	if err := s.call(cleanup, "ReleaseIdentityGuard", nil, key, token); err != nil {
		// A lost acquisition and lost cleanup can leave an orphan lock. Stop
		// protected contexts and readiness until the controller proves a stop
		// and starts a fresh session; never keep a healthy ambiguous writer.
		s.quarantine(errNativeQuarantined)
	}
}

func (s *Native) AcquireAccount(ctx context.Context, id string) (context.Context, func(), error) {
	a, err := s.GetWorkerAccount(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return s.AcquireIdentity(ctx, a.Platform, a.Identity.ID)
}
func (s *Native) TryAcquireAccount(ctx context.Context, id string) (context.Context, func(), error) {
	admission, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	a, err := s.GetWorkerAccount(admission, id)
	if err == nil {
		var guard context.Context
		var release func()
		guard, release, err = s.acquireIdentity(admission, ctx, a.Platform, a.Identity.ID, true)
		if err == nil {
			return guard, release, nil
		}
	}
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	if admission.Err() != nil {
		return nil, nil, ErrAccountBusy
	}
	return nil, nil, err
}
func (s *Native) AcquireIdentity(ctx context.Context, platform model.Platform, providerID string) (context.Context, func(), error) {
	return s.acquireIdentity(ctx, ctx, platform, providerID, false)
}

func (s *Native) acquireIdentity(admission, parent context.Context, platform model.Platform, providerID string, try bool) (context.Context, func(), error) {
	if !validPlatform(platform) || providerID == "" || !utf8.ValidString(providerID) {
		return nil, nil, ErrConflict
	}
	key := nativeIdentityKey(platform, providerID)
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, nil, errors.New("native account guard identity unavailable")
	}
	token := hex.EncodeToString(tokenBytes)
	var fence nativeFence
	for {
		err := s.call(admission, "AcquireIdentityGuard", &fence, platform, providerID, token)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrAccountBusy) || try {
			var uncertain *nativeUncertainError
			if errors.As(err, &uncertain) {
				s.cleanupUncertainGuard(admission, key, token)
			}
			return nil, nil, err
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-admission.Done():
			timer.Stop()
			return nil, nil, admission.Err()
		case <-timer.C:
		}
	}
	if fence.Key != key || fence.Token != token {
		s.cleanupUncertainGuard(admission, key, token)
		return nil, nil, errors.New("native account guard acknowledgement invalid")
	}
	previous, _ := parent.Value(nativeFenceContextKey{}).([]nativeFence)
	fences := append(append([]nativeFence{}, previous...), fence)
	guard, cancel := context.WithCancelCause(context.WithValue(parent, nativeFenceContextKey{}, fences))
	stopQuarantine := context.AfterFunc(s.guardContext, func() { cancel(errNativeQuarantined) })
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-guard.Done():
				return
			case <-ticker.C:
				probe, stop := context.WithTimeout(guard, 2*time.Second)
				err := s.call(probe, "ProbeIdentityGuard", nil, fence.Key, fence.Token)
				stop()
				if err != nil {
					cancel(errAccountLockLost)
					return
				}
			}
		}
	}()
	var once sync.Once
	return guard, func() {
		once.Do(func() {
			cancel(context.Canceled)
			stopQuarantine()
			<-stopped
			// Never delete on local expiry or assume a lost release succeeded.
			// Failed exact releases retain the durable guard until a confirmed stop.
			release, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if err := s.call(release, "ReleaseIdentityGuard", nil, fence.Key, fence.Token); err != nil {
				s.quarantine(errNativeQuarantined)
			}
		})
	}, nil
}
