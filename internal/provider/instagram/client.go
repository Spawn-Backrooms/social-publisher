// Package instagram implements the Instagram Login publishing flow. Provider
// credentials and endpoint overrides are server configuration, never post options.
package instagram

import (
	"bytes"
	"context"
	"encoding/json"
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

const (
	defaultGraphVersion = "v26.0"
	maxResponseBytes    = 1 << 20
	minimumRefreshAge   = 24 * time.Hour
)

var publishingScopes = []string{"instagram_business_basic", "instagram_business_content_publish"}

type Client struct {
	clientID, clientSecret, redirectURI string
	graphBase, authBase, tokenBase      string
	graphVersion                        string
	http                                *http.Client
	refreshHTTP                         *http.Client
	now                                 func() time.Time
}

var _ provider.Adapter = (*Client)(nil)

// New constructs an adapter. AuthBase, when set, replaces both Instagram OAuth
// origins; this supports controlled fixtures without changing request payloads.
func New(c provider.Config) *Client {
	hc := &http.Client{Timeout: 30 * time.Second}
	if c.HTTPClient != nil {
		copy := *c.HTTPClient
		hc = &copy
		if hc.Timeout == 0 {
			hc.Timeout = 30 * time.Second
		}
	}
	// A redirect must not forward a token, secret, code, or media URL elsewhere.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	graphBase := strings.TrimRight(c.APIBase, "/")
	if graphBase == "" {
		graphBase = "https://graph.instagram.com"
	}
	authBase, tokenBase := "https://www.instagram.com", "https://api.instagram.com"
	if c.AuthBase != "" {
		authBase = strings.TrimRight(c.AuthBase, "/")
		tokenBase = authBase
	}
	version := c.GraphVersion
	if version == "" {
		version = defaultGraphVersion
	}
	return &Client{clientID: c.ClientID, clientSecret: c.ClientSecret, redirectURI: c.RedirectURI,
		graphBase: graphBase, authBase: authBase, tokenBase: tokenBase, graphVersion: version,
		http: hc, refreshHTTP: newRefreshHTTPClient(hc), now: time.Now}
}

func (*Client) Platform() model.Platform { return model.Instagram }

func (c *Client) AuthorizationURL(state, _ string) string {
	q := url.Values{
		"client_id": {c.clientID}, "redirect_uri": {c.redirectURI},
		"response_type": {"code"}, "scope": {strings.Join(publishingScopes, ",")}, "state": {state},
	}
	return c.authBase + "/oauth/authorize?" + q.Encode()
}

func (c *Client) Exchange(ctx context.Context, code, _ string) (model.Token, error) {
	if strings.TrimSpace(code) == "" {
		return model.Token{}, localError("invalid_authorization_code", "An Instagram authorization code is required.")
	}
	body, err := c.request(ctx, http.MethodPost, c.tokenBase+"/oauth/access_token", url.Values{
		"client_id": {c.clientID}, "client_secret": {c.clientSecret}, "redirect_uri": {c.redirectURI},
		"grant_type": {"authorization_code"}, "code": {code},
	}, "", requestOAuth)
	if err != nil {
		return model.Token{}, err
	}
	short, err := decodeToken(body, true)
	if err != nil {
		return model.Token{}, err
	}
	body, err = c.request(ctx, http.MethodGet, c.graphBase+"/access_token", url.Values{
		"grant_type": {"ig_exchange_token"}, "client_secret": {c.clientSecret}, "access_token": {short.AccessToken},
	}, "", requestOAuth)
	if err != nil {
		return model.Token{}, err
	}
	long, err := decodeToken(body, false)
	if err != nil {
		return model.Token{}, err
	}
	if long.UserID != "" && long.UserID != short.UserID {
		return model.Token{}, malformedResponse("Instagram returned inconsistent account identities.")
	}
	issued := c.now().UTC()
	return model.Token{AccessToken: long.AccessToken, TokenType: tokenType(long.TokenType),
		ProviderUserID: string(short.UserID), Scopes: append([]string(nil), short.Permissions...),
		IssuedAt: issued, ExpiresAt: issued.Add(time.Duration(long.ExpiresIn) * time.Second)}, nil
}

func (c *Client) Refresh(ctx context.Context, token model.Token) (model.Token, error) {
	now := c.now().UTC()
	if strings.TrimSpace(token.AccessToken) == "" || token.ExpiresAt.IsZero() || !token.ExpiresAt.After(now) {
		return model.Token{}, &provider.Error{Code: "instagram_auth_expired", Message: "Instagram authorization has expired; reconnect the account.", AuthExpired: true}
	}
	if token.IssuedAt.IsZero() || token.IssuedAt.After(now) {
		pe := localError("token_age_unknown", "The Instagram token issuance time is invalid; reconnect the account.")
		pe.RefreshNotApplied = true
		return model.Token{}, pe
	}
	if now.Sub(token.IssuedAt) < minimumRefreshAge {
		return token, nil
	}
	body, err := c.request(ctx, http.MethodGet, c.graphBase+"/refresh_access_token", url.Values{
		"grant_type": {"ig_refresh_token"}, "access_token": {token.AccessToken},
	}, "", requestRefresh)
	if err != nil {
		return model.Token{}, err
	}
	refreshed, err := decodeToken(body, false)
	if err != nil {
		return model.Token{}, refreshUncertain()
	}
	if refreshed.UserID != "" && string(refreshed.UserID) != token.ProviderUserID {
		return model.Token{}, refreshUncertain()
	}
	issued := c.now().UTC()
	token.AccessToken = refreshed.AccessToken
	token.TokenType = tokenType(refreshed.TokenType)
	token.IssuedAt = issued
	token.ExpiresAt = issued.Add(time.Duration(refreshed.ExpiresIn) * time.Second)
	// Instagram refreshes its access token directly; it has no refresh credential.
	token.RefreshToken = ""
	token.RefreshExpiresAt = time.Time{}
	return token, nil
}

func refreshUncertain() *provider.Error {
	return &provider.Error{Code: "instagram_refresh_uncertain", Message: "Instagram may have renewed authorization without returning usable credentials. Reconnect the account.", Ambiguous: true}
}

func (c *Client) Identity(ctx context.Context, token model.Token) (model.Identity, error) {
	body, err := c.request(ctx, http.MethodGet, c.graph("me"), url.Values{
		"fields": {"user_id,username,account_type,name"},
	}, token.AccessToken, requestRead)
	if err != nil {
		return model.Identity{}, err
	}
	var account struct {
		ID          graphID `json:"id"`
		UserID      graphID `json:"user_id"`
		Username    string  `json:"username"`
		Name        string  `json:"name"`
		AccountType string  `json:"account_type"`
	}
	if err := decodeJSON(body, &account); err != nil {
		return model.Identity{}, malformedResponse("Instagram returned an invalid account identity.")
	}
	id := account.UserID
	if id == "" {
		id = account.ID
	}
	if id == "" || strings.TrimSpace(account.Username) == "" {
		return model.Identity{}, malformedResponse("Instagram did not return the authorized account identity.")
	}
	accountType := strings.ToUpper(account.AccountType)
	switch accountType {
	case "", "BUSINESS", "MEDIA_CREATOR", "CREATOR":
	default:
		return model.Identity{}, localError("professional_account_required", "Instagram publishing requires a Business or Creator account.")
	}
	name := account.Name
	if name == "" {
		name = account.Username
	}
	return model.Identity{ID: string(id), Username: account.Username, DisplayName: name, AccountType: accountType}, nil
}

func (*Client) Revoke(context.Context, model.Token) error {
	return localError("revoke_unsupported", "Provider-side Instagram revocation is not implemented; remove the app's access in Instagram settings if needed. Local disconnection is handled separately.")
}

type publishingQuota struct {
	Usage           int64 `json:"usage"`
	Total           int64 `json:"total"`
	DurationSeconds int64 `json:"duration_seconds"`
}

func (c *Client) Capabilities(ctx context.Context, token model.Token) (model.Capabilities, error) {
	id, err := c.Identity(ctx, token)
	if err != nil {
		return model.Capabilities{}, err
	}
	if token.ProviderUserID != "" && token.ProviderUserID != id.ID {
		return model.Capabilities{}, localError("account_mismatch", "The Instagram token does not match the connected account.")
	}
	quota, err := c.quota(ctx, token, id.ID)
	if err != nil {
		return model.Capabilities{}, err
	}
	// Return a selected schema, never the provider's raw response body.
	raw, _ := json.Marshal(struct {
		PublishingQuota  publishingQuota `json:"publishing_quota"`
		SupportedMedia   []string        `json:"supported_media"`
		MaxCarouselItems int             `json:"max_carousel_items"`
		StoriesSupported bool            `json:"stories_supported"`
	}{quota, []string{"image", "reel", "carousel"}, 10, false})
	canPublish := true
	if len(token.Scopes) > 0 {
		canPublish = contains(token.Scopes, "instagram_business_basic") && contains(token.Scopes, "instagram_business_content_publish")
	}
	// A full rolling quota does not remove the ability to queue a future post.
	return model.Capabilities{CanPublish: canPublish, DisplayName: id.DisplayName, AccountType: id.AccountType,
		MaxVideoDurationSeconds: 900, Raw: raw}, nil
}

func (c *Client) quota(ctx context.Context, token model.Token, accountID string) (publishingQuota, error) {
	if !validID(accountID) {
		return publishingQuota{}, localError("invalid_account_id", "The connected Instagram account ID is invalid.")
	}
	body, err := c.request(ctx, http.MethodGet, c.graph(accountID+"/content_publishing_limit"), url.Values{
		"fields": {"quota_usage,config"},
	}, token.AccessToken, requestRead)
	if err != nil {
		return publishingQuota{}, err
	}
	var response struct {
		Data []struct {
			Usage  *int64 `json:"quota_usage"`
			Config struct {
				Total    *int64 `json:"quota_total"`
				Duration *int64 `json:"quota_duration"`
			} `json:"config"`
		} `json:"data"`
	}
	if err := decodeJSON(body, &response); err != nil || len(response.Data) != 1 {
		return publishingQuota{}, malformedResponse("Instagram returned an invalid publishing quota.")
	}
	data := response.Data[0]
	if data.Usage == nil || data.Config.Total == nil || data.Config.Duration == nil || *data.Usage < 0 || *data.Config.Total < 0 || *data.Config.Duration <= 0 || *data.Config.Duration > 366*86400 {
		return publishingQuota{}, malformedResponse("Instagram did not return a usable publishing quota.")
	}
	return publishingQuota{Usage: *data.Usage, Total: *data.Config.Total, DurationSeconds: *data.Config.Duration}, nil
}

func (c *Client) graph(path string) string {
	return c.graphBase + "/" + c.graphVersion + "/" + path
}

type graphID string

func (id *graphID) UnmarshalJSON(b []byte) error {
	var s string
	if len(b) > 0 && b[0] == '"' {
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
	} else {
		s = string(b)
	}
	if !validID(s) {
		return fmt.Errorf("invalid Instagram ID")
	}
	*id = graphID(s)
	return nil
}

func validID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	positive := false
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
		positive = positive || r != '0'
	}
	return positive
}

