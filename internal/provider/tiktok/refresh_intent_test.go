package tiktok

import (
	"net/http"
	"testing"
)

func TestRefreshFailureProvesNonRotationOnlyForRejection(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		status                    int
		body                      string
		proof, ambiguous, limited bool
	}{
		{"invalid_grant", 400, `{"error":"invalid_grant"}`, true, false, false},
		{"rate_limit", 429, `{"error":"rate_limit_exceeded"}`, true, false, true},
		{"structured_rate_limit", 200, `{"error":"rate_limit_exceeded"}`, true, false, true},
		{"server_error", 503, `{"error":"server_error"}`, false, true, false},
		{"timeout", 408, `{"error":"server_error"}`, false, true, false},
		{"redirect", 302, `{}`, false, true, false},
		{"malformed_success", 200, `{"access_token":"partial"}`, false, true, false},
		{"identity_mismatch", 200, `{"access_token":"new","refresh_token":"new-refresh","expires_in":3600,"refresh_expires_in":7200,"token_type":"Bearer","open_id":"different-user"}`, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newFixture(t, func(w http.ResponseWriter, _ *http.Request) { jsonReply(w, tc.status, tc.body) })
			_, err := c.Refresh(t.Context(), testToken())
			pe := providerError(t, err)
			if pe.RefreshNotApplied != tc.proof || pe.Ambiguous != tc.ambiguous || pe.RateLimited != tc.limited {
				t.Fatalf("proof=%v ambiguous=%v limited=%v; want proof=%v ambiguous=%v limited=%v", pe.RefreshNotApplied, pe.Ambiguous, pe.RateLimited, tc.proof, tc.ambiguous, tc.limited)
			}
		})
	}
}

func TestRefreshContradictoryTokenErrorBodyCannotProveNonRotation(t *testing.T) {
	for _, status := range []int{200, 400, 429} {
		for _, fields := range []string{`"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600`, `"data":[{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600}]`} {
			c := newFixture(t, func(w http.ResponseWriter, _ *http.Request) {
				jsonReply(w, status, `{`+fields+`,"error":"rate_limit_exceeded"}`)
			})
			_, err := c.Refresh(t.Context(), testToken())
			pe := providerError(t, err)
			if !pe.Ambiguous || pe.RefreshNotApplied {
				t.Fatalf("HTTP %d contradictory token/error response allowed old-grant reuse", status)
			}
		}
	}
}
