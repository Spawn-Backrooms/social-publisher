package tiktok

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
)

func (c *Client) AuthorizationURL(state, _ string) string {
	u, err := url.Parse(c.authURL)
	if err != nil {
		return ""
	}
	q := u.Query()
	q.Set("client_key", c.config.ClientID)
	q.Set("response_type", "code")
	q.Set("scope", "user.info.basic,video.publish")
	q.Set("redirect_uri", c.config.RedirectURI)
	q.Set("state", state)
	q.Set("disable_auto_auth", "1")
	u.RawQuery = q.Encode()
	return u.String()
}

func (c *Client) Exchange(ctx context.Context, code, _ string) (model.Token, error) {
	if code == "" {
		return model.Token{}, localError("invalid_authorization_code", "TikTok authorization did not return a code")
	}
	form := c.clientForm()
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", c.config.RedirectURI)
	return c.tokenRequest(ctx, form, "")
}

func (c *Client) Refresh(ctx context.Context, token model.Token) (model.Token, error) {
	if token.RefreshToken == "" || (!token.RefreshExpiresAt.IsZero() && !c.now().Before(token.RefreshExpiresAt)) {
		return model.Token{}, &provider.Error{Code: "invalid_grant", Message: "Reconnect TikTok because its refresh authorization has expired", AuthExpired: true}
	}
	form := c.clientForm()
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", token.RefreshToken)
	return c.tokenRequest(ctx, form, token.ProviderUserID)
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	ExpiresIn        *int64 `json:"expires_in"`
	OpenID           string `json:"open_id"`
	RefreshToken     string `json:"refresh_token"`
	RefreshExpiresIn *int64 `json:"refresh_expires_in"`
	Scope            string `json:"scope"`
	TokenType        string `json:"token_type"`
	Error            string `json:"error"`
}

func (c *Client) tokenRequest(ctx context.Context, form url.Values, expectedUserID string) (model.Token, error) {
	if err := ctx.Err(); err != nil {
		return model.Token{}, err
	}
	res, err := c.request(ctx, http.MethodPost, "/v2/oauth/token/", "", "application/x-www-form-urlencoded", []byte(form.Encode()))
	if err != nil {
		return model.Token{}, uncertain(ctx, "oauth_outcome_uncertain", "TikTok token exchange could not be confirmed; reconnect if authorization cannot be recovered")
	}
	var wire tokenResponse
	decodeErr := json.Unmarshal(res.body, &wire)
	if res.status >= 500 || res.status == http.StatusRequestTimeout {
		return model.Token{}, uncertain(ctx, "oauth_outcome_uncertain", "TikTok token exchange could not be confirmed; reconnect if authorization cannot be recovered")
	}
	if res.status < 200 || (res.status >= 300 && res.status < 400) {
		return model.Token{}, uncertain(ctx, "oauth_outcome_uncertain", "TikTok token exchange could not be confirmed; reconnect if authorization cannot be recovered")
	}
	if decodeErr == nil && wire.Error != "" {
		if !provider.IsTokenRejectionBody(res.body) {
			return model.Token{}, uncertain(ctx, "oauth_outcome_uncertain", "TikTok returned contradictory authorization data; reconnect the account")
		}
		return model.Token{}, oauthRejection(wire.Error, res.status, res.retryAfter)
	}
	if res.status < 200 || res.status >= 300 {
		if !provider.IsTokenRejectionBody(res.body) {
			return model.Token{}, uncertain(ctx, "oauth_outcome_uncertain", "TikTok authorization rejection could not be confirmed; reconnect the account")
		}
		return model.Token{}, oauthRejection("", res.status, res.retryAfter)
	}
	// Missing values never inherit a guessed lifetime, the previous refresh
	// token, or originally requested scopes. A malformed success may already
	// have consumed the grant and therefore must not be replayed automatically.
	if decodeErr != nil || wire.AccessToken == "" || wire.OpenID == "" ||
		!strings.EqualFold(wire.TokenType, "Bearer") || !positiveDuration(wire.ExpiresIn) ||
		wire.RefreshToken == "" || !positiveDuration(wire.RefreshExpiresIn) ||
		(expectedUserID != "" && wire.OpenID != expectedUserID) {
		return model.Token{}, uncertain(ctx, "oauth_response_invalid", "TikTok returned incomplete or inconsistent authorization data")
	}
	now := c.now().UTC()
	return model.Token{
		AccessToken: wire.AccessToken, RefreshToken: wire.RefreshToken,
		TokenType: wire.TokenType, ProviderUserID: wire.OpenID,
		Scopes: splitScopes(wire.Scope), IssuedAt: now,
		ExpiresAt:        now.Add(time.Duration(*wire.ExpiresIn) * time.Second),
		RefreshExpiresAt: now.Add(time.Duration(*wire.RefreshExpiresIn) * time.Second),
	}, nil
}

