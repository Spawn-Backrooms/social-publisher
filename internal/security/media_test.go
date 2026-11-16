package security

import (
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"socialpublisher/internal/model"
)

func testMedia() model.Media {
	return model.Media{URL: "https://media.example.com/video.mp4?signature=private-secret", MIMEType: "video/mp4", SizeBytes: 5, DurationSeconds: 1}
}

func testFetcher(t *testing.T) *MediaFetcher {
	t.Helper()
	f, err := NewMediaFetcher([]string{"media.example.com"}, 100)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestMediaURLAndMetadataPolicy(t *testing.T) {
	f := testFetcher(t)
	valid := testMedia()
	if err := f.Validate(valid); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		"http://media.example.com/video.mp4", "https://media.example.com.evil.test/video.mp4",
		"https://evilmedia.example.com/video.mp4", "https://sub.media.example.com/video.mp4",
		"https://media.example.com@evil.test/video.mp4", "https://user:private-secret@media.example.com/video.mp4",
		"https://media.example.com./video.mp4", "https://media.example.com:444/video.mp4",
		"https://media.example.com:/video.mp4", "https://media.example.com:0443/video.mp4",
		"https://127.0.0.1/video.mp4", "https://[::1]/video.mp4", "https://[::ffff:127.0.0.1]/video.mp4",
		"https://2130706433/video.mp4", "https://0177.0.0.1/video.mp4", "https://127.1/video.mp4",
		"https://0x7f.0x0.0x0.0x1/video.mp4", "https://media.example.com/video.mp4#private-secret",
		"https://media.example.com/video.mp4#", "https://media.example.com\\@evil.test/video.mp4",
		"https://media.example.com%2e.evil.test/video.mp4", "https://media.example.com\n/video.mp4",
		"https:///media.example.com/video.mp4", "//media.example.com/video.mp4",
	} {
		media := valid
		media.URL = raw
		if err := f.Validate(media); err == nil {
			t.Fatalf("unsafe URL accepted: %q", raw)
		} else if strings.Contains(err.Error(), "private-secret") {
			t.Fatal("URL credential leaked in validation error")
		}
	}
	for _, host := range []string{"*.example.com", "https://media.example.com", "media.example.com:443", "127.0.0.1", "127.1", "0x7f.0x0.0x0.0x1", "media.example.com.", "media_example.com", "localhost", "éxample.com"} {
		if _, err := NewMediaFetcher([]string{host}, 100); err == nil {
			t.Fatalf("unsafe host configuration accepted: %q", host)
		}
	}
	for _, change := range []func(*model.Media){
		func(m *model.Media) { m.SizeBytes = 0 }, func(m *model.Media) { m.SizeBytes = 101 },
		func(m *model.Media) { m.DurationSeconds = 0 }, func(m *model.Media) { m.DurationSeconds = math.NaN() },
		func(m *model.Media) { m.DurationSeconds = math.Inf(1) }, func(m *model.Media) { m.MIMEType = "text/html" },
		func(m *model.Media) { m.Width = 10 }, func(m *model.Media) { m.Height = -1 },
	} {
		media := valid
		change(&media)
		if err := f.Validate(media); err == nil {
			t.Fatal("invalid metadata accepted")
		}
	}
	valid.URL = "https://MEDIA.EXAMPLE.COM:443/video.mp4"
	if err := f.Validate(valid); err != nil {
		t.Fatal("canonical DNS host rejected")
	}
	denyAll, _ := NewMediaFetcher(nil, 100)
	if err := denyAll.Validate(valid); err == nil {
		t.Fatal("empty allowlist permitted media")
	}
}

