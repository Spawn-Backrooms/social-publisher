package x

import (
	"net/http"
	"testing"
)

func TestRefreshFailureProvesNonRotationOnlyForRejection(t *testing.T) {
	for _, tc := range []struct {
		name             string
		status           int
		body             string
		proof, ambiguous bool
	}{
		{"invalid_grant", 400, `{"error":"invalid_grant"}`, true, false},
		{"rate_limit", 429, `{"error":"rate_limit"}`, true, false},
		{"server_error", 503, `{"error":"server_error"}`, false, true},
		{"timeout", 408, `{"error":"server_error"}`, false, true},
		{"redirect", 302, `{}`, false, true},
		{"malformed_success", 200, `{"access_token":"partial"}`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}, nil)
			_, err := c.Refresh(t.Context(), fixtureToken())
			pe := providerError(t, err)
			if pe.RefreshNotApplied != tc.proof || pe.Ambiguous != tc.ambiguous {
				t.Fatalf("proof=%v ambiguous=%v; want proof=%v ambiguous=%v", pe.RefreshNotApplied, pe.Ambiguous, tc.proof, tc.ambiguous)
			}
		})
	}
}

func TestRefreshExplicitInvalidReplacementDoesNotRestoreOldGrant(t *testing.T) {
	for _, field := range []string{`""`, `null`, `"   "`, `42`} {
		t.Run(field, func(t *testing.T) {
			c, _ := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":` + field + `,"token_type":"bearer","expires_in":3600}`))
			}, nil)
			fresh, err := c.Refresh(t.Context(), fixtureToken())
			if err == nil || fresh.RefreshToken != "" {
				t.Fatal("explicit invalid replacement inherited a previous refresh credential")
			}
			pe := providerError(t, err)
			if !pe.Ambiguous || pe.RefreshNotApplied {
				t.Fatal("malformed replacement was classified as a definite rejection")
			}
		})
	}
}

func TestRefreshContradictoryTokenErrorBodyCannotProveNonRotation(t *testing.T) {
	for _, status := range []int{200, 400, 429} {
		for _, fields := range []string{`"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600`, `"data":[{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}]`} {
			c, _ := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{` + fields + `,"error":"server_error"}`))
			}, nil)
			fresh, err := c.Refresh(t.Context(), fixtureToken())
			if err == nil || fresh.AccessToken != "" {
				t.Fatalf("HTTP %d contradictory token/error response accepted", status)
			}
			pe := providerError(t, err)
			if !pe.Ambiguous || pe.RefreshNotApplied {
				t.Fatalf("HTTP %d contradictory token/error response allowed old-grant reuse", status)
			}
		}
	}
}
