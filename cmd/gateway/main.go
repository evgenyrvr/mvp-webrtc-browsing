package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"mvp1/internal/runner"
	"mvp1/internal/server"
	"mvp1/internal/sessions"
	"mvp1/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP listen address")
	dbPath := flag.String("db", "", "SQLite file (required; separate testing/deploy)")
	origin := flag.String("origin", "http://127.0.0.1:8080", "exact public origin")
	cert := flag.String("tls-cert", "", "TLS certificate file")
	key := flag.String("tls-key", "", "TLS key file")
	runnerSocket := flag.String("runner-socket", "", "private runner socket; empty disables application launch")
	relayOnly := flag.Bool("relay-only", false, "require TURN relay for browser WebRTC")
	flag.Parse()
	if *dbPath == "" {
		return errors.New("-db is required")
	}
	u, err := url.Parse(*origin)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return errors.New("-origin must be an origin without path, query or credentials")
	}
	if (*cert == "") != (*key == "") {
		return errors.New("both TLS certificate and key are required")
	}
	secure := *cert != ""
	if secure && u.Scheme != "https" {
		return errors.New("TLS requires https origin")
	}
	if !secure {
		host, _, err := net.SplitHostPort(*addr)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		originIP := net.ParseIP(u.Hostname())
		if ip == nil || !ip.IsLoopback() || originIP == nil || !originIP.IsLoopback() || u.Scheme != "http" {
			return errors.New("without TLS, listen address and origin must be loopback IPs")
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Bootstrap(ctx, os.Getenv("MVP_ADMIN_LOGIN"), os.Getenv("MVP_ADMIN_PASSWORD")); err != nil {
		return err
	}
	// Drop bootstrap secrets from the environment after use; they are never logged.
	_ = os.Unsetenv("MVP_ADMIN_PASSWORD")
	var runtime sessions.Runtime = sessions.Unavailable{}
	var signaling *server.Signaling
	if *relayOnly && (os.Getenv("MVP_TURN_URL") == "" || os.Getenv("MVP_TURN_SECRET") == "") {
		return errors.New("relay-only requires MVP_TURN_URL and MVP_TURN_SECRET")
	}
	if *runnerSocket != "" {
		client := runner.NewClient(*runnerSocket)
		start, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := client.Reset(start); err != nil {
			return err
		}
		old, err := db.Sessions(start, 0)
		if err != nil {
			return err
		}
		for _, v := range old {
			if err := db.State(start, v.ID, "closed"); err != nil {
				return err
			}
		}
		runtime = client
		signaling = &server.Signaling{Runner: client, TURNURL: os.Getenv("MVP_TURN_URL"), TURNSecret: os.Getenv("MVP_TURN_SECRET"), RelayOnly: *relayOnly}
	}
	service := sessions.New(db, runtime)
	defer func() {
		if signaling != nil {
			signaling.Close()
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		_ = service.StopUser(cleanup, 0)
	}()
	maintenanceDone := make(chan struct{})
	defer func() { stop(); <-maintenanceDone }()
	go func() {
		defer close(maintenanceDone)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check, cancel := context.WithTimeout(ctx, 45*time.Second)
				err := service.Reconcile(check)
				cancel()
				if err != nil && ctx.Err() == nil {
					slog.Error("session reconciliation failed", "error", err)
				}
			}
		}
	}()
	app := &server.Server{Store: db, Sessions: service, Origin: *origin, Secure: secure, Signal: signaling}
	httpServer := &http.Server{Addr: *addr, Handler: app.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	done := make(chan error, 1)
	go func() {
		slog.Info("gateway listening", "address", *addr, "origin", *origin, "runtime_available", service.Available())
		if secure {
			done <- httpServer.ListenAndServeTLS(*cert, *key)
		} else {
			done <- httpServer.ListenAndServe()
		}
	}()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err := httpServer.Shutdown(shutdown)
		if err != nil {
			_ = httpServer.Close()
		}
		return err
	}
}