func TestMediaIPPolicy(t *testing.T) {
	for _, raw := range []string{
		"0.0.0.0", "10.1.2.3", "100.64.1.1", "127.0.0.1", "169.254.169.254", "172.16.1.1", "192.168.1.1",
		"192.0.0.9", "192.0.2.1", "192.88.99.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "240.0.0.1", "255.255.255.255",
		"::", "::1", "::ffff:127.0.0.1", "::ffff:10.1.2.3", "::ffff:192.0.2.1", "fc00::1", "fe80::1", "ff02::1",
		"64:ff9b::7f00:1", "64:ff9b:1::a00:1", "100::1", "2001::1", "2001:db8::1", "2002:7f00:1::1", "3fff::1", "5f00::1",
		"2000::1", "2001:1000::1", "2d00::1", "3000::1", "3ffe::1",
		"fe80::1%eth0",
	} {
		if publicMediaIP(netip.MustParseAddr(raw)) {
			t.Fatalf("nonpublic address accepted: %s", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "::ffff:8.8.8.8", "2606:4700:4700::1111", "2001:4860:4860::8888"} {
		if !publicMediaIP(netip.MustParseAddr(raw)) {
			t.Fatalf("public address rejected: %s", raw)
		}
	}
}

type fakeConn struct {
	remote net.Addr
	closed bool
}

func (c *fakeConn) Read([]byte) (int, error)    { return 0, io.EOF }
func (c *fakeConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *fakeConn) Close() error                { c.closed = true; return nil }
func (c *fakeConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 50000}
}
func (c *fakeConn) RemoteAddr() net.Addr             { return c.remote }
func (c *fakeConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }

func TestMediaDialerPinsResolutionAndRejectsRebinding(t *testing.T) {
	f := testFetcher(t)
	lookups, dials := 0, 0
	f.lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
		if host != "media.example.com" {
			t.Fatal("wrong DNS hostname")
		}
		lookups++
		if lookups == 1 {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}
	f.dial = func(_ context.Context, network, address string) (net.Conn, error) {
		dials++
		if network != "tcp" || address != "8.8.8.8:443" {
			t.Fatal("dialer did not use the approved literal IP")
		}
		return &fakeConn{remote: &net.TCPAddr{IP: net.ParseIP("8.8.8.8"), Port: 443}}, nil
	}
	conn, err := f.dialContext(context.Background(), "tcp", "media.example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if _, err := f.dialContext(context.Background(), "tcp", "media.example.com:443"); err == nil || dials != 1 {
		t.Fatal("DNS change reached a private address")
	}
	f.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("::ffff:127.0.0.1")}, nil
	}
	if _, err := f.dialContext(context.Background(), "tcp", "media.example.com:443"); err == nil || dials != 1 {
		t.Fatal("mixed public/private DNS answers were accepted")
	}
	f.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	unexpected := &fakeConn{remote: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 443}}
	f.dial = func(context.Context, string, string) (net.Conn, error) { return unexpected, nil }
	if _, err := f.dialContext(context.Background(), "tcp", "media.example.com:443"); err == nil || !unexpected.closed {
		t.Fatal("unapproved connected peer was accepted or left open")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type trackedBody struct {
	io.Reader
	closed bool
}

func (b *trackedBody) Close() error { b.closed = true; return nil }

func TestMediaFetcherNoProxyRedirectsOrCredentials(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://untrusted-proxy.example:8080")
	f := testFetcher(t)
	transport := f.client.Transport.(*http.Transport)
	if transport.Proxy != nil || transport.DialContext == nil || f.client.Jar != nil {
		t.Fatal("media transport bypasses protected dialing or retains cookies")
	}
	calls := 0
	f.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" || request.URL.User != nil {
			t.Fatal("credentials were sent to a hosted-media server")
		}
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://media.example.com/other.mp4?private-secret"}, "Set-Cookie": []string{"session=secret"}}, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
	})
	if _, _, err := f.Open(context.Background(), testMedia()); err == nil || calls != 1 || strings.Contains(err.Error(), "private-secret") {
		t.Fatal("redirect was followed or signed URL leaked")
	}
}

func TestMediaLengthMIMEAndStreamEnforcement(t *testing.T) {
	for _, tc := range []struct {
		name, data, mime, encoding string
		length                     int64
		status                     int
		openError, readError       error
	}{
		{"known length", "12345", "video/mp4", "", 5, 200, nil, nil},
		{"chunked exact", "12345", "video/mp4", "", -1, 200, nil, nil},
		{"chunked overrun", "123456", "video/mp4", "", -1, 200, nil, ErrMediaLength},
		{"chunked short", "123", "video/mp4", "", -1, 200, nil, ErrMediaLength},
		{"advertised oversized", "123456", "video/mp4", "", 6, 200, ErrMediaLength, nil},
		{"advertised short", "1234", "video/mp4", "", 4, 200, ErrMediaLength, nil},
		{"wrong media type", "12345", "text/html", "", 5, 200, ErrInvalidMedia, nil},
		{"compressed content", "12345", "video/mp4", "gzip", 5, 200, ErrInvalidMedia, nil},
		{"partial response", "12345", "video/mp4", "", 5, 206, ErrMediaFetch, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testFetcher(t)
			body := &trackedBody{Reader: strings.NewReader(tc.data)}
			f.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": []string{tc.mime}, "Content-Encoding": []string{tc.encoding}}, ContentLength: tc.length, Body: body, Request: request}, nil
			})
			reader, size, err := f.Open(context.Background(), testMedia())
			if tc.openError != nil {
				if !errors.Is(err, tc.openError) || !body.closed {
					t.Fatal("bad response accepted or body left open")
				}
				return
			}
			if err != nil || size != 5 {
				t.Fatalf("open failed: %v", err)
			}
			got, readErr := io.ReadAll(reader)
			if !errors.Is(readErr, tc.readError) || len(got) > 5 {
				t.Fatalf("stream limit failed: bytes=%d error=%v", len(got), readErr)
			}
			if tc.readError == nil && string(got) != tc.data {
				t.Fatal("valid content changed")
			}
			_ = reader.Close()
			if !body.closed {
				t.Fatal("underlying response body was not closed")
			}
		})
	}
}

