// Package tiktok implements TikTok's user-authorized Content Posting API.
package tiktok

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
)

const (
	defaultAPIBase   = "https://open.tiktokapis.com"
	defaultAuthURL   = "https://www.tiktok.com/v2/auth/authorize/"
	maxResponseBytes = 1 << 20
	pollInterval     = 5 * time.Second
)

// Client contains no user credentials or mutable publishing state. The service
// owns per-account refresh coordination, media allowlists, and durable progress.
type Client struct {
	config  provider.Config
	http    *http.Client
	apiBase string
	authURL string
	now     func() time.Time
}

var _ provider.Adapter = (*Client)(nil)

func New(config provider.Config) *Client {
	apiBase := strings.TrimRight(config.APIBase, "/")
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
	authURL := config.AuthBase
	if authURL == "" {
		authURL = defaultAuthURL
	}
	httpClient := http.Client{Timeout: 30 * time.Second}
	if config.HTTPClient != nil {
		httpClient = *config.HTTPClient
		if httpClient.Timeout == 0 {
			httpClient.Timeout = 30 * time.Second
		}
	}
	// Never forward bearer tokens, client secrets, or source URLs through a
	// redirect, including when a caller supplies a custom HTTP client.
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	httpClient.Jar = nil
	return &Client{config: config, http: &httpClient, apiBase: apiBase, authURL: authURL, now: time.Now}
}

func (c *Client) Platform() model.Platform { return model.TikTok }

type response struct {
	status     int
	body       []byte
	retryAfter time.Duration
}

type apiEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Code string `json:"code"`
	} `json:"error"`
}

func (c *Client) request(ctx context.Context, method, path, accessToken, contentType string, body []byte) (response, error) {
	if err := ctx.Err(); err != nil {
		return response{}, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase+path, bytes.NewReader(body))
	if err != nil {
		return response{}, localError("invalid_configuration", "TikTok API configuration is invalid")
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return response{}, contextError(ctx, &provider.Error{Code: "request_failed", Message: "TikTok request could not be completed", Retryable: true})
	}
	defer res.Body.Close()
	result := response{status: res.StatusCode, retryAfter: retryAfter(res.Header.Get("Retry-After"), c.now())}
	if res.StatusCode == http.StatusTooManyRequests && path != "/v2/oauth/token/" {
		// The HTTP status is already a definite rejection. A broken or delayed
		// error body must not convert account throttling into delivery failure.
		// OAuth must inspect its body for contradictory replacement credentials.
		return result, apiRejection("", res.StatusCode, result.retryAfter)
	}
	result.body, err = io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil {
		return result, contextError(ctx, &provider.Error{Code: "response_unavailable", Message: "TikTok response could not be read", Retryable: true})
	}
	if len(result.body) > maxResponseBytes {
		result.body = nil
		return result, &provider.Error{Code: "invalid_response", Message: "TikTok response exceeded the permitted size", Retryable: true}
	}
	return result, nil
}

func decodeAPI(res response, dest any) error {
	if res.status >= 500 {
		return &provider.Error{Code: "provider_unavailable", Message: "TikTok is temporarily unavailable", Retryable: true, RetryAfter: res.retryAfter}
	}
	var envelope apiEnvelope
	err := json.Unmarshal(res.body, &envelope)
	if err == nil && envelope.Error != nil && envelope.Error.Code != "" && envelope.Error.Code != "ok" {
		return apiRejection(envelope.Error.Code, res.status, res.retryAfter)
	}
	if res.status < 200 || res.status >= 300 {
		return apiRejection("", res.status, res.retryAfter)
	}
	if err != nil || envelope.Error == nil || envelope.Error.Code != "ok" {
		return &provider.Error{Code: "invalid_response", Message: "TikTok returned an invalid API response", Retryable: true}
	}
	if dest != nil && (len(envelope.Data) == 0 || string(envelope.Data) == "null" || json.Unmarshal(envelope.Data, dest) != nil) {
		return &provider.Error{Code: "invalid_response", Message: "TikTok returned incomplete API data", Retryable: true}
	}
	return nil
}