func (c *Client) Revoke(ctx context.Context, token model.Token) error {
	if token.AccessToken == "" {
		return localError("access_token_invalid", "TikTok remote revocation requires an access token")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	form := c.clientForm()
	form.Set("token", token.AccessToken)
	res, err := c.request(ctx, http.MethodPost, "/v2/oauth/revoke/", "", "application/x-www-form-urlencoded", []byte(form.Encode()))
	if err != nil {
		return uncertain(ctx, "revocation_uncertain", "TikTok remote revocation could not be confirmed")
	}
	if res.status >= 500 || res.status == http.StatusRequestTimeout {
		return uncertain(ctx, "revocation_uncertain", "TikTok remote revocation could not be confirmed")
	}
	var wire struct {
		Error string `json:"error"`
	}
	var fields map[string]json.RawMessage
	if len(strings.TrimSpace(string(res.body))) != 0 && (json.Unmarshal(res.body, &fields) != nil || fields == nil || json.Unmarshal(res.body, &wire) != nil) {
		if res.status >= 400 && res.status < 500 {
			return oauthRejection("", res.status, res.retryAfter)
		}
		return uncertain(ctx, "revocation_uncertain", "TikTok remote revocation could not be confirmed")
	}
	if wire.Error != "" {
		return oauthRejection(wire.Error, res.status, res.retryAfter)
	}
	if res.status < 200 || res.status >= 300 {
		return oauthRejection("", res.status, res.retryAfter)
	}
	if len(fields) != 0 {
		return uncertain(ctx, "revocation_uncertain", "TikTok remote revocation could not be confirmed")
	}
	return nil
}

func (c *Client) Identity(ctx context.Context, token model.Token) (model.Identity, error) {
	if err := requireScope(token, "user.info.basic"); err != nil {
		return model.Identity{}, err
	}
	res, err := c.request(ctx, http.MethodGet, "/v2/user/info/?fields=open_id,display_name", token.AccessToken, "", nil)
	if err != nil {
		return model.Identity{}, err
	}
	var data struct {
		User struct {
			OpenID      string `json:"open_id"`
			DisplayName string `json:"display_name"`
		} `json:"user"`
	}
	if err := decodeAPI(res, &data); err != nil {
		return model.Identity{}, err
	}
	if data.User.OpenID == "" || (token.ProviderUserID != "" && data.User.OpenID != token.ProviderUserID) {
		return model.Identity{}, localError("identity_mismatch", "TikTok returned inconsistent account identity data")
	}
	// user.info.basic does not authorize the User Info username field. The
	// creator query separately supplies its username for the composer.
	return model.Identity{ID: data.User.OpenID, DisplayName: data.User.DisplayName}, nil
}

func (c *Client) clientForm() url.Values {
	return url.Values{"client_key": {c.config.ClientID}, "client_secret": {c.config.ClientSecret}}
}

func oauthRejection(code string, status int, after time.Duration) *provider.Error {
	result := &provider.Error{Code: "oauth_rejected", Message: "TikTok rejected authorization", RefreshNotApplied: true, RateLimited: status == http.StatusTooManyRequests}
	switch code {
	case "invalid_grant":
		result.Code, result.Message, result.AuthExpired = code, "Reconnect TikTok because its authorization grant is no longer usable", true
	case "invalid_client", "unauthorized_client":
		result.Code, result.Message = code, "TikTok application credentials or permissions are not configured correctly"
	case "invalid_scope":
		result.Code, result.Message = code, "TikTok application scopes are not configured correctly"
	case "access_denied":
		result.Code, result.Message = code, "TikTok authorization was declined"
	case "invalid_request", "unsupported_grant_type", "unsupported_response_type":
		result.Code, result.Message = code, "TikTok rejected the authorization parameters"
	case "temporarily_unavailable", "server_error":
		result.Code, result.Message, result.Retryable = code, "TikTok authorization is temporarily unavailable", true
	case "rate_limit_exceeded":
		result.Code, result.Message, result.Retryable, result.RateLimited = code, "TikTok authorization request limit reached; try again later", true, true
	default:
		if status == http.StatusTooManyRequests {
			result.Code, result.Message, result.Retryable = "rate_limit_exceeded", "TikTok authorization request limit reached; try again later", true
		}
		if status == http.StatusUnauthorized {
			result.Code, result.Message, result.AuthExpired = "invalid_grant", "Reconnect the TikTok account", true
		}
	}
	if result.Retryable {
		result.RetryAfter = after
	}
	return result
}

func positiveDuration(value *int64) bool {
	return value != nil && *value > 0 && *value <= int64((1<<63-1)/int64(time.Second))
}

func splitScopes(value string) []string {
	var scopes []string
	seen := make(map[string]bool)
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part != "" && !seen[part] {
			scopes = append(scopes, part)
			seen[part] = true
		}
	}
	return scopes
}
