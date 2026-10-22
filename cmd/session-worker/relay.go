package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
	"mvp1/internal/appbridge"
	"mvp1/internal/egress"
	"mvp1/internal/worker"
)

func runRelay(ctx context.Context, port int) error {
	var policy egress.Policy
	if err := json.Unmarshal([]byte(os.Getenv("MVP_ACCESS")), &policy); err != nil {
		return err
	}
	var denied []netip.Prefix
	for _, raw := range strings.Split(os.Getenv("MVP_DENIED_CIDRS"), ",") {
		if raw == "" {
			continue
		}
		p, err := netip.ParsePrefix(raw)
		if err != nil {
			return err
		}
		denied = append(denied, p)
	}
	// Always deny the worker's own interfaces, irrespective of publication rules.
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return err
	}
	for _, a := range addresses {
		p, err := netip.ParsePrefix(a.String())
		if err == nil {
			denied = append(denied, netip.PrefixFrom(p.Addr(), p.Addr().BitLen()))
		}
	}
	proxy, err := egress.NewProxy(ctx, policy, denied)
	if err != nil {
		return err
	}
	client := appbridge.NewClient()
	var ice []webrtc.ICEServer
	if err = json.Unmarshal([]byte(os.Getenv("MVP_ICE_SERVERS")), &ice); err != nil {
		return err
	}
	token := os.Getenv("MVP_WORKER_TOKEN")
	w, err := worker.New(worker.Config{Token: token, Port: port, PublicIP: os.Getenv("MVP_PUBLIC_IP"), ICEServers: ice, Input: client, Capture: client.Capture})
	if err != nil {
		return err
	}
	defer w.Close()
	handler := http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(out, "unauthorized", 401)
			return
		}
		if r.URL.Path == "/attach" && r.Method == "POST" {
			var a struct{ Address, Token string }
			if json.NewDecoder(http.MaxBytesReader(out, r.Body, 1024)).Decode(&a) != nil {
				http.Error(out, "invalid attachment", 400)
				return
			}
			host, port, err := net.SplitHostPort(strings.TrimPrefix(a.Address, "http://"))
			if err != nil || net.ParseIP(host) == nil || port != "8090" || len(a.Token) != 64 || !strings.HasPrefix(a.Address, "http://") {
				http.Error(out, "invalid attachment", 400)
				return
			}
			if client.Attach(a.Address, a.Token) != nil {
				http.Error(out, "already attached", 409)
				return
			}
			out.Write([]byte(`{}`))
			return
		}
		if r.URL.Path == "/healthz" {
			if client.Health(r.Context()) != nil {
				http.Error(out, "app not ready", 503)
				return
			}
		}
		w.Handler().ServeHTTP(out, r)
	})
	networkGuard := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
			if raw := os.Getenv("MVP_APP_NETWORK"); raw != "" {
				prefix, err := netip.ParsePrefix(raw)
				host, _, e := net.SplitHostPort(r.RemoteAddr)
				ip, e2 := netip.ParseAddr(host)
				if err != nil || e != nil || e2 != nil || !prefix.Contains(ip.Unmap()) {
					http.Error(out, "forbidden", 403)
					return
				}
			}
			h.ServeHTTP(out, r)
		})
	}
	servers := []*http.Server{
		{Addr: ":8081", Handler: handler, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 25 * time.Second, WriteTimeout: 25 * time.Second},
		{Addr: ":8082", Handler: networkGuard(proxy), ReadHeaderTimeout: 3 * time.Second, MaxHeaderBytes: 32 << 10, IdleTimeout: 30 * time.Second},
		{Addr: ":8631", Handler: networkGuard(w.PrintHandler()), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 5 * time.Second, MaxHeaderBytes: 8192},
	}
	done := make(chan error, len(servers))
	for _, srv := range servers {
		defer srv.Close()
		go func() { done <- srv.ListenAndServe() }()
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ticker.C:
			if w.IdleFor() > 90*time.Second {
				return nil
			}
		}
	}
}
