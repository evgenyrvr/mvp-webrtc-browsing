package egress

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// Proxy is scoped to one immutable publication and lifetime. Its listener must
// be reachable only by that application's isolated network.
type Proxy struct {
	lookup  func(context.Context, string, string) ([]netip.Addr, error)
	dialIP  func(context.Context, string, string) (net.Conn, error)
	Policy  Policy
	Denied  []netip.Prefix
	Context context.Context
	slots   chan struct{}
}

func NewProxy(ctx context.Context, p Policy, denied []netip.Prefix) (*Proxy, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &Proxy{Policy: p, Denied: denied, Context: ctx, slots: make(chan struct{}, 64), lookup: net.DefaultResolver.LookupNetIP, dialIP: (&net.Dialer{Timeout: 5 * time.Second}).DialContext}, nil
}
func (p *Proxy) addressAllowed(ip netip.Addr, byName bool) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range p.Denied {
		if prefix.Contains(ip) {
			return false
		}
	}
	if ip.IsPrivate() && !p.Policy.AllowsIP(ip) {
		return false
	}
	return byName || p.Policy.AllowsIP(ip)
}
func (p *Proxy) dial(ctx context.Context, network, authority string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		return nil, errors.New("invalid destination")
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || !p.Policy.AllowsPort(uint16(n)) {
		return nil, errors.New("port denied")
	}
	byName := p.Policy.AllowsDomain(host)
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else {
		// Denied names must not trigger DNS (including DNS exfiltration).
		if !byName {
			return nil, errors.New("domain denied")
		}
		ips, err = p.lookup(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
	}
	if len(ips) == 0 {
		return nil, errors.New("empty DNS answer")
	}
	for _, ip := range ips {
		if !p.addressAllowed(ip, byName) {
			return nil, errors.New("address denied")
		}
	}
	// Dial the checked numeric address, never resolve again during connection.
	for _, ip := range ips {
		conn, err := p.dialIP(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, errors.New("destination unavailable")
}
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	default:
		http.Error(w, "proxy busy", 503)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stop := context.AfterFunc(p.Context, cancel)
	defer stop()
	if p.Context.Err() != nil {
		http.Error(w, "session ended", 503)
		return
	}
	if r.Method == http.MethodConnect {
		p.connect(ctx, w, r)
		return
	}
	if r.URL.Scheme != "http" || r.URL.Host == "" || r.URL.User != nil || r.Header.Get("Upgrade") != "" {
		http.Error(w, "unsupported proxy request", 400)
		return
	}
	req := r.Clone(ctx)
	req.RequestURI = ""
	req.Host = req.URL.Host
	stripHop(req.Header)
	transport := &http.Transport{Proxy: nil, DialContext: p.dial, DisableKeepAlives: true, ResponseHeaderTimeout: 15 * time.Second, MaxResponseHeaderBytes: 64 << 10}
	defer transport.CloseIdleConnections()
	resp, err := transport.RoundTrip(req)
	if err != nil {
		http.Error(w, "destination denied or unavailable", 403)
		return
	}
	defer resp.Body.Close()
	stripHop(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
func stripHop(h http.Header) {
	for _, v := range strings.Split(h.Get("Connection"), ",") {
		h.Del(strings.TrimSpace(v))
	}
	for _, k := range []string{"Connection", "Proxy-Connection", "Proxy-Authorization", "Proxy-Authenticate", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(k)
	}
}
func (p *Proxy) connect(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > 0 || len(r.TransferEncoding) > 0 || r.URL.User != nil {
		http.Error(w, "invalid CONNECT", 400)
		return
	}
	upstream, err := p.dial(ctx, "tcp", r.Host)
	if err != nil {
		http.Error(w, "destination denied or unavailable", 403)
		return
	}
	defer upstream.Close()
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "tunnel unsupported", 500)
		return
	}
	downstream, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer downstream.Close()
	_ = downstream.SetDeadline(time.Now().Add(30 * time.Minute))
	_ = upstream.SetDeadline(time.Now().Add(30 * time.Minute))
	stop := context.AfterFunc(ctx, func() { downstream.Close(); upstream.Close() })
	defer stop()
	if _, err = buf.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if buf.Flush() != nil {
		return
	}
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.Copy(upstream, buf); upstream.Close(); downstream.Close() }()
	_, _ = io.Copy(downstream, upstream)
	downstream.Close()
	upstream.Close()
	<-done
}