// A non-success provider envelope or a final client-error response proves that
// initialization was rejected. A 408, 5xx, redirect, unreadable success, or
// transport failure does not establish that a post was not initialized.
func explicitRejection(res response) bool {
	if res.status == http.StatusRequestTimeout || res.status >= 500 || res.status < 200 || (res.status >= 300 && res.status < 400) {
		return false
	}
	if res.status >= 400 && res.status < 500 {
		return true
	}
	var envelope apiEnvelope
	return json.Unmarshal(res.body, &envelope) == nil && envelope.Error != nil && envelope.Error.Code != "" && envelope.Error.Code != "ok"
}

func apiRejection(code string, status int, after time.Duration) *provider.Error {
	result := &provider.Error{Code: "provider_rejected", Message: "TikTok rejected the request"}
	switch code {
	case "access_token_invalid":
		result.Code, result.Message, result.AuthExpired = code, "TikTok authorization has expired or is invalid", true
	case "scope_not_authorized":
		result.Code, result.Message, result.AuthExpired = code, "Reconnect TikTok with the required permissions", true
	case "rate_limit_exceeded":
		result.Code, result.Message, result.Retryable = code, "TikTok request limit reached; try again later", true
	case "spam_risk_too_many_posts":
		result.Code, result.Message, result.Retryable = code, "TikTok account publishing limit reached; try again later", true
	case "reached_active_user_cap":
		result.Code, result.Message, result.Retryable = code, "TikTok application creator limit reached; try again later", true
	case "spam_risk_user_banned_from_posting":
		result.Code, result.Message = code, "TikTok has disabled publishing for this account"
	case "unaudited_client_can_only_post_to_private_accounts":
		result.Code, result.Message = code, "Unaudited TikTok applications require a private account and only-me visibility"
	case "url_ownership_unverified":
		result.Code, result.Message = code, "The media origin must be verified in the TikTok application"
	case "privacy_level_option_mismatch":
		result.Code, result.Message = code, "TikTok privacy settings changed; ask the creator to review this post"
	case "invalid_param":
		result.Code, result.Message = code, "TikTok rejected the posting parameters"
	case "invalid_publish_id":
		result.Code, result.Message = code, "TikTok could not find the saved publishing operation"
	case "token_not_authorized_for_specified_publish_id":
		result.Code, result.Message = code, "This TikTok connection cannot access the saved publishing operation"
	case "publish_not_cancellable":
		result.Code, result.Message = code, "TikTok can no longer cancel the publishing operation"
	case "internal_error":
		result.Code, result.Message, result.Retryable = code, "TikTok is temporarily unavailable", true
	default:
		switch status {
		case http.StatusUnauthorized:
			result.Code, result.Message, result.AuthExpired = "access_token_invalid", "TikTok authorization has expired or is invalid", true
		case http.StatusTooManyRequests:
			result.Code, result.Message, result.Retryable = "rate_limit_exceeded", "TikTok request limit reached; try again later", true
		}
	}
	if result.Retryable {
		result.RetryAfter = after
	}
	if status == http.StatusTooManyRequests || result.Code == "rate_limit_exceeded" || result.Code == "spam_risk_too_many_posts" || result.Code == "reached_active_user_cap" {
		result.RateLimited = true
		result.Retryable = true
		result.RetryAfter = after
	}
	return result
}

func requireScope(token model.Token, scope string) error {
	if token.AccessToken == "" {
		return &provider.Error{Code: "access_token_invalid", Message: "Reconnect the TikTok account", AuthExpired: true}
	}
	for _, granted := range token.Scopes {
		if granted == scope {
			return nil
		}
	}
	return &provider.Error{Code: "scope_not_authorized", Message: "Reconnect TikTok with the required permissions", AuthExpired: true}
}

func localError(code, message string) *provider.Error {
	return &provider.Error{Code: code, Message: message}
}

func contextError(ctx context.Context, err error) error {
	if cause := ctx.Err(); cause != nil {
		return errors.Join(err, cause)
	}
	return err
}

func uncertain(ctx context.Context, code, message string) error {
	return contextError(ctx, &provider.Error{Code: code, Message: message, Ambiguous: true})
}

func retryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
		if seconds > 0 && seconds <= int64((1<<63-1)/int64(time.Second)) {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	if deadline, err := http.ParseTime(value); err == nil && deadline.After(now) {
		return deadline.Sub(now)
	}
	return 0
}
