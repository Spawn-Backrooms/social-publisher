package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"socialpublisher/internal/model"
)

const NativeStorageURL = "http://publisher-storage.internal/v1"
const nativeBodyLimit = 2 << 20

// Native is a private RPC adapter to the singleton Cloudflare SQLite Durable
// Object. It never connects to a public storage URL and never retries a mutation
// after an uncertain acknowledgement. The controller installs interception and
// fences the prior stopped process before it creates the session capability.
type Native struct {
	client       *http.Client
	session      string
	guardContext context.Context
	quarantine   context.CancelCauseFunc
}

var _ Store = (*Native)(nil)

func ValidateNativeStorage(url, session string) error {
	if url != NativeStorageURL {
		return errors.New("NATIVE_STORAGE_URL must be the exact private storage bridge URL")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{32,200}$`).MatchString(session) {
		return errors.New("NATIVE_STORAGE_SESSION must be a controller-issued session capability")
	}
	return nil
}

func NewNative(url, session string) (*Native, error) {
	if err := ValidateNativeStorage(url, session); err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil // Private routing must never use an environment proxy.
	guardContext, quarantine := context.WithCancelCause(context.Background())
	return &Native{session: session, guardContext: guardContext, quarantine: quarantine, client: &http.Client{Transport: transport, Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("native storage redirects are disabled") }}}, nil
}

func (s *Native) Close()                         { s.quarantine(context.Canceled); s.client.CloseIdleConnections() }
func (s *Native) Ping(ctx context.Context) error { return s.call(ctx, "Ping", nil) }

type nativeFence struct {
	Key   string `json:"key"`
	Token string `json:"token"`
}
type nativeFenceContextKey struct{}

var errNativeQuarantined = errors.New("native storage session quarantined; confirmed container restart required")

type nativeUncertainError struct{ cause error }

func (*nativeUncertainError) Error() string {
	return "native storage acknowledgement unavailable; operation outcome may be uncertain"
}
func (e *nativeUncertainError) Unwrap() error { return e.cause }

type nativeEnvelope struct {
	Version    int           `json:"v"`
	RequestID  string        `json:"request_id"`
	IssuedAtMS int64         `json:"issued_at_ms"`
	Session    string        `json:"session"`
	Operation  string        `json:"op"`
	Args       []any         `json:"args"`
	Fences     []nativeFence `json:"fences"`
}

func (s *Native) call(ctx context.Context, op string, out any, args ...any) error {
	if s.guardContext.Err() != nil {
		return errNativeQuarantined
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return errors.New("native storage request identity unavailable")
	}
	fences, _ := ctx.Value(nativeFenceContextKey{}).([]nativeFence)
	fences = append([]nativeFence{}, fences...)
	if args == nil {
		args = []any{}
	}
	envelope := nativeEnvelope{1, hex.EncodeToString(id), time.Now().UnixMilli(), s.session, op, args, fences}
	body, err := json.Marshal(envelope)
	if err != nil || len(body) > nativeBodyLimit {
		return errors.New("native storage request cannot be encoded within its limit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, NativeStorageURL, bytes.NewReader(body))
	if err != nil {
		return errors.New("native storage request unavailable")
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(req)
	if err != nil {
		return &nativeUncertainError{cause: ctx.Err()}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return &nativeUncertainError{}
	}
	reply, err := io.ReadAll(io.LimitReader(response.Body, nativeBodyLimit+1))
	if err != nil || len(reply) > nativeBodyLimit {
		return &nativeUncertainError{}
	}
	var value struct {
		OK           *bool           `json:"ok"`
		Result       json.RawMessage `json:"result"`
		Error        string          `json:"error"`
		Reason       string          `json:"reason"`
		RetryAfterNS time.Duration   `json:"retry_after_ns"`
	}
	if json.Unmarshal(reply, &value) != nil || value.OK == nil {
		return &nativeUncertainError{}
	}
	if !*value.OK {
		return nativeError(value.Error, value.Reason, value.RetryAfterNS)
	}
	if out != nil && (len(value.Result) == 0 || bytes.Equal(bytes.TrimSpace(value.Result), []byte("null")) || json.Unmarshal(value.Result, out) != nil) {
		return &nativeUncertainError{}
	}
	return nil
}

func nativeError(code, reason string, retry time.Duration) error {
	switch code {
	case "not_found":
		return ErrNotFound
	case "conflict":
		return ErrConflict
	case "idempotency":
		return ErrIdempotency
	case "lease_lost":
		return ErrLeaseLost
	case "oauth_state":
		return ErrOAuthState
	case "no_work":
		return ErrNoWork
	case "account_busy":
		return ErrAccountBusy
	case "admission_unavailable":
		return ErrAdmissionUnavailable
	case "admission":
		// Only known reasons are safe to expose through the public API.
		if reason != "owner_concurrency" && reason != "account_concurrency" && reason != "request_rate" && reason != "queue_capacity" && reason != "admission_busy" {
			reason = "capacity"
		}
		if retry < 0 {
			retry = 0
		}
		return &AdmissionError{Reason: reason, RetryAfter: retry}
	default:
		return errors.New("native storage operation failed")
	}
}

func (s *Native) PutOAuthState(ctx context.Context, value model.OAuthState) error {
	return s.call(ctx, "PutOAuthState", nil, oauthToNative(value))
}
func (s *Native) ConsumeOAuthState(ctx context.Context, hash, cookie string, platform model.Platform) (model.OAuthState, error) {
	var out nativeOAuthState
	err := s.call(ctx, "ConsumeOAuthState", &out, hash, cookie, platform)
	return out.model(), err
}
func (s *Native) UpsertAccount(ctx context.Context, value model.Account) (model.Account, error) {
	var out nativeAccount
	err := s.call(ctx, "UpsertAccount", &out, accountToNative(value))
	return out.model(), err
}
func (s *Native) GetAccount(ctx context.Context, owner model.Principal, id string) (model.Account, error) {
	var out nativeAccount
	err := s.call(ctx, "GetAccount", &out, owner, id)
	return out.model(), err
}
func (s *Native) GetWorkerAccount(ctx context.Context, id string) (model.Account, error) {
	var out nativeAccount
	err := s.call(ctx, "GetWorkerAccount", &out, id)
	return out.model(), err
}
func (s *Native) accounts(ctx context.Context, op string, args ...any) ([]model.Account, error) {
	var out []nativeAccount
	if err := s.call(ctx, op, &out, args...); err != nil {
		return nil, err
	}
	values := make([]model.Account, len(out))
	for i, a := range out {
		values[i] = a.model()
	}
	return values, nil
}
func (s *Native) ListAccounts(ctx context.Context, owner model.Principal) ([]model.Account, error) {
	return s.accounts(ctx, "ListAccounts", owner)
}
func (s *Native) ListRefreshCandidates(ctx context.Context, platform model.Platform, before time.Time, limit int) ([]model.Account, error) {
	return s.accounts(ctx, "ListRefreshCandidates", platform, before, limit)
}
func (s *Native) SaveToken(ctx context.Context, id string, version int64, blob []byte, expires time.Time) error {
	return s.call(ctx, "SaveToken", nil, id, version, blob, expires)
}
func (s *Native) BeginTokenRefresh(ctx context.Context, id string, version int64) error {
	return s.call(ctx, "BeginTokenRefresh", nil, id, version)
}
func (s *Native) ClearTokenRefresh(ctx context.Context, id string, version int64) error {
	return s.call(ctx, "ClearTokenRefresh", nil, id, version)
}
func (s *Native) MarkReauth(ctx context.Context, id string, version int64) error {
	return s.call(ctx, "MarkReauth", nil, id, version)
}
func (s *Native) Disconnect(ctx context.Context, owner model.Principal, id string) (model.Account, error) {
	var out nativeAccount
	err := s.call(ctx, "Disconnect", &out, owner, id)
	return out.model(), err
}
func (s *Native) ReserveProviderRequest(ctx context.Context, id, scope string, interval time.Duration) (time.Duration, error) {
	var out time.Duration
	err := s.call(ctx, "ReserveProviderRequest", &out, id, scope, interval)
	return out, err
}
func (s *Native) SetProviderCooldown(ctx context.Context, id, scope string, delay time.Duration) (time.Duration, error) {
	var out time.Duration
	err := s.call(ctx, "SetProviderCooldown", &out, id, scope, delay)
	return out, err
}
func (s *Native) AdmitRequest(ctx context.Context, owner model.Principal, limits AdmissionLimits) (AdmissionLease, error) {
	var out AdmissionLease
	err := s.call(ctx, "AdmitRequest", &out, owner, limits)
	return out, err
}
func (s *Native) AdmitProviderOperation(ctx context.Context, owner model.Principal, platform model.Platform, id string, limits AdmissionLimits) (AdmissionLease, error) {
	var out AdmissionLease
	err := s.call(ctx, "AdmitProviderOperation", &out, owner, platform, id, limits)
	return out, err
}
func (s *Native) ReleaseAdmission(ctx context.Context, owner model.Principal, token string) error {
	return s.call(ctx, "ReleaseAdmission", nil, owner, token)
}
func (s *Native) CreatePost(ctx context.Context, value model.Post, key, hash string) (model.Post, bool, error) {
	var out struct {
		Post    nativePost `json:"post"`
		Created bool       `json:"created"`
	}
	err := s.call(ctx, "CreatePost", &out, postToNative(value), key, hash)
	return out.Post.model(), out.Created, err
}
func (s *Native) post(ctx context.Context, op string, args ...any) (model.Post, error) {
	var out nativePost
	err := s.call(ctx, op, &out, args...)
	return out.model(), err
}
func (s *Native) LookupIdempotency(ctx context.Context, owner model.Principal, key, hash string) (model.Post, error) {
	return s.post(ctx, "LookupIdempotency", owner, key, hash)
}
func (s *Native) GetPost(ctx context.Context, owner model.Principal, id string) (model.Post, error) {
	return s.post(ctx, "GetPost", owner, id)
}
func (s *Native) ListPosts(ctx context.Context, owner model.Principal, limit int) ([]model.Post, error) {
	var out []nativePost
	if err := s.call(ctx, "ListPosts", &out, owner, limit); err != nil {
		return nil, err
	}
	values := make([]model.Post, len(out))
	for i, p := range out {
		values[i] = p.model()
	}
	return values, nil
}
func (s *Native) CancelPost(ctx context.Context, owner model.Principal, id string) (model.Post, error) {
	return s.post(ctx, "CancelPost", owner, id)
}
func (s *Native) ReschedulePost(ctx context.Context, owner model.Principal, id string, version int64, at time.Time, timezone string) (model.Post, error) {
	return s.post(ctx, "ReschedulePost", owner, id, version, at, timezone)
}
func (s *Native) RetryTarget(ctx context.Context, owner model.Principal, postID, targetID string) (model.Post, error) {
	return s.post(ctx, "RetryTarget", owner, postID, targetID)
}
func (s *Native) ResolveTarget(ctx context.Context, owner model.Principal, postID, targetID string, resolution Resolution) (model.Post, error) {
	return s.post(ctx, "ResolveTarget", owner, postID, targetID, resolution)
}
func (s *Native) ListEvents(ctx context.Context, owner model.Principal, postID string) ([]model.Event, error) {
	var out []model.Event
	err := s.call(ctx, "ListEvents", &out, owner, postID)
	return out, err
}
func (s *Native) ListEventsPage(ctx context.Context, owner model.Principal, postID string, after int64, limit int) (EventPage, error) {
	var out EventPage
	err := s.call(ctx, "ListEventsPage", &out, owner, postID, after, limit)
	return out, err
}
func (s *Native) Claim(ctx context.Context, lease time.Duration) (model.Target, error) {
	var out nativeTarget
	err := s.call(ctx, "Claim", &out, lease)
	return out.model(), err
}
func (s *Native) Heartbeat(ctx context.Context, id, lease string, duration time.Duration) error {
	return s.call(ctx, "Heartbeat", nil, id, lease, duration)
}
func (s *Native) SaveProgress(ctx context.Context, id, lease string, progress model.Progress) error {
	return s.call(ctx, "SaveProgress", nil, id, lease, progress)
}
func (s *Native) FinishTarget(ctx context.Context, id, lease string, finish Finish) error {
	return s.call(ctx, "FinishTarget", nil, id, lease, finish)
}
func (s *Native) RecordProcessHeartbeat(ctx context.Context, heartbeat ProcessHeartbeat) error {
	return s.call(ctx, "RecordProcessHeartbeat", nil, heartbeat)
}
func (s *Native) OperationsSnapshot(ctx context.Context, query OperationsQuery) (OperationsSnapshot, error) {
	var out OperationsSnapshot
	err := s.call(ctx, "OperationsSnapshot", &out, query)
	return out, err
}
func (s *Native) RetainOperationalRecords(ctx context.Context, policy RetentionPolicy) (RetentionResult, error) {
	var out RetentionResult
	// Long nanosecond durations exceed JavaScript's safe integer range. Decimal
	// text lets the private bridge convert them losslessly to milliseconds.
	wire := struct {
		EventRetention string
		BatchSize      int
	}{strconv.FormatInt(int64(policy.EventRetention), 10), policy.BatchSize}
	err := s.call(ctx, "RetainOperationalRecords", &out, wire)
	return out, err
}
