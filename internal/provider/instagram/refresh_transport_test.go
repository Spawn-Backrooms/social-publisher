package instagram

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"socialpublisher/internal/model"
	"socialpublisher/internal/provider"
)

func refreshTransportToken() model.Token {
	return model.Token{AccessToken: "old-grant", ProviderUserID: "42", IssuedAt: time.Now().Add(-48 * time.Hour), ExpiresAt: time.Now().Add(24 * time.Hour)}
}

func TestRefreshCannotReplayOnWarmedConnection(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/prime" {
			_, _ = w.Write([]byte("ready"))
			return
		}
		if r.URL.Path != "/refresh_access_token" || r.Method != http.MethodGet || r.URL.Query().Get("access_token") != "old-grant" {
			t.Error("unexpected refresh request")
		}
		if calls.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close() // The grant rotated, but no response arrives.
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	hc := &http.Client{Transport: transport}
	warm, err := hc.Get(server.URL + "/prime")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, warm.Body)
	_ = warm.Body.Close()
	c := New(provider.Config{APIBase: server.URL, HTTPClient: hc})
	_, err = c.Refresh(t.Context(), refreshTransportToken())
	var pe *provider.Error
	if calls.Load() != 1 || !errors.As(err, &pe) || !pe.Ambiguous || pe.RefreshNotApplied {
		t.Fatalf("lost response: requests=%d, error=%v; a later 429 must not hide an earlier rotation", calls.Load(), err)
	}
}

func TestRefreshTransportPreservesPolicyAndDisablesInheritedHTTP2(t *testing.T) {
	var requests, dials, proxies, verified atomic.Int32
	var reused atomic.Bool
	var firstConnection atomic.Value
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			t.Error("refresh inherited an HTTP/2 transport")
		}
		if requests.Add(1) == 1 {
			firstConnection.Store(r.RemoteAddr)
		} else if firstConnection.Load() == r.RemoteAddr {
			reused.Store(true)
		}
		_, _ = w.Write([]byte(`{"access_token":"new-access","expires_in":3600}`))
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	endpoint, _ := url.Parse(server.URL)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport := &http.Transport{
		ForceAttemptHTTP2: true,
		TLSClientConfig: &tls.Config{RootCAs: roots, NextProtos: []string{"h2", "http/1.1"}, MinVersion: tls.VersionTLS12,
			VerifyConnection: func(state tls.ConnectionState) error {
				verified.Add(1)
				if len(state.VerifiedChains) == 0 {
					return errors.New("fixture requires verified TLS")
				}
				return nil
			}},
		Proxy: func(*http.Request) (*url.URL, error) { proxies.Add(1); return nil, nil },
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dials.Add(1)
			if addr != endpoint.Host {
				return nil, errors.New("fixture permits only its configured endpoint")
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	transport.Protocols.SetHTTP2(true)
	defer transport.CloseIdleConnections()
	hc := &http.Client{Transport: transport, Timeout: time.Second}
	c := New(provider.Config{APIBase: server.URL, HTTPClient: hc})
	for range 2 {
		if _, err := c.Refresh(t.Context(), refreshTransportToken()); err != nil {
			t.Fatal(err)
		}
	}
	if requests.Load() != 2 || dials.Load() != 2 || proxies.Load() != 2 || verified.Load() != 2 || reused.Load() {
		t.Fatal("refresh bypassed configured transport policy or reused a connection")
	}
	if !transport.Protocols.HTTP2() || transport.DisableKeepAlives || !transport.ForceAttemptHTTP2 {
		t.Fatal("refresh modified the shared client used by ordinary provider reads")
	}
}

type opaqueRefreshTransport struct{ calls atomic.Int32 }

func (r *opaqueRefreshTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls.Add(1)
	return nil, errors.New("opaque transport must not receive a refresh grant")
}

func TestRefreshCannotDispatchThroughOpaqueRetryPolicy(t *testing.T) {
	transport := &opaqueRefreshTransport{}
	c := New(provider.Config{APIBase: "https://fixture.invalid", HTTPClient: &http.Client{Transport: transport}})
	_, err := c.Refresh(t.Context(), refreshTransportToken())
	var pe *provider.Error
	if transport.calls.Load() != 0 || !errors.As(err, &pe) || !pe.RefreshNotApplied {
		t.Fatal("an opaque retry policy received the refresh credential")
	}
}

func TestRefreshTransportPreservesClientTimeout(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-r.Context().Done()
	}))
	defer server.Close()
	hc := &http.Client{Timeout: 50 * time.Millisecond}
	c := New(provider.Config{APIBase: server.URL, HTTPClient: hc})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err := c.Refresh(ctx, refreshTransportToken())
	var pe *provider.Error
	if ctx.Err() != nil || calls.Load() != 1 || !errors.As(err, &pe) || !pe.Ambiguous || pe.RefreshNotApplied {
		t.Fatal("configured client timeout was lost or its uncertain request retried")
	}
}

func TestRefreshLeavesOrdinaryClientConnectionReusable(t *testing.T) {
	var connections, identities, refreshes atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v26.0/me":
			identities.Add(1)
			_, _ = w.Write([]byte(`{"id":"42","user_id":"42","username":"fixture","account_type":"BUSINESS"}`))
		case "/refresh_access_token":
			refreshes.Add(1)
			_, _ = w.Write([]byte(`{"access_token":"new-access","expires_in":3600}`))
		default:
			t.Error("unexpected fixture endpoint")
		}
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	c := New(provider.Config{APIBase: server.URL, HTTPClient: &http.Client{Transport: transport}})
	for range 2 {
		if _, err := c.Identity(t.Context(), refreshTransportToken()); err != nil {
			t.Fatal(err)
		}
	}
	fresh, err := c.Refresh(t.Context(), refreshTransportToken())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Identity(t.Context(), fresh); err != nil {
		t.Fatal(err)
	}
	if identities.Load() != 3 || refreshes.Load() != 1 || connections.Load() != 2 {
		t.Fatal("refresh closed or replaced the ordinary provider client's reusable connection")
	}
}

func TestRefreshTransportDoesNotFollowCredentialRedirect(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusFound)
	}))
	defer server.Close()
	c := New(provider.Config{APIBase: server.URL, HTTPClient: server.Client()})
	_, err := c.Refresh(t.Context(), refreshTransportToken())
	var pe *provider.Error
	if redirected.Load() != 0 || !errors.As(err, &pe) || !pe.Ambiguous || pe.RefreshNotApplied {
		t.Fatal("refresh followed a redirect or treated it as definite rejection")
	}
}
