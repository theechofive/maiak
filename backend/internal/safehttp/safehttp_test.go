package safehttp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// publicIP stands in for a real public address; the test dial override sends the validated
// connection to the loopback test server instead.
var publicIP = netip.MustParseAddr("93.184.215.14")

type fakeResolver struct {
	mu    sync.Mutex
	calls int
	fn    func(call int, host string) ([]netip.Addr, error)
}

func (f *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	return f.fn(n, host)
}

func (f *fakeResolver) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func static(addrs ...string) *fakeResolver {
	return &fakeResolver{fn: func(int, string) ([]netip.Addr, error) {
		out := make([]netip.Addr, len(addrs))
		for i, a := range addrs {
			out[i] = netip.MustParseAddr(a)
		}
		return out, nil
	}}
}

// testServer is a loopback TLS server with a certificate for example.test.
type testServer struct {
	srv   *httptest.Server
	roots *x509.CertPool
	mu    sync.Mutex
	dials []string
}

func newTestServer(t *testing.T, h http.Handler) *testServer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "example.test"},
		DNSNames:              []string{"example.test", "other.test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return &testServer{srv: srv, roots: roots}
}

// options returns guard options whose validated dials land on the test server.
func (ts *testServer) options(o Options) Options {
	o.rootCAs = ts.roots
	o.dialOverride = func(ctx context.Context, network, validatedAddr string) (net.Conn, error) {
		ts.mu.Lock()
		ts.dials = append(ts.dials, validatedAddr)
		ts.mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, network, ts.srv.Listener.Addr().String())
	}
	return o
}

func (ts *testServer) dialed() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]string(nil), ts.dials...)
}

func get(t *testing.T, c *http.Client, url string) (*http.Response, error) {
	t.Helper()
	resp, err := c.Get(url)
	if resp != nil {
		resp.Body.Close()
	}
	return resp, err
}

func TestAllowed(t *testing.T) {
	blocked := []string{
		"0.0.0.0", "0.1.2.3", "10.0.0.1", "10.255.255.255", "100.64.0.1", "100.127.255.254",
		"127.0.0.1", "127.8.9.10", "169.254.169.254", "169.254.0.1", "172.16.0.1", "172.31.255.255",
		"192.168.1.1", "192.0.0.8", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1",
		"224.0.0.1", "239.255.255.250", "240.0.0.1", "255.255.255.255",
		"::", "::1", "fc00::1", "fd12:3456::1", "fe80::1", "fe80::1%eth0", "ff02::1", "fec0::1",
		"::ffff:127.0.0.1", "::ffff:169.254.169.254", "::ffff:10.0.0.1", "::127.0.0.1", "::a00:1",
		"64:ff9b::7f00:1", "64:ff9b::808:808", "64:ff9b:1::1", "2002:7f00:1::1", "2002:808:808::1",
		"2001:0:4136:e378:8000:63bf:3fff:fdd2", "2001:db8::1", "100::1",
	}
	for _, s := range blocked {
		if allowed(netip.MustParseAddr(s), nil) {
			t.Errorf("%s must be blocked", s)
		}
	}
	for _, s := range []string{"93.184.215.14", "1.1.1.1", "8.8.8.8", "::ffff:8.8.8.8", "2606:4700:4700::1111", "2a00:1450:4001::1"} {
		if !allowed(netip.MustParseAddr(s), nil) {
			t.Errorf("%s must be allowed", s)
		}
	}
	extra := []netip.Prefix{netip.MustParsePrefix("93.184.0.0/16"), netip.MustParsePrefix("::ffff:1.1.1.0/120"), netip.MustParsePrefix("2a00::/16")}
	for _, s := range []string{"93.184.215.14", "::ffff:93.184.215.14", "1.1.1.1", "2a00:1450:4001::1"} {
		if allowed(netip.MustParseAddr(s), extra) {
			t.Errorf("%s must be blocked by the extra prefixes", s)
		}
	}
	if !allowed(netip.MustParseAddr("8.8.8.8"), extra) {
		t.Error("8.8.8.8 must stay allowed")
	}
}

