package instagram

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
)

func TestRefreshFailureProvesNonRotationOnlyForRejection(t *testing.T) {
	for _, tc := range []struct {
		name             string
		status           int
		body             string
		proof, ambiguous bool
	}{
		{"revoked", 400, `{"error":{"code":190}}`, true, false},
		{"rate_limit", 429, `{}`, true, false},
		{"structured_rejection", 400, `{"error":{"code":4}}`, true, false},
		{"server_error", 503, `{"error":{"code":2}}`, false, true},
		{"timeout", 408, `{"error":{"code":2}}`, false, true},
		{"redirect", 302, `{}`, false, true},
		{"unstructured_error", 400, `{}`, false, true},
		{"malformed_success", 200, `{"access_token":"partial"}`, false, true},
		{"identity_mismatch", 200, `{"access_token":"new","expires_in":3600,"user_id":"43"}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			_, err := c.Refresh(t.Context(), model.Token{AccessToken: "fixture", ProviderUserID: "42", IssuedAt: c.now().Add(-48 * time.Hour), ExpiresAt: c.now().Add(time.Hour)})
			var pe *provider.Error
			if !errors.As(err, &pe) {
				t.Fatalf("refresh failure was not classified: %v", err)
			}
			if pe.RefreshNotApplied != tc.proof || pe.Ambiguous != tc.ambiguous {
				t.Fatalf("proof=%v ambiguous=%v; want proof=%v ambiguous=%v", pe.RefreshNotApplied, pe.Ambiguous, tc.proof, tc.ambiguous)
			}
		})
	}
}

func TestRefreshContradictoryTokenErrorBodyCannotProveNonRotation(t *testing.T) {
	for _, status := range []int{200, 400, 429} {
		for _, fields := range []string{`"access_token":"rotated-access","expires_in":3600`, `"data":[{"access_token":"rotated-access","expires_in":3600}]`} {
			c, _ := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{` + fields + `,"error":{"code":4}}`))
			})
			_, err := c.Refresh(t.Context(), model.Token{AccessToken: "fixture", ProviderUserID: "42", IssuedAt: c.now().Add(-48 * time.Hour), ExpiresAt: c.now().Add(time.Hour)})
			var pe *provider.Error
			if !errors.As(err, &pe) || !pe.Ambiguous || pe.RefreshNotApplied {
				t.Fatalf("HTTP %d contradictory token/error response allowed old-grant reuse", status)
			}
		}
	}
}

func TestRefreshMalformedErrorEnvelopeCannotBecomeTokenSuccess(t *testing.T) {
	c, _ := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"new-access","expires_in":3600,"error":{"code":[]}}`))
	})
	_, err := c.Refresh(t.Context(), model.Token{AccessToken: "old-access", ProviderUserID: "42", IssuedAt: c.now().Add(-48 * time.Hour), ExpiresAt: c.now().Add(time.Hour)})
	var pe *provider.Error
	if !errors.As(err, &pe) || !pe.Ambiguous || pe.RefreshNotApplied {
		t.Fatal("malformed error envelope was ignored while accepting returned token fields")
	}
}
