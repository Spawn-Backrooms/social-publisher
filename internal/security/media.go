package security

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
)

var (
	ErrInvalidMedia = errors.New("hosted media URL or metadata is invalid")
	ErrMediaFetch   = errors.New("hosted media could not be fetched")
	ErrMediaLength  = errors.New("hosted media length does not match its metadata")
	errMediaAddress = errors.New("hosted media address is not permitted")
	errRedirect     = errors.New("hosted media redirects are not permitted")
)

var mediaMIMEs = map[string]bool{
	"image/jpeg": true, "image/png": true, "image/gif": true, "image/webp": true,
	"video/mp4": true, "video/quicktime": true, "video/webm": true, "video/mp2t": true,
}

// MediaFetcher permits only explicitly configured public HTTPS origins. An
// empty host list is a valid deny-all configuration for text-only deployments.
// Configuration is immutable after construction and safe for concurrent use.
type MediaFetcher struct {
	hosts    map[string]struct{}
	maxBytes int64
	client   *http.Client
	lookup   func(context.Context, string) ([]netip.Addr, error)
	dial     func(context.Context, string, string) (net.Conn, error)
}

var _ provider.MediaFetcher = (*MediaFetcher)(nil)

func NewMediaFetcher(hosts []string, maxBytes int64) (*MediaFetcher, error) {
	if maxBytes <= 0 {
		return nil, errors.New("media byte limit must be positive")
	}
	f := &MediaFetcher{hosts: make(map[string]struct{}, len(hosts)), maxBytes: maxBytes}
	for _, host := range hosts {
		canonical, ok := mediaHost(host)
		if !ok {
			return nil, errors.New("media allowlist must contain exact DNS hostnames")
		}
		f.hosts[canonical] = struct{}{}
	}
	f.lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
		return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	f.dial = dialer.DialContext
	transport := &http.Transport{
		// Never use ProxyFromEnvironment: a proxy could resolve the original
		// hostname itself and bypass this transport's protected dialer.
		Proxy:                  nil,
		DialContext:            f.dialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           32,
		MaxIdleConnsPerHost:    4,
		MaxConnsPerHost:        8,
		IdleConnTimeout:        60 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  20 * time.Second,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		DisableCompression:     true,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
	}
	f.client = &http.Client{
		Transport: transport,
		Timeout:   10 * time.Minute,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return errRedirect
		},
		// No cookie jar; each request is unauthenticated.
		Jar: nil,
	}
	return f, nil
}

// Validate is a deterministic URL/metadata check; actual network destinations
// are checked when dialing, so cached validation cannot authorize a DNS change.
func (f *MediaFetcher) Validate(media model.Media) error {
	if f == nil || media.SizeBytes <= 0 || media.SizeBytes > f.maxBytes || !mediaMIMEs[media.MIMEType] ||
		math.IsNaN(media.DurationSeconds) || math.IsInf(media.DurationSeconds, 0) || media.DurationSeconds < 0 ||
		strings.HasPrefix(media.MIMEType, "video/") && media.DurationSeconds <= 0 ||
		media.Width < 0 || media.Height < 0 || media.Width > 65535 || media.Height > 65535 ||
		(media.Width == 0) != (media.Height == 0) {
		return ErrInvalidMedia
	}
	_, err := f.parseURL(media.URL)
	return err
}

func (f *MediaFetcher) parseURL(raw string) (*url.URL, error) {
	if len(raw) == 0 || len(raw) > 8192 || strings.ContainsAny(raw, "\\#") {
		return nil, ErrInvalidMedia
	}
	for _, c := range raw {
		if c <= 0x20 || c == 0x7f {
			return nil, ErrInvalidMedia
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Fragment != "" || u.Host == "" {
		return nil, ErrInvalidMedia
	}
	host, ok := mediaHost(u.Hostname())
	if !ok {
		return nil, ErrInvalidMedia
	}
	if _, allowed := f.hosts[host]; !allowed {
		return nil, ErrInvalidMedia
	}
	// Reject empty, padded or nonstandard port spellings in addition to
	// non-443 ports. DNS hostnames are case-insensitive, but no suffix match
	// or trailing-dot alias is accepted.
	authority := strings.ToLower(u.Host)
	if authority != host && authority != host+":443" {
		return nil, ErrInvalidMedia
	}
	u.Host = authority
	return u, nil
}

func mediaHost(value string) (string, bool) {
	host := strings.ToLower(value)
	if len(host) == 0 || len(host) > 253 || strings.HasSuffix(host, ".") || strings.ContainsAny(host, ":/\\@%?#[]*") {
		return "", false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return "", false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return "", false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", false
			}
		}
	}
	// A numeric final label catches abbreviated, octal and integer IPv4
	// forms; all-hex components catch legacy hexadecimal IPv4 spellings.
	tldHasLetter := false
	for _, c := range labels[len(labels)-1] {
		tldHasLetter = tldHasLetter || c >= 'a' && c <= 'z'
	}
	if !tldHasLetter || ipLikeLabels(labels) {
		return "", false
	}
	return host, true
}