type tokenResponse struct {
	AccessToken string   `json:"access_token"`
	UserID      graphID  `json:"user_id"`
	TokenType   string   `json:"token_type"`
	ExpiresIn   int64    `json:"expires_in"`
	Permissions []string `json:"permissions"`
}

func decodeToken(body []byte, short bool) (tokenResponse, error) {
	fields, err := decodeJSONObject(body)
	if err != nil {
		return tokenResponse{}, malformedResponse("Instagram returned an invalid token response.")
	}
	if _, hasError := fields["error"]; hasError {
		return tokenResponse{}, malformedResponse("Instagram returned contradictory token and error data.")
	}
	if wrapped, ok := fields["data"]; ok {
		// Do not choose between multiple credentials or between flat and wrapped data.
		if _, exists := fields["access_token"]; exists {
			return tokenResponse{}, malformedResponse("Instagram returned an ambiguous token response.")
		}
		var entries []json.RawMessage
		if err := decodeJSON(wrapped, &entries); err != nil || len(entries) != 1 {
			return tokenResponse{}, malformedResponse("Instagram returned an ambiguous token response.")
		}
		body = entries[0]
		entry, err := decodeJSONObject(body)
		if err != nil || entry["data"] != nil || entry["error"] != nil {
			return tokenResponse{}, malformedResponse("Instagram returned an invalid token response.")
		}
	}
	var token tokenResponse
	if err := decodeJSON(body, &token); err != nil || strings.TrimSpace(token.AccessToken) == "" || len(token.AccessToken) > 32*1024 || strings.ContainsAny(token.AccessToken, "\r\n") {
		return tokenResponse{}, malformedResponse("Instagram returned an invalid token response.")
	}
	if short && token.UserID == "" {
		return tokenResponse{}, malformedResponse("Instagram did not identify the authorized account.")
	}
	if !short && (token.ExpiresIn <= 0 || token.ExpiresIn > 366*86400) {
		return tokenResponse{}, malformedResponse("Instagram did not return a usable token expiration.")
	}
	return token, nil
}