func TestResolutionRules(t *testing.T) {
	cases := []struct {
		name     string
		url      string
		resolver *fakeResolver
		want     error
		dnsErr   bool
	}{
		{"loopback hostname", "https://example.test/", static("127.0.0.1"), ErrBlockedAddress, false},
		{"metadata hostname", "https://example.test/", static("169.254.169.254"), ErrBlockedAddress, false},
		{"mapped loopback", "https://example.test/", static("::ffff:127.0.0.1"), ErrBlockedAddress, false},
		{"nat64 embedding", "https://example.test/", static("64:ff9b::a9fe:a9fe"), ErrBlockedAddress, false},
		{"mixed allowed and blocked", "https://example.test/", static("93.184.215.14", "10.0.0.5"), ErrBlockedAddress, false},
		{"mixed blocked first", "https://example.test/", static("fd00::1", "93.184.215.14"), ErrBlockedAddress, false},
		{"ip literal loopback", "https://127.0.0.1/", static(), ErrBlockedAddress, false},
		{"ip literal metadata", "https://169.254.169.254/latest/meta-data/", static(), ErrBlockedAddress, false},
		{"ipv6 literal loopback", "https://[::1]/", static(), ErrBlockedAddress, false},
		{"ipv6 literal mapped", "https://[::ffff:a9fe:a9fe]/", static(), ErrBlockedAddress, false},
		{"port 8443", "https://example.test:8443/", static("93.184.215.14"), ErrBadPort, false},
		{"port 80 https", "https://example.test:80/", static("93.184.215.14"), ErrBadPort, false},
		{"plain http", "http://example.test/", static("93.184.215.14"), ErrNotHTTPS, false},
		{"plain http on 443", "http://example.test:443/", static("93.184.215.14"), ErrNotHTTPS, false},
		{"nxdomain", "https://missing.test/", &fakeResolver{fn: func(int, string) ([]netip.Addr, error) {
			return nil, &net.DNSError{Err: "no such host", IsNotFound: true}
		}}, nil, true},
		{"empty answer", "https://empty.test/", static(), nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
			c := NewClient(ts.options(Options{Resolver: tc.resolver, MaxRedirects: 3}))
			_, err := get(t, c, tc.url)
			if tc.dnsErr {
				var de *net.DNSError
				if !errors.As(err, &de) {
					t.Fatalf("want DNS error, got %v", err)
				}
			} else if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			if d := ts.dialed(); len(d) != 0 {
				t.Fatalf("nothing may be dialed, dialed %v", d)
			}
			if err != nil && (strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "10.0.0.5") || strings.Contains(err.Error(), "169.254")) && !strings.Contains(tc.url, "127.0.0.1") && !strings.Contains(tc.url, "169.254") {
				t.Fatalf("error leaks a resolved address: %v", err)
			}
		})
	}
}

func TestAllowedRequestDialsValidatedIP(t *testing.T) {
	ts := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	c := NewClient(ts.options(Options{Resolver: static("93.184.215.14")}))
	resp, err := get(t, c, "https://example.test/")
	if err != nil || resp.StatusCode != 204 {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	if d := ts.dialed(); len(d) != 1 || d[0] != "93.184.215.14:443" {
		t.Fatalf("dialed %v", d)
	}
}

func TestDNSRebinding(t *testing.T) {
	ts := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	r := &fakeResolver{fn: func(call int, _ string) ([]netip.Addr, error) {
		if call == 1 {
			return []netip.Addr{publicIP}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}}
	c := NewClient(ts.options(Options{Resolver: r}))
	if _, err := get(t, c, "https://example.test/"); err != nil {
		t.Fatal(err)
	}
	if r.count() != 1 {
		t.Fatalf("one connection must resolve once, resolved %d times", r.count())
	}
	if d := ts.dialed(); len(d) != 1 || d[0] != "93.184.215.14:443" {
		t.Fatalf("connection must go to the validated IP, dialed %v", d)
	}
	// The next connection sees the rebound answer and is refused.
	if _, err := get(t, c, "https://example.test/"); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("rebound lookup: %v", err)
	}
	if d := ts.dialed(); len(d) != 1 {
		t.Fatalf("rebound address dialed: %v", d)
	}
}

// TestControlAtConnectTime checks the socket-level hook of the production dialer on a real
// connect: a loopback listener on a non-443 port and a loopback address on 443 are refused.
func TestControlAtConnectTime(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			c.Close()
		}
	}()
	g := &guard{resolver: static()}
	g.dialer = &net.Dialer{Timeout: time.Second, Control: g.control}
	if _, err := g.dialer.DialContext(context.Background(), "tcp", ln.Addr().String()); !errors.Is(err, ErrBadPort) {
		t.Fatalf("non-443 port: %v", err)
	}
	if _, err := g.dialer.DialContext(context.Background(), "tcp", "127.0.0.1:443"); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("loopback: %v", err)
	}
	for _, addr := range []string{"10.0.0.1:443", "[::ffff:169.254.169.254]:443", "[fe80::1]:443"} {
		if err := g.control("tcp", addr, nil); !errors.Is(err, ErrBlockedAddress) {
			t.Fatalf("control(%s) = %v", addr, err)
		}
	}
	if err := g.control("tcp", "93.184.215.14:443", nil); err != nil {
		t.Fatalf("public address refused: %v", err)
	}
	if accepted.Load() != 0 {
		t.Fatal("a refused dial reached the listener")
	}
	// The client wires the same hook into its dialer.
	c := NewClient(Options{})
	tr := c.Transport.(schemeCheck).next.(*http.Transport)
	if tr.Proxy != nil || tr.TLSClientConfig.MinVersion != tls.VersionTLS12 || tr.TLSClientConfig.RootCAs != nil || c.Jar != nil {
		t.Fatal("transport is not hardened")
	}
}