func ipLikeLabels(labels []string) bool {
	for _, label := range labels {
		digits := label
		hex := strings.HasPrefix(label, "0x")
		if hex {
			digits = label[2:]
		}
		if len(digits) == 0 {
			return false
		}
		for _, c := range digits {
			if !(c >= '0' && c <= '9' || hex && c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}

func (f *MediaFetcher) Open(ctx context.Context, media model.Media) (io.ReadCloser, int64, error) {
	if err := f.Validate(media); err != nil {
		return nil, 0, err
	}
	u, _ := f.parseURL(media.URL)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, ErrInvalidMedia
	}
	request.Header.Set("Accept", media.MIMEType)
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("User-Agent", "socialpublisher-media/1")
	response, err := f.client.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, 0, ErrMediaFetch
	}
	if response.StatusCode != http.StatusOK || response.Body == nil {
		if response.Body != nil {
			_ = response.Body.Close()
		}
		return nil, 0, ErrMediaFetch
	}
	fail := func(err error) (io.ReadCloser, int64, error) {
		_ = response.Body.Close()
		return nil, 0, err
	}
	contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || !mediaMIMEs[contentType] || contentType != media.MIMEType ||
		response.Header.Get("Content-Encoding") != "" && !strings.EqualFold(response.Header.Get("Content-Encoding"), "identity") {
		return fail(ErrInvalidMedia)
	}
	if response.ContentLength >= 0 && response.ContentLength != media.SizeBytes {
		return fail(ErrMediaLength)
	}
	return &exactMediaReader{body: response.Body, remaining: media.SizeBytes}, media.SizeBytes, nil
}

// dialContext resolves once, validates every answer, then dials a literal IP.
// The actual connected peer must be that same approved public IP. TLS still
// authenticates the original hostname through the normal HTTP transport.
func (f *MediaFetcher) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port != "443" || network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, errMediaAddress
	}
	canonical, valid := mediaHost(host)
	if !valid {
		return nil, errMediaAddress
	}
	if _, allowed := f.hosts[canonical]; !allowed {
		return nil, errMediaAddress
	}
	addresses, err := f.lookup(ctx, canonical)
	if err != nil || len(addresses) == 0 || len(addresses) > 64 {
		return nil, errMediaAddress
	}
	for _, ip := range addresses {
		if !publicMediaIP(ip) {
			return nil, errMediaAddress
		}
	}
	for _, ip := range addresses {
		ip = ip.Unmap()
		if network == "tcp4" && !ip.Is4() || network == "tcp6" && !ip.Is6() {
			continue
		}
		connection, err := f.dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if err != nil {
			continue
		}
		peer, err := netip.ParseAddrPort(connection.RemoteAddr().String())
		if err != nil || peer.Port() != 443 || !publicMediaIP(peer.Addr()) || peer.Addr().Unmap() != ip {
			_ = connection.Close()
			return nil, errMediaAddress
		}
		return connection, nil
	}
	return nil, ErrMediaFetch
}

