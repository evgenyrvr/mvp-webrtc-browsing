package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"mvp1/internal/runner"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		slog.Error("runner stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	browserCPUs := flag.Float64("browser-cpus", 1.5, "Docker browser CPU limit")
	backend := flag.String("backend", "docker", "docker or kubernetes")
	pullSecret := flag.String("image-pull-secret", "", "Kubernetes registry Secret name")
	socket := flag.String("socket", "", "private Unix socket path (required)")
	image := flag.String("image", "mvp1-worker:pilot", "prebuilt application image")
	namespace := flag.String("namespace", "mvp1-testing", "owned container namespace")
	network := flag.String("network", "mvp1-testing", "preconfigured Docker network")
	publicIP := flag.String("public-ip", "127.0.0.1", "IP advertised in host ICE candidates")
	udp := flag.Int("udp-base", 50000, "first of four published UDP ports")
	flag.Parse()
	if *socket == "" {
		return errors.New("-socket required")
	}
	if net.ParseIP(*publicIP) == nil {
		return errors.New("public-ip must be a literal IP")
	}
	r, err := runner.New(runner.Config{BrowserCPUs: *browserCPUs, Backend: *backend, ImagePullSecret: *pullSecret, Image: *image, Namespace: *namespace, Network: *network, PublicIP: *publicIP, UDPBase: *udp, TURNURL: os.Getenv("MVP_TURN_URL"), TURNSecret: os.Getenv("MVP_TURN_SECRET")})
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(*socket), 0700); err != nil {
		return err
	}
	// An existing socket may belong to a live runner. Never silently replace it.
	if info, err := os.Lstat(*socket); err == nil {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || info.Mode()&os.ModeSocket == 0 || st.Uid != uint32(os.Geteuid()) {
			return errors.New("runner socket path has unexpected owner/type")
		}
		conn, dialErr := net.DialTimeout("unix", *socket, time.Second)
		if dialErr == nil {
			conn.Close()
			return errors.New("runner already listening")
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) {
			return dialErr
		}
		current, err := os.Lstat(*socket)
		if err != nil || !os.SameFile(info, current) {
			return errors.New("runner socket changed during recovery")
		}
		if err := os.Remove(*socket); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", *socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	if *backend == "kubernetes" {
		startup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := r.Reset(startup)
		cancel()
		if err != nil {
			return err
		}
	}
	if err = os.Chmod(*socket, 0600); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := &http.Server{Handler: r.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 35 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdown)
	return r.Reset(shutdown)
}
