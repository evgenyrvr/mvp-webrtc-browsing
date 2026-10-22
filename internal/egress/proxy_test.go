package egress

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestProxyPinsDNSAndStripsCredentials(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "allowed.example" || r.Header.Get("Proxy-Authorization") != "" {
			t.Errorf("incorrect upstream headers: %v", r.Header)
		}
		w.Write([]byte("allowed"))
	}))
	defer target.Close()
	p, _ := NewProxy(context.Background(), Policy{Domains: []string{"allowed.example"}, Ports: []uint16{80}}, nil)
	lookups, dials := 0, 0
	p.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		lookups++
		return []netip.Addr{netip.MustParseAddr("192.0.2.10")}, nil
	}
	p.dialIP = func(ctx context.Context, network, address string) (net.Conn, error) {
		dials++
		if address != "192.0.2.10:80" {
			t.Errorf("unpinned dial: %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, target.Listener.Addr().String())
	}
	req := httptest.NewRequest("GET", "http://allowed.example/", nil)
	req.Header.Set("Proxy-Authorization", "Basic secret")
	out := httptest.NewRecorder()
	p.ServeHTTP(out, req)
	if out.Code != 200 || out.Body.String() != "allowed" || lookups != 1 || dials != 1 {
		t.Fatalf("request failed: %d %s lookup=%d dial=%d", out.Code, out.Body, lookups, dials)
	}
	out = httptest.NewRecorder()
	p.ServeHTTP(out, httptest.NewRequest("GET", "http://denied.example/", nil))
	if out.Code != 403 || lookups != 1 || dials != 1 {
		t.Fatal("denied domain performed network access")
	}
}
func TestProxyRejectsMixedDNSAndProtectedAddresses(t *testing.T) {
	p, _ := NewProxy(context.Background(), Policy{Domains: []string{"allowed.example"}, CIDRs: []string{"10.0.0.0/8"}, Ports: []uint16{443}}, []netip.Prefix{netip.MustParsePrefix("10.1.0.0/16")})
	p.dialIP = func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("denied address dialed")
		return nil, io.EOF
	}
	for _, bad := range []string{"127.0.0.1", "::ffff:127.0.0.1", "169.254.169.254", "10.1.2.3", "::1", "fe80::1", "224.0.0.1"} {
		p.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr(bad)}, nil
		}
		if _, err := p.dial(context.Background(), "tcp", "allowed.example:443"); err == nil {
			t.Errorf("accepted mixed DNS with %s", bad)
		}
	}
	if p.addressAllowed(netip.MustParseAddr("192.168.1.1"), true) {
		t.Fatal("domain grants implicit private access")
	}
	if !p.addressAllowed(netip.MustParseAddr("10.2.0.1"), true) {
		t.Fatal("explicit corporate CIDR denied")
	}
}
func TestProxyRejectsPortsAndClosedSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p, _ := NewProxy(ctx, Policy{Domains: []string{"allowed.example"}, Ports: []uint16{443}}, nil)
	p.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		t.Fatal("forbidden port resolved")
		return nil, io.EOF
	}
	if _, err := p.dial(ctx, "tcp", "allowed.example:22"); err == nil {
		t.Fatal("SSH allowed")
	}
	cancel()
	out := httptest.NewRecorder()
	p.ServeHTTP(out, httptest.NewRequest("GET", "http://allowed.example/", nil))
	if out.Code != 503 {
		t.Fatal("expired proxy still accepts requests")
	}
}
