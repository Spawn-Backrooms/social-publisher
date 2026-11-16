package security

import "testing"

func TestPublicDeliveryOriginIsExactCanonicalHTTPSDNSOrigin(t *testing.T) {
	for _, value := range []string{"https://spawn.example.com", "https://review-29.chatgpt-team.site"} {
		if !ValidPublicDeliveryOrigin(value) {
			t.Errorf("valid configured origin rejected: %q", value)
		}
	}
	for _, value := range []string{
		"", "http://spawn.example.com", "HTTPS://spawn.example.com", "https://SPAWN.example.com",
		"https://spawn.example.com/", "https://spawn.example.com/path", "https://spawn.example.com?",
		"https://spawn.example.com?x=1", "https://spawn.example.com#", "https://spawn.example.com#fragment",
		"https://user@spawn.example.com", "https://user:password@spawn.example.com", "https://spawn.example.com:443",
		"https://spawn.example.com:8443", "https://spawn.example.com.", "https://spawn.example.com\n",
		"https://127.0.0.1", "https://[::1]", "https://localhost", "https://*.example.com",
		"https://spawn.example.com\\evil.example.com", "https://spáwn.example.com", " https://spawn.example.com",
	} {
		if ValidPublicDeliveryOrigin(value) {
			t.Errorf("noncanonical or unsafe configured origin accepted: %q", value)
		}
	}
}
