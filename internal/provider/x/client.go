// Package x implements the X API v2 user-context publishing adapter.
package x

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
)

const maxResponseBytes = 1 << 20

var requiredScopes = []string{"tweet.read", "tweet.write", "users.read", "offline.access", "media.write"}

type Client struct {
	config  provider.Config
	apiBase string
	authURL string
	http    *http.Client
	now     func() time.Time
}

var _ provider.Adapter = (*Client)(nil)

// New accepts a complete authorization URL in AuthBase. APIBase is an API
// origin/base path, which can point to a local HTTP fixture in tests.
func New(c provider.Config) *Client {
	apiBase := strings.TrimRight(c.APIBase, "/")
	if apiBase == "" {
		apiBase = "https://api.x.com"
	}
	authURL := c.AuthBase
	if authURL == "" {
		authURL = "https://x.com/i/oauth2/authorize"
	}
	hc := http.Client{Timeout: 45 * time.Second}
	if c.HTTPClient != nil {
		hc = *c.HTTPClient
		if hc.Timeout <= 0 {
			hc.Timeout = 45 * time.Second
		}
	}
	// A caller-supplied client must not permit credentials to follow redirects.
	hc.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{config: c, apiBase: apiBase, authURL: authURL, http: &hc, now: time.Now}
}

func (c *Client) Platform() model.Platform { return model.X }

func (c *Client) AuthorizationURL(state, challenge string) string {
	u, err := url.Parse(c.authURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", c.config.ClientID)
	q.Set("redirect_uri", c.config.RedirectURI)
	q.Set("scope", strings.Join(requiredScopes, " "))
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String()
}

type tokenResponse struct {
	AccessToken  string          `json:"access_token"`
	RefreshToken json.RawMessage `json:"refresh_token"`
	TokenType    string          `json:"token_type"`
	ExpiresIn    int64           `json:"expires_in"`
	Scope        *string         `json:"scope"`
	Error        json.RawMessage `json:"error"`
}

func (c *Client) Exchange(ctx context.Context, code, verifier string) (model.Token, error) {
	if err := ctx.Err(); err != nil {
		return model.Token{}, err
	}
	if err := c.credentials(); err != nil {
		return model.Token{}, err
	}
	if code == "" || verifier == "" || c.config.RedirectURI == "" {
		return model.Token{}, invalid("x_oauth_input", "The authorization code, PKCE verifier, and redirect URI are required.")
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {c.config.RedirectURI}, "code_verifier": {verifier}}
	r, err := c.request(ctx, http.MethodPost, "/2/oauth2/token", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), "", true)
	if err := c.checkTokenResponse(r, err, "exchange"); err != nil {
		return model.Token{}, err
	}
	token, err := c.decodeToken(r.body, model.Token{Scopes: append([]string(nil), requiredScopes...)})
	if err != nil {
		return model.Token{}, tokenUncertain("exchange")
	}
	if token.RefreshToken == "" {
		return model.Token{}, invalid("x_refresh_token_missing", "X did not grant a refresh token. Reconnect with offline.access to enable scheduled publication.")
	}
	if !hasScopes(token.Scopes, requiredScopes...) {
		return model.Token{}, invalid("x_missing_scope", "X did not grant all requested permissions. Reconnect with the publishing, media, and offline scopes.")
	}
	return token, nil
}

func (c *Client) Refresh(ctx context.Context, old model.Token) (model.Token, error) {
	if err := ctx.Err(); err != nil {
		return model.Token{}, err
	}
	if err := c.credentials(); err != nil {
		var pe *provider.Error
		if errors.As(err, &pe) {
			pe.RefreshNotApplied = true
		}
		return model.Token{}, err
	}
	if old.RefreshToken == "" {
		return model.Token{}, &provider.Error{Code: "x_refresh_token_missing", Message: "Reconnect the X account to renew its authorization.", AuthExpired: true}
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {old.RefreshToken}}
	r, err := c.request(ctx, http.MethodPost, "/2/oauth2/token", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), "", true)
	if err := c.checkTokenResponse(r, err, "refresh"); err != nil {
		return model.Token{}, err
	}
	token, err := c.decodeToken(r.body, old)
	if err != nil {
		return model.Token{}, tokenUncertain("refresh")
	}
	return token, nil
}

func (c *Client) checkTokenResponse(r response, transportErr error, grant string) error {
	if r.status >= 400 && r.status < 500 && r.status != http.StatusRequestTimeout {
		if transportErr != nil || !provider.IsTokenRejectionBody(r.body) {
			return tokenUncertain(grant)
		}
		pe := c.responseError(r, true)
		pe.RefreshNotApplied = grant == "refresh"
		return pe
	}
	if transportErr != nil || r.status < 200 || r.status >= 300 {
		var local *provider.Error
		if errors.As(transportErr, &local) && local.Code == "x_invalid_configuration" {
			local.RefreshNotApplied = grant == "refresh"
			return transportErr
		}
		return tokenUncertain(grant)
	}
	return nil
}

