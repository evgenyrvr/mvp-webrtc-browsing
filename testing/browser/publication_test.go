package browser

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
)

// A deterministic corporate web target, reachable only through the session proxy.
func publicationTarget(t *testing.T, ctx context.Context, network string) (string, string, *atomic.Int64) {
	t.Helper()
	hits := &atomic.Int64{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><title>Publication test</title><body style="background:white;color:black"><h1>Published corporate application</h1><input autofocus placeholder="Type here"><p>Browser isolation test</p></body>`)
	}))
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = listener
	srv.Start()
	t.Cleanup(srv.Close)
	out, err := exec.CommandContext(ctx, "docker", "network", "inspect", network, "--format", "{{range .IPAM.Config}}{{.Gateway}}{{end}}").Output()
	if err != nil {
		t.Fatal(err)
	}
	host := strings.TrimSpace(string(out))
	if net.ParseIP(host) == nil {
		t.Fatalf("invalid bridge gateway %q", host)
	}
	port := fmt.Sprint(listener.Addr().(*net.TCPAddr).Port)
	return "http://" + net.JoinHostPort(host, port), host, hits
}
