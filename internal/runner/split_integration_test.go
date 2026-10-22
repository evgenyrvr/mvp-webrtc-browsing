package runner

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mvp1/internal/egress"
	"mvp1/internal/wire"
)

func TestDockerPublication(t *testing.T) {
	if os.Getenv("MVP_PUBLICATION_DOCKER_TEST") != "1" {
		t.Skip("requires Docker publication image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	network := "mvp1-publication-check"
	if _, err := docker(ctx, "network", "inspect", network); err != nil {
		if _, err = docker(ctx, "network", "create", network); err != nil {
			t.Fatal(err)
		}
	}
	image := os.Getenv("MVP_WORKER_IMAGE")
	if image == "" {
		image = "mvp1-worker:publications-testing"
	}
	r, err := New(Config{Image: image, Namespace: network, Network: network, PublicIP: "127.0.0.1", UDPBase: 50320})
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := r.Reset(cleanup); err != nil {
			t.Error(err)
		}
	}()
	hits := &atomic.Int64{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, "<html><body><h1>Publication works</h1></body></html>")
	}))
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = listener
	srv.Start()
	defer srv.Close()
	out, err := docker(ctx, "network", "inspect", network, "--format", "{{range .IPAM.Config}}{{.Gateway}}{{end}}")
	if err != nil {
		t.Fatal(err)
	}
	host := strings.TrimSpace(string(out))
	port := listener.Addr().(*net.TCPAddr).Port
	template := os.Getenv("MVP_BROWSER_APP")
	if template == "" {
		template = "falkon"
	}
	id := strings.Repeat("d", 64)
	v := wire.Start{ID: id, Template: template, StartURL: "http://" + net.JoinHostPort(host, strconv.Itoa(port)), Access: &egress.Policy{CIDRs: []string{host + "/32"}, Ports: []uint16{uint16(port)}}}
	if err = r.Start(ctx, v); err != nil {
		for _, suffix := range []string{"", "-app"} {
			out, _ := docker(context.Background(), "logs", network+"-"+id+suffix)
			t.Logf("%s logs: %s", suffix, out)
		}
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if hits.Load() == 0 {
		t.Fatal("browser never reached target through proxy")
	}
	app := network + "-" + id + "-app"
	// Bypassing proxy must fail even for the target explicitly permitted through it.
	if _, err = docker(ctx, "exec", "--user", "1000:1000", app, "timeout", "2", "bash", "-c", "exec 3<>/dev/tcp/"+host+"/"+strconv.Itoa(port)); err == nil {
		t.Fatal("direct target access bypassed proxy")
	}
	workerIP, err := dockerIP(ctx, network+"-"+id, r.splitNetwork(id))
	if err != nil {
		t.Fatal(err)
	}
	out, err = docker(ctx, "exec", "--user", "1000:1000", app, "timeout", "3", "bash", "-c", `exec 3<>/dev/tcp/"$1"/8082; printf '%s\r\n' "GET http://$2:1/ HTTP/1.1" "Host: $2" "Connection: close" "" >&3; head -n 1 <&3`, "probe", workerIP, host)
	if err != nil || !strings.Contains(string(out), "403") {
		t.Fatalf("proxy allowed forbidden port: %s %v", out, err)
	}
	if _, err = docker(ctx, "exec", "--user", "1000:1000", app, "timeout", "3", "getent", "ahostsv4", "example.org"); err == nil {
		t.Fatal("external DNS available in isolated app")
	}
	out, err = docker(ctx, "exec", network+"-"+id, "cat", "/proc/sys/net/ipv4/ip_forward")
	if err != nil || strings.TrimSpace(string(out)) != "0" {
		t.Fatalf("worker forwards traffic: %s %v", out, err)
	}
	// The isolated app must not have media/control credentials in its process env.
	out, err = docker(ctx, "inspect", app, "--format", "{{json .Config.Env}}")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "MVP_WORKER_TOKEN") || strings.Contains(string(out), "MVP_ICE_SERVERS") {
		t.Fatal("worker credentials leaked")
	}
	if err = r.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	out, err = docker(ctx, "ps", "-aq", "--filter", "label=mvp1.namespace="+network)
	if err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatal("pair leaked after stop")
	}
}
