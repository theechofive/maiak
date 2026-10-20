package checks

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/theechofive/maiak/backend/internal/safehttp"
)

// The guarded client cannot reach loopback outside its own package's tests, so the prober's
// own behaviour is tested with a plain client that routes hostnames to loopback TLS servers;
// the guard's error kinds are tested through a real safehttp client that never connects.

type tlsServer struct {
	srv      *httptest.Server
	notAfter time.Time
}

func newTLSServer(t *testing.T, host string, notAfter time.Time, h http.Handler) *tlsServer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: host},
		DNSNames:              []string{host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return &tlsServer{srv: srv, notAfter: notAfter.UTC().Truncate(time.Second)}
}

// routingClient sends each hostname to its loopback server and trusts their certificates.
func routingClient(t *testing.T, servers map[string]*tlsServer) *http.Client {
	t.Helper()
	roots := x509.NewCertPool()
	for _, s := range servers {
		roots.AddCert(s.srv.Certificate())
	}
	return &http.Client{Transport: &http.Transport{
		Proxy:             nil,
		DisableKeepAlives: true,
		TLSClientConfig:   &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(addr)
			s, ok := servers[host]
			if !ok {
				return nil, fmt.Errorf("no server for %s", host)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, s.srv.Listener.Addr().String())
		},
	}}
}

func TestProbeStatusAndTLS(t *testing.T) {
	notAfter := time.Now().Add(10 * 24 * time.Hour)
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("/nocontent", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("/fail", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })
	s := newTLSServer(t, "site.test", notAfter, mux)
	p := NewHTTPSProber(routingClient(t, map[string]*tlsServer{"site.test": s}))

	cases := []struct {
		path, expected string
		ok             bool
		status         int
		kind           string
	}{
		{"/ok", "", true, 200, ""},
		{"/ok", "2xx", true, 200, ""},
		{"/nocontent", "2xx", true, 204, ""},
		{"/nocontent", "204", true, 204, ""},
		{"/ok", "204", false, 200, KindUnexpectedStatus},
		{"/fail", "2xx", false, 503, KindUnexpectedStatus},
		{"/fail", "503", true, 503, ""},
	}
	for _, tc := range cases {
		t.Run(tc.path+"_"+tc.expected, func(t *testing.T) {
			before := time.Now().UTC()
			r := p.Probe(context.Background(), Request{URL: "https://site.test" + tc.path, ExpectedStatus: tc.expected})
			if r.OK != tc.ok || r.StatusCode != tc.status || r.ErrorKind != tc.kind {
				t.Fatalf("got %+v", r)
			}
			if r.TLSNotAfter == nil || !r.TLSNotAfter.Equal(s.notAfter) {
				t.Fatalf("TLSNotAfter = %v, want %v", r.TLSNotAfter, s.notAfter)
			}
			if r.CheckedAt.Before(before.Add(-time.Second)) || r.CheckedAt.Location() != time.UTC {
				t.Fatalf("CheckedAt = %v", r.CheckedAt)
			}
		})
	}
}

func TestProbeTLSExpiryOfFinalHop(t *testing.T) {
	final := newTLSServer(t, "final.test", time.Now().Add(3*24*time.Hour), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	first := newTLSServer(t, "first.test", time.Now().Add(300*24*time.Hour), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://final.test/", http.StatusMovedPermanently)
	}))
	p := NewHTTPSProber(routingClient(t, map[string]*tlsServer{"first.test": first, "final.test": final}))
	r := p.Probe(context.Background(), Request{URL: "https://first.test/"})
	if !r.OK || r.TLSNotAfter == nil || !r.TLSNotAfter.Equal(final.notAfter) {
		t.Fatalf("got %+v (final cert %v)", r, final.notAfter)
	}
}

func TestProbeDoesNotReadBody(t *testing.T) {
	var written atomic.Int64
	done := make(chan struct{})
	s := newTLSServer(t, "big.test", time.Now().Add(time.Hour), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		chunk := []byte(strings.Repeat("x", 64<<10))
		for i := 0; i < 50; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
			n, err := w.Write(chunk)
			written.Add(int64(n))
			if err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	}))
	p := NewHTTPSProber(routingClient(t, map[string]*tlsServer{"big.test": s}))
	start := time.Now()
	r := p.Probe(context.Background(), Request{URL: "https://big.test/"})
	elapsed := time.Since(start)
	if !r.OK || r.StatusCode != 200 {
		t.Fatalf("got %+v", r)
	}
	if elapsed > time.Second || r.LatencyMs > 1000 {
		t.Fatalf("probe waited for the body: elapsed %v, latency %d ms", elapsed, r.LatencyMs)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("server kept streaming: connection not closed after headers")
	}
	if w := written.Load(); w > 1<<20 {
		t.Fatalf("server wrote %d bytes; body was consumed", w)
	}
}

func TestProbeTimeout(t *testing.T) {
	s := newTLSServer(t, "slow.test", time.Now().Add(time.Hour), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	}))
	p := NewHTTPSProber(routingClient(t, map[string]*tlsServer{"slow.test": s}))
	start := time.Now()
	r := p.Probe(context.Background(), Request{URL: "https://slow.test/", Timeout: 200 * time.Millisecond})
	if r.OK || r.ErrorKind != KindTimeout || r.Cancelled { // its own deadline is not a cancellation
		t.Fatalf("got %+v", r)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout not enforced")
	}
}