func TestRedirects(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/chain/", func(w http.ResponseWriter, r *http.Request) {
		var n int
		for _, c := range strings.TrimPrefix(r.URL.Path, "/chain/") {
			n = n*10 + int(c-'0')
		}
		if n == 0 {
			w.WriteHeader(200)
			return
		}
		http.Redirect(w, r, "https://other.test/chain/"+string(rune('0'+n-1)), http.StatusFound)
	})
	mux.HandleFunc("/to-http", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://other.test/", http.StatusFound)
	})
	mux.HandleFunc("/to-metadata", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://169.254.169.254/latest/meta-data/", http.StatusFound)
	})
	mux.HandleFunc("/to-mapped", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://[::ffff:7f00:1]/", http.StatusFound)
	})
	mux.HandleFunc("/to-port", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://other.test:8443/", http.StatusFound)
	})
	mux.HandleFunc("/to-internal-name", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://internal.test/", http.StatusFound)
	})
	resolver := &fakeResolver{fn: func(_ int, host string) ([]netip.Addr, error) {
		if host == "internal.test" {
			return []netip.Addr{netip.MustParseAddr("10.1.2.3")}, nil
		}
		return []netip.Addr{publicIP}, nil
	}}
	cases := []struct {
		path      string
		max       int
		want      error
		wantDials int
	}{
		{"/chain/3", 3, nil, 4},
		{"/chain/4", 3, ErrTooManyRedirects, 4},
		{"/chain/1", 0, ErrTooManyRedirects, 1},
		{"/chain/9", 10, ErrTooManyRedirects, 4}, // clamped to 3
		{"/to-http", 3, ErrRedirectNotHTTPS, 1},
		{"/to-metadata", 3, ErrBlockedAddress, 1},
		{"/to-mapped", 3, ErrBlockedAddress, 1},
		{"/to-port", 3, ErrBadPort, 1},
		{"/to-internal-name", 3, ErrBlockedAddress, 1},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			ts := newTestServer(t, mux)
			c := NewClient(ts.options(Options{Resolver: resolver, MaxRedirects: tc.max}))
			_, err := get(t, c, "https://example.test"+tc.path)
			if tc.want == nil && err != nil || tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			d := ts.dialed()
			if len(d) != tc.wantDials {
				t.Fatalf("dials = %v, want %d", d, tc.wantDials)
			}
			for _, a := range d {
				if a != "93.184.215.14:443" {
					t.Fatalf("dialed unvalidated address %s", a)
				}
			}
		})
	}
}

func TestProxyEnvironmentIgnored(t *testing.T) {
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	var hits atomic.Int32
	go func() {
		for {
			c, err := proxy.Accept()
			if err != nil {
				return
			}
			hits.Add(1)
			c.Close()
		}
	}()
	proxyURL := "http://" + proxy.Addr().String()
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY"} {
		t.Setenv(k, proxyURL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	ts := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	c := NewClient(ts.options(Options{Resolver: static("93.184.215.14")}))
	if _, err := get(t, c, "https://example.test/"); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 0 {
		t.Fatal("the proxy from the environment was contacted")
	}
	if d := ts.dialed(); len(d) != 1 || d[0] != "93.184.215.14:443" {
		t.Fatalf("dialed %v", d)
	}
}

func TestDeadlineCoversRedirects(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		http.Redirect(w, r, "https://other.test/slow", http.StatusFound)
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(150 * time.Millisecond):
		case <-r.Context().Done():
		}
	})
	ts := newTestServer(t, mux)
	c := NewClient(ts.options(Options{Resolver: static("93.184.215.14"), MaxRedirects: 3, Timeout: 250 * time.Millisecond}))
	start := time.Now()
	_, err := get(t, c, "https://example.test/start")
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("want timeout, got %v", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("deadline not enforced: %v", el)
	}
}

// TestOverrideNotSetOutsideTests enforces that the test-only fields (unexported, so settable
// only inside this package) are never assigned by non-test files, and that no exported
// identifier offers an equivalent escape hatch.
func TestOverrideNotSetOutsideTests(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		af, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				for _, lhs := range x.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && (sel.Sel.Name == "dialOverride" || sel.Sel.Name == "rootCAs") {
						t.Errorf("%s: %s assigned outside tests", fset.Position(x.Pos()), sel.Sel.Name)
					}
				}
			case *ast.KeyValueExpr:
				if id, ok := x.Key.(*ast.Ident); ok && (id.Name == "dialOverride" || id.Name == "rootCAs") {
					t.Errorf("%s: %s set outside tests", fset.Position(x.Pos()), id.Name)
				}
			}
			return true
		})
		for name, obj := range af.Scope.Objects {
			lower := strings.ToLower(name)
			if ast.IsExported(name) && (strings.Contains(lower, "test") || strings.Contains(lower, "override") || strings.Contains(lower, "insecure") || strings.Contains(lower, "loopback")) {
				t.Errorf("%s: exported escape hatch %s (%v)", f, name, obj.Kind)
			}
		}
	}
}