func TestMPEGTSMediaIsValidatedAndFetchedWithoutWeakeningOriginPolicy(t *testing.T) {
	f := testFetcher(t)
	media := testMedia()
	media.URL = "https://media.example.com/video.ts"
	media.MIMEType = "video/mp2t"
	if err := f.Validate(media); err != nil {
		t.Fatalf("supported MPEG-TS rejected by shared policy: %v", err)
	}
	f.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"video/mp2t"}}, ContentLength: 5,
			Body: io.NopCloser(strings.NewReader("12345")), Request: r}, nil
	})
	body, size, err := f.Open(t.Context(), media)
	if err != nil || size != 5 {
		t.Fatalf("MPEG-TS download rejected: size=%d err=%v", size, err)
	}
	defer body.Close()
	if data, err := io.ReadAll(body); err != nil || string(data) != "12345" {
		t.Fatalf("MPEG-TS bytes changed or length rejected: %q %v", data, err)
	}
	media.URL = "https://unapproved.example/video.ts"
	if err := f.Validate(media); err == nil {
		t.Fatal("new media type bypassed origin policy")
	}
}

func TestMediaOverrunCannotBeHiddenByExactLengthCopy(t *testing.T) {
	for _, size := range []int64{1, 5} {
		reader := &exactMediaReader{body: io.NopCloser(strings.NewReader(strings.Repeat("x", int(size)+1))), remaining: size}
		buffer := make([]byte, size)
		if n, err := io.ReadFull(reader, buffer); !errors.Is(err, ErrMediaLength) || int64(n) >= size {
			t.Fatalf("io.ReadFull hid an overrun: n=%d err=%v", n, err)
		}
		_ = reader.Close()
	}
}

func TestMediaReaderConcurrentReadAndClose(t *testing.T) {
	// X uploads close the source from a cancellation callback while a read can
	// still be in progress. Run this regression with -race.
	for i := 0; i < 20; i++ {
		reader := &exactMediaReader{body: io.NopCloser(strings.NewReader("x")), remaining: 1}
		start := make(chan struct{})
		var done sync.WaitGroup
		done.Add(2)
		go func() {
			defer done.Done()
			<-start
			var buffer [1]byte
			n, err := reader.Read(buffer[:])
			if !(n == 1 && err == io.EOF || n == 0 && errors.Is(err, ErrMediaFetch)) {
				t.Errorf("concurrent read: n=%d err=%v", n, err)
			}
		}()
		go func() {
			defer done.Done()
			<-start
			if err := reader.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
		}()
		close(start)
		done.Wait()
		if n, err := reader.Read(make([]byte, 1)); n != 0 || !errors.Is(err, ErrMediaFetch) {
			t.Fatalf("read after close: n=%d err=%v", n, err)
		}
	}
}

type errorCloseBody struct {
	io.Reader
	closes atomic.Int32
}

func (b *errorCloseBody) Close() error {
	b.closes.Add(1)
	return errors.New("private source failure")
}

func TestMediaReaderConcurrentCloseClosesBodyOnce(t *testing.T) {
	body := &errorCloseBody{Reader: strings.NewReader("x")}
	reader := &exactMediaReader{body: body, remaining: 1}
	start := make(chan struct{})
	var done sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			<-start
			errs[i] = reader.Close()
		}(i)
	}
	close(start)
	done.Wait()
	if calls := body.closes.Load(); calls != 1 {
		t.Fatalf("underlying source closed %d times", calls)
	}
	for _, err := range errs {
		if err != ErrMediaFetch {
			t.Fatalf("close did not preserve its sanitized result: %v", err)
		}
	}
}

type signallingReadCloser struct {
	io.ReadCloser
	started chan struct{}
}

func (b *signallingReadCloser) Read(p []byte) (int, error) {
	close(b.started)
	return b.ReadCloser.Read(p)
}

func TestMediaReaderCloseUnblocksRead(t *testing.T) {
	source, writer := io.Pipe()
	defer writer.Close()
	body := &signallingReadCloser{ReadCloser: source, started: make(chan struct{})}
	reader := &exactMediaReader{body: body, remaining: 1}
	readDone := make(chan error, 1)
	go func() {
		_, err := reader.Read(make([]byte, 1))
		readDone <- err
	}()
	<-body.started
	closeDone := make(chan error, 1)
	go func() { closeDone <- reader.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close blocked behind an unfinished media read")
	}
	select {
	case err := <-readDone:
		if !errors.Is(err, ErrMediaFetch) {
			t.Fatalf("blocked read: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not unblock the media read")
	}
}