// A cancelled parent context (worker shutdown) is not a failed check (ADR-0018).
func TestProbeCancelledParent(t *testing.T) {
	started := make(chan struct{})
	s := newTLSServer(t, "hang.test", time.Now().Add(time.Hour), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	p := NewHTTPSProber(routingClient(t, map[string]*tlsServer{"hang.test": s}))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()
	r := p.Probe(ctx, Request{URL: "https://hang.test/", Timeout: 5 * time.Second})
	if !r.Cancelled || r.OK || r.ErrorKind != "" || r.CheckedAt.IsZero() {
		t.Fatalf("got %+v, want a cancelled marker", r)
	}
}

func TestProbeTLSAndConnectFailures(t *testing.T) {
	s := newTLSServer(t, "site.test", time.Now().Add(time.Hour), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	// Untrusted certificate.
	untrusting := routingClient(t, map[string]*tlsServer{"site.test": s})
	untrusting.Transport.(*http.Transport).TLSClientConfig.RootCAs = x509.NewCertPool()
	if r := NewHTTPSProber(untrusting).Probe(context.Background(), Request{URL: "https://site.test/"}); r.ErrorKind != KindTLS || r.OK {
		t.Fatalf("untrusted cert: %+v", r)
	}
	// Expired certificate.
	expired := newTLSServer(t, "old.test", time.Now().Add(-time.Minute), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	if r := NewHTTPSProber(routingClient(t, map[string]*tlsServer{"old.test": expired})).Probe(context.Background(), Request{URL: "https://old.test/"}); r.ErrorKind != KindTLS {
		t.Fatalf("expired cert: %+v", r)
	}
	// Nothing listening.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	refusing := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}}}
	if r := NewHTTPSProber(refusing).Probe(context.Background(), Request{URL: "https://down.test/"}); r.ErrorKind != KindConnect {
		t.Fatalf("refused: %+v", r)
	}
	if r := NewHTTPSProber(refusing).Probe(context.Background(), Request{URL: "https://bad host/"}); r.ErrorKind != KindOther {
		t.Fatalf("invalid url: %+v", r)
	}
}

type fakeResolver func(host string) ([]netip.Addr, error)

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	return f(host)
}

// TestProbeThroughGuard uses the production client; every case is refused before connecting.
func TestProbeThroughGuard(t *testing.T) {
	resolver := fakeResolver(func(host string) ([]netip.Addr, error) {
		switch host {
		case "internal.test":
			return []netip.Addr{netip.MustParseAddr("10.0.0.7")}, nil
		case "mixed.test":
			return []netip.Addr{netip.MustParseAddr("93.184.215.14"), netip.MustParseAddr("169.254.169.254")}, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	})
	p := NewHTTPSProber(safehttp.NewClient(safehttp.Options{Resolver: resolver, MaxRedirects: 3}))
	cases := []struct{ url, kind string }{
		{"https://internal.test/", KindBlockedAddress},
		{"https://mixed.test/", KindBlockedAddress},
		{"https://127.0.0.1/", KindBlockedAddress},
		{"https://[::ffff:169.254.169.254]/", KindBlockedAddress},
		{"https://example.test:8443/", KindBadPort},
		{"https://missing.test/", KindDNS},
		{"http://example.test/", KindOther},
	}
	for _, tc := range cases {
		r := p.Probe(context.Background(), Request{URL: tc.url})
		if r.OK || r.ErrorKind != tc.kind || r.StatusCode != 0 || r.TLSNotAfter != nil {
			t.Errorf("%s: got %+v, want kind %s", tc.url, r, tc.kind)
		}
	}
}

func TestClassify(t *testing.T) {
	wrap := func(err error) error { return &url.Error{Op: "Get", URL: "https://x.test/", Err: err} }
	cases := []struct {
		err  error
		kind string
	}{
		{wrap(safehttp.ErrTooManyRedirects), KindRedirect},
		{wrap(safehttp.ErrRedirectNotHTTPS), KindRedirect},
		{wrap(fmt.Errorf("dial: %w", safehttp.ErrBlockedAddress)), KindBlockedAddress},
		{wrap(&net.OpError{Op: "dial", Err: safehttp.ErrBadPort}), KindBadPort},
		{wrap(&net.DNSError{Err: "timeout", IsTimeout: true}), KindTimeout},
		{wrap(&net.DNSError{Err: "no such host", IsNotFound: true}), KindDNS},
		{wrap(context.DeadlineExceeded), KindTimeout},
		{wrap(tls.RecordHeaderError{Msg: "not tls"}), KindTLS},
		{wrap(errors.New("boom")), KindOther},
	}
	for _, tc := range cases {
		if got := classify(context.Background(), tc.err); got != tc.kind {
			t.Errorf("classify(%v) = %s, want %s", tc.err, got, tc.kind)
		}
	}
}

// Compile-time check that the implementation satisfies the seam.
var _ Prober = (*HTTPSProber)(nil)