func tokenUncertain(grant string) *provider.Error {
	return &provider.Error{Code: "x_" + grant + "_uncertain", Message: "X may have consumed the authorization grant without returning usable credentials. Reconnect the account before trying again.", Ambiguous: true}
}

func (c *Client) decodeToken(body []byte, old model.Token) (model.Token, error) {
	var wire tokenResponse
	if err := json.Unmarshal(body, &wire); err != nil || wire.AccessToken == "" || wire.ExpiresIn <= 0 || wire.ExpiresIn > 365*24*60*60 || wire.Error != nil {
		return model.Token{}, invalid("x_invalid_token_response", "X returned an incomplete token response; reconnect the account.")
	}
	if wire.TokenType != "" && !strings.EqualFold(wire.TokenType, "bearer") {
		return model.Token{}, invalid("x_invalid_token_type", "X returned an unsupported token type.")
	}
	issued := c.now().UTC()
	token := old
	token.AccessToken = wire.AccessToken
	token.TokenType = "bearer"
	token.IssuedAt = issued
	token.ExpiresAt = issued.Add(time.Duration(wire.ExpiresIn) * time.Second)
	if wire.RefreshToken != nil {
		var replacement string
		if err := json.Unmarshal(wire.RefreshToken, &replacement); err != nil || strings.TrimSpace(replacement) == "" {
			return model.Token{}, invalid("x_invalid_refresh_token", "X returned an unusable replacement refresh credential.")
		}
		token.RefreshToken = replacement
	}
	if wire.Scope != nil {
		token.Scopes = strings.Fields(*wire.Scope)
		if len(token.Scopes) == 0 {
			return model.Token{}, invalid("x_missing_scope", "X returned an empty authorization scope. Reconnect the account.")
		}
	} else {
		token.Scopes = append([]string(nil), old.Scopes...)
	}
	return token, nil
}

func (c *Client) Revoke(ctx context.Context, token model.Token) error {
	if token.AccessToken == "" && token.RefreshToken == "" {
		return nil
	}
	if err := c.credentials(); err != nil {
		return err
	}
	// Revoke both credentials; do not assume revoking an access token also
	// invalidates the offline grant, or that revocation cascades in reverse.
	tokens := []string{token.RefreshToken, token.AccessToken}
	var failures []error
	for i, value := range tokens {
		if value == "" || (i == 1 && value == tokens[0]) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		form := url.Values{"token": {value}}
		r, err := c.request(ctx, http.MethodPost, "/2/oauth2/revoke", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), "", true)
		if err := c.checkResponse(r, err, true); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

type userResponse struct {
	Data struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Username     string `json:"username"`
		Verified     bool   `json:"verified"`
		VerifiedType string `json:"verified_type"`
	} `json:"data"`
}

func (c *Client) user(ctx context.Context, token model.Token) (userResponse, error) {
	var user userResponse
	if token.AccessToken == "" {
		return user, &provider.Error{Code: "x_auth_expired", Message: "Connect the X account before publishing.", AuthExpired: true}
	}
	r, err := c.request(ctx, http.MethodGet, "/2/users/me?user.fields=verified%2Cverified_type", "", nil, token.AccessToken, false)
	if err := c.checkResponse(r, err, false); err != nil {
		return user, err
	}
	if err := json.Unmarshal(r.body, &user); err != nil || !validID(user.Data.ID) {
		return user, invalid("x_invalid_identity_response", "X did not return the connected account's identity.")
	}
	if token.ProviderUserID != "" && token.ProviderUserID != user.Data.ID {
		return user, invalid("x_identity_mismatch", "The X token belongs to a different connected account.")
	}
	return user, nil
}

func (c *Client) Identity(ctx context.Context, token model.Token) (model.Identity, error) {
	u, err := c.user(ctx, token)
	if err != nil {
		return model.Identity{}, err
	}
	return model.Identity{ID: u.Data.ID, Username: u.Data.Username, DisplayName: u.Data.Name, AccountType: accountType(u)}, nil
}

func accountType(u userResponse) string {
	if u.Data.Verified || u.Data.VerifiedType == "blue" || u.Data.VerifiedType == "business" || u.Data.VerifiedType == "government" {
		return "premium_or_verified"
	}
	return "standard_or_unknown"
}

func (c *Client) Capabilities(ctx context.Context, token model.Token) (model.Capabilities, error) {
	u, err := c.user(ctx, token)
	if err != nil {
		return model.Capabilities{}, err
	}
	account := accountType(u)
	maxSeconds := 1200
	maxBytes := int64(8 << 30)
	if account == "premium_or_verified" {
		maxSeconds = 7500
		maxBytes = 16 << 30
	}
	canPublish := len(token.Scopes) == 0 || hasScopes(token.Scopes, "tweet.read", "tweet.write", "users.read")
	raw, _ := json.Marshal(struct {
		MaxImageBytes       int64  `json:"max_image_bytes"`
		MaxGIFBytes         int64  `json:"max_gif_bytes"`
		MaxVideoBytes       int64  `json:"max_video_bytes"`
		MediaPermission     bool   `json:"media_permission"`
		ScopesKnown         bool   `json:"scopes_known"`
		ServerChecksPublish bool   `json:"server_checks_publish_entitlement"`
		TextValidation      string `json:"text_validation"`
	}{maxImageBytes, maxGIFBytes, maxBytes, len(token.Scopes) == 0 || hasScopes(token.Scopes, "media.write"), len(token.Scopes) > 0, true, "conservative Unicode/URL estimate; X is authoritative"})
	return model.Capabilities{CanPublish: canPublish, DisplayName: u.Data.Name, AccountType: account, MaxVideoDurationSeconds: maxSeconds, Raw: raw}, nil
}