func tokenType(t string) string {
	if t == "" {
		return "bearer"
	}
	return t
}

type requestKind int

const (
	requestRead requestKind = iota
	requestCreate
	requestOAuth
	requestPublish
	requestRefresh
)

func (c *Client) request(ctx context.Context, method, endpoint string, params url.Values, token string, kind requestKind) ([]byte, error) {
	var body io.Reader
	if method == http.MethodGet {
		if len(params) > 0 {
			endpoint += "?" + params.Encode()
		}
	} else {
		body = strings.NewReader(params.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		pe := localError("instagram_configuration_error", "The Instagram endpoint configuration is invalid.")
		pe.RefreshNotApplied = kind == requestRefresh
		return nil, pe
	}
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := c.http
	if kind == requestRefresh {
		client = c.refreshHTTP
		if client == nil {
			return nil, &provider.Error{Code: "instagram_refresh_transport", Message: "The configured HTTP transport cannot safely refresh authorization.", RefreshNotApplied: true}
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, &provider.Error{Code: "instagram_transport_error", Message: "The Instagram request did not return a response.",
			Ambiguous: kind == requestPublish || kind == requestRefresh, Retryable: kind == requestRead || kind == requestCreate}
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if readErr != nil || len(data) > maxResponseBytes {
		pe := &provider.Error{Code: "instagram_response_error", Message: "The Instagram response could not be read safely.",
			Ambiguous: kind == requestPublish || kind == requestRefresh, Retryable: kind == requestRead || kind == requestCreate}
		// Throttle headers remain useful for safe requests even when the body
		// cannot be read. They do not prove that a public mutation or token
		// refresh was rejected; those outcomes retain their uncertainty.
		if resp.StatusCode == http.StatusTooManyRequests {
			pe.RateLimited = true
			pe.RetryAfter = retryAfter(resp.Header.Get("Retry-After"), c.now())
		}
		return nil, pe
	}
	var envelope struct {
		ID          json.RawMessage `json:"id"`
		ConfigIssue json.RawMessage `json:"config_issue"`
		Error       *struct {
			Code        json.Number `json:"code"`
			Subcode     json.Number `json:"error_subcode"`
			IsTransient bool        `json:"is_transient"`
		} `json:"error"`
	}
	decodeErr := decodeJSON(data, &envelope)
	structured := decodeErr == nil && envelope.Error != nil
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && !structured {
		return data, nil
	}
	code, subcode := int64(0), int64(0)
	transient := false
	if structured {
		code, _ = envelope.Error.Code.Int64()
		subcode, _ = envelope.Error.Subcode.Int64()
		transient = envelope.Error.IsTransient
	}
	// Only an explicit provider rejection or rate-limit response proves that a
	// public mutation was rejected. A 5xx, redirect, or broken body is uncertain.
	hasReceipt := len(envelope.ID) > 0 || len(envelope.ConfigIssue) > 0
	definite := !hasReceipt && (resp.StatusCode == http.StatusTooManyRequests || (structured && code != 0 && resp.StatusCode < 500 && resp.StatusCode >= 200 && (resp.StatusCode < 300 || resp.StatusCode >= 400)))
	refreshRejected := definite && resp.StatusCode != http.StatusRequestTimeout && provider.IsTokenRejectionBody(data)
	pe := &provider.Error{Code: "instagram_api_error", Message: fmt.Sprintf("Instagram rejected the request (HTTP %d).", resp.StatusCode),
		Ambiguous:         (kind == requestPublish && !definite) || (kind == requestRefresh && !refreshRejected),
		RefreshNotApplied: kind == requestRefresh && refreshRejected, RetryAfter: retryAfter(resp.Header.Get("Retry-After"), c.now())}
	if code != 0 {
		pe.Message = fmt.Sprintf("Instagram rejected the request (HTTP %d, code %d, subcode %d).", resp.StatusCode, code, subcode)
	}
	if code == 190 || resp.StatusCode == http.StatusUnauthorized {
		pe.Code = "instagram_auth_expired"
		pe.Message = "Instagram authorization has expired or was revoked; reconnect the account."
		pe.AuthExpired = true
	} else if code == 10 || code == 200 {
		pe.Code = "instagram_permission_denied"
		pe.Message = "Instagram did not grant permission for this operation; reconnect with publishing access."
	} else {
		pe.Retryable = !pe.Ambiguous && kind != requestOAuth && (resp.StatusCode == 429 || resp.StatusCode >= 500 || transient || code == 1 || code == 2 || code == 4 || code == 17 || code == 32 || code == 613 || subcode == 2207042 || subcode == 2207027)
		pe.RateLimited = !pe.Ambiguous && (resp.StatusCode == http.StatusTooManyRequests || code == 4 || code == 17 || code == 32 || code == 613 || subcode == 2207042)
		if subcode == 2207042 {
			pe.Code = "instagram_quota_exhausted"
			if pe.RetryAfter == 0 {
				pe.RetryAfter = 15 * time.Minute
			}
		}
		if subcode == 2207027 && pe.RetryAfter == 0 {
			pe.RetryAfter = time.Minute
		}
	}
	return nil, pe
}

func retryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && seconds > 0 {
		if seconds > 86400 {
			seconds = 86400
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return min(at.Sub(now), 24*time.Hour)
	}
	return 0
}

func decodeJSON(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("unexpected trailing JSON")
	}
	return nil
}

func decodeJSONObject(data []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return nil, fmt.Errorf("expected JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok || fields[key] != nil {
			return nil, fmt.Errorf("duplicate or invalid JSON field")
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if last, err := d.Token(); err != nil || last != json.Delim('}') {
		return nil, fmt.Errorf("invalid JSON object")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing JSON")
	}
	return fields, nil
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func localError(code, message string) *provider.Error {
	return &provider.Error{Code: code, Message: message}
}

func malformedResponse(message string) *provider.Error {
	return localError("instagram_malformed_response", message)
}