// Block special-purpose IPv4 networks, including documentation, transition,
// shared-address, benchmark and reserved ranges. Deliberately reject whole
// special-purpose allocations even if an individual anycast address is public.
var blockedIPv4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.31.196.0/24"),
	netip.MustParsePrefix("192.52.193.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("192.175.48.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// RIR allocations from IANA's IPv6 Global Unicast Address Space registry,
// checked 2026-10-05. Unlisted space is reserved, so future allocations fail
// closed until this list is updated. Special-purpose subranges remain blocked.
// https://www.iana.org/assignments/ipv6-unicast-address-assignments/
var allocatedIPv6 = []netip.Prefix{
	netip.MustParsePrefix("2001:200::/23"),
	netip.MustParsePrefix("2001:400::/23"),
	netip.MustParsePrefix("2001:600::/23"),
	netip.MustParsePrefix("2001:800::/22"),
	netip.MustParsePrefix("2001:c00::/23"),
	netip.MustParsePrefix("2001:e00::/23"),
	netip.MustParsePrefix("2001:1200::/23"),
	netip.MustParsePrefix("2001:1400::/22"),
	netip.MustParsePrefix("2001:1800::/23"),
	netip.MustParsePrefix("2001:1a00::/23"),
	netip.MustParsePrefix("2001:1c00::/22"),
	netip.MustParsePrefix("2001:2000::/19"),
	netip.MustParsePrefix("2001:4000::/23"),
	netip.MustParsePrefix("2001:4200::/23"),
	netip.MustParsePrefix("2001:4400::/23"),
	netip.MustParsePrefix("2001:4600::/23"),
	netip.MustParsePrefix("2001:4800::/23"),
	netip.MustParsePrefix("2001:4a00::/23"),
	netip.MustParsePrefix("2001:4c00::/23"),
	netip.MustParsePrefix("2001:5000::/20"),
	netip.MustParsePrefix("2001:8000::/19"),
	netip.MustParsePrefix("2001:a000::/20"),
	netip.MustParsePrefix("2001:b000::/20"),
	netip.MustParsePrefix("2003::/18"),
	netip.MustParsePrefix("2400::/12"),
	netip.MustParsePrefix("2410::/12"),
	netip.MustParsePrefix("2600::/12"),
	netip.MustParsePrefix("2610::/23"),
	netip.MustParsePrefix("2620::/23"),
	netip.MustParsePrefix("2630::/12"),
	netip.MustParsePrefix("2800::/12"),
	netip.MustParsePrefix("2a00::/12"),
	netip.MustParsePrefix("2a10::/12"),
	netip.MustParsePrefix("2c00::/12"),
}
var blockedIPv6 = []netip.Prefix{
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2620:4f:8000::/48"),
	netip.MustParsePrefix("3fff::/20"),
}

func publicMediaIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
		return false
	}
	blocked := blockedIPv4
	if ip.Is6() {
		allocated := false
		for _, prefix := range allocatedIPv6 {
			if prefix.Contains(ip) {
				allocated = true
				break
			}
		}
		if !allocated {
			return false
		}
		blocked = blockedIPv6
	}
	for _, prefix := range blocked {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

// exactMediaReader never yields more bytes than were authorized. It probes for
// an extra byte before reporting successful completion, including for chunked
// responses, and reports a short body as a length mismatch. On a failed final
// probe it withholds the final byte, so io.ReadFull/io.CopyN cannot discard the
// error merely because they received their requested byte count.
type exactMediaReader struct {
	body      io.ReadCloser
	remaining int64
	terminal  error
	closed    atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

func (r *exactMediaReader) Read(p []byte) (int, error) {
	if r.closed.Load() {
		return 0, ErrMediaFetch
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.terminal != nil {
		return 0, r.terminal
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.body.Read(p)
	r.remaining -= int64(n)
	if r.remaining > 0 {
		if err == io.EOF {
			r.terminal = ErrMediaLength
			return n, r.terminal
		}
		if err != nil {
			r.terminal = ErrMediaFetch
			return n, r.terminal
		}
		return n, nil
	}
	if err == io.EOF {
		r.terminal = io.EOF
		return n, io.EOF
	}
	if err != nil {
		r.terminal = ErrMediaFetch
		return n - 1, r.terminal
	}
	var probe [1]byte
	count, probeErr := io.ReadFull(r.body, probe[:])
	if count != 0 {
		r.terminal = ErrMediaLength
	} else if probeErr == io.EOF {
		r.terminal = io.EOF
	} else {
		r.terminal = ErrMediaFetch
	}
	if r.terminal != io.EOF {
		return n - 1, r.terminal
	}
	return n, r.terminal
}

func (r *exactMediaReader) Close() error {
	// Cancellation can close the source concurrently with a blocked Read and
	// the upload's deferred Close. Never hold a read lock across body.Read:
	// closing the HTTP body must remain able to unblock that read.
	r.closeOnce.Do(func() {
		r.closed.Store(true)
		if r.body.Close() != nil {
			r.closeErr = ErrMediaFetch
		}
	})
	return r.closeErr
}