func hasScopes(scopes []string, required ...string) bool {
	for _, want := range required {
		found := false
		for _, got := range scopes {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (c *Client) credentials() error {
	if c.config.ClientID == "" || c.config.ClientSecret == "" {
		return invalid("x_not_configured", "The X application's OAuth client ID and secret are required.")
	}
	return nil
}

type response struct {
	status int
	header http.Header
	body   []byte
}

// request is used only for X API calls. Source assets always go through
// Config.Media.Open, whose interface cannot receive the X bearer token.
func (c *Client) request(ctx context.Context, method, path, contentType string, body io.Reader, accessToken string, basic bool) (response, error) {
	var result response
	if err := ctx.Err(); err != nil {
		return result, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase+path, body)
	if err != nil {
		return result, invalid("x_invalid_configuration", "The configured X API endpoint is invalid.")
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if basic {
		req.SetBasicAuth(c.config.ClientID, c.config.ClientSecret)
	} else if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, &provider.Error{Code: "x_transport_error", Message: "The X API request ended without a response.", Retryable: true}
	}
	defer resp.Body.Close()
	result.status = resp.StatusCode
	result.header = resp.Header.Clone()
	result.body, err = io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if len(result.body) > maxResponseBytes {
		result.body = result.body[:maxResponseBytes]
		return result, &provider.Error{Code: "x_response_too_large", Message: "The X API response exceeded the allowed size.", Retryable: true}
	}
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, &provider.Error{Code: "x_response_interrupted", Message: "The X API response could not be read completely.", Retryable: true}
	}
	return result, nil
}

func (c *Client) checkResponse(r response, transportErr error, oauth bool) error {
	if r.status != 0 && (r.status < 200 || r.status >= 300) {
		return c.responseError(r, oauth)
	}
	return transportErr
}

func (c *Client) responseError(r response, oauth bool) *provider.Error {
	e := &provider.Error{Code: "x_http_" + strconv.Itoa(r.status), Message: fmt.Sprintf("X rejected the request (HTTP %d). Check account permissions, content, and media entitlements.", r.status)}
	if oauth {
		var wire struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(r.body, &wire)
		switch wire.Error {
		case "invalid_grant":
			e.Code, e.Message, e.AuthExpired = "x_invalid_grant", "X authorization expired or was revoked. Reconnect the account.", true
		case "invalid_client":
			e.Code, e.Message = "x_invalid_client", "X rejected the configured OAuth client credentials."
		case "invalid_scope":
			e.Code, e.Message = "x_invalid_scope", "X rejected the requested OAuth permissions."
		case "access_denied":
			e.Code, e.Message = "x_access_denied", "X authorization was denied."
		}
	}
	if r.status == http.StatusUnauthorized && e.Code != "x_invalid_client" {
		e.Code, e.Message, e.AuthExpired = "x_auth_expired", "X rejected this authorization. Refresh or reconnect the account.", true
	}
	if r.status == http.StatusTooManyRequests {
		e.Code, e.Message, e.Retryable = "x_rate_limited", "X's request limit was reached. Publication can resume after the reset.", true
		e.RateLimited = true
		e.RetryAfter = retryDelay(r.header, c.now())
		if e.RetryAfter <= 0 {
			e.RetryAfter = time.Minute
		}
	}
	if r.status >= 500 || r.status == http.StatusRequestTimeout {
		e.Retryable = true
		e.Message = fmt.Sprintf("X temporarily could not complete the request (HTTP %d).", r.status)
	}
	// Do not return provider body text: it can contain tokens, authorization
	// codes, signed source URLs, or submitted content that the caller logs.
	return e
}

func retryDelay(header http.Header, now time.Time) time.Duration {
	var delay time.Duration
	if raw := header.Get("Retry-After"); raw != "" {
		if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds >= 0 && seconds <= 365*24*60*60 {
			delay = time.Duration(seconds) * time.Second
		} else if at, err := http.ParseTime(raw); err == nil && at.After(now) {
			delay = at.Sub(now)
		}
	}
	if reset, err := strconv.ParseInt(header.Get("x-rate-limit-reset"), 10, 64); err == nil {
		if until := time.Unix(reset, 0).Sub(now); until > delay {
			delay = until
		}
	}
	return delay
}

func invalid(code, message string) *provider.Error {
	return &provider.Error{Code: code, Message: message}
}

func validID(id string) bool {
	if id == "" || len(id) > 19 {
		return false
	}
	for _, char := range id {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}
