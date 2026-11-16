package security

import "net/url"

// ValidPublicDeliveryOrigin accepts an exact canonical HTTPS DNS origin. It
// performs no network requests and grants no read or write access to that host.
// The lifecycle event can append only a reviewed opaque delivery path.
func ValidPublicDeliveryOrigin(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.Path != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || value != "https://"+u.Host {
		return false
	}
	host, ok := mediaHost(u.Host)
	return ok && host == u.Host
}
