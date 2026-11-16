package instagram

import (
	"crypto/tls"
	"net/http"
)

// newRefreshHTTPClient isolates refresh from the ordinary client's connection
// pool. Go may replay a GET after a lost response on a reused connection. Each
// refresh instead uses a fresh HTTP/1 connection: HTTP/1 does not replay a
// request on a fresh connection, and HTTP/2's independent retry path is disabled.
// Clone retains the configured dial, proxy, TLS verification and timeout policy.
func newRefreshHTTPClient(client *http.Client) *http.Client {
	roundTripper := client.Transport
	if roundTripper == nil {
		roundTripper = http.DefaultTransport
	}
	base, ok := roundTripper.(*http.Transport)
	if !ok || base == nil {
		// An opaque RoundTripper can contain its own replay behavior. Do not
		// bypass its policy by silently substituting the default transport.
		return nil
	}
	transport := base.Clone()
	transport.DisableKeepAlives = true
	transport.ForceAttemptHTTP2 = false
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	transport.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	refresh := *client
	refresh.Transport = transport
	return &refresh
}
