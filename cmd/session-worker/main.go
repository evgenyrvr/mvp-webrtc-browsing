package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"
	"mvp1/internal/input"
	"mvp1/internal/wire"
	"mvp1/internal/worker"
)

func main() {
	if err := run(); err != nil {
		slog.Error("session worker stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, expire := context.WithTimeout(ctx, wire.MaxSessionLifetime)
	defer expire()
	app := os.Getenv("MVP_APP")
	if app != "falkon" && app != "firefox" && app != "terminal" {
		return errors.New("unknown application")
	}
	port, err := strconv.Atoi(os.Getenv("MVP_UDP_PORT"))
	if err != nil {
		return err
	}
	prepare := exec.CommandContext(ctx, "mkdir", "-p", "/home/app/profile")
	prepare.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000, Groups: []uint32{1000}}}
	if err = prepare.Run(); err != nil {
		return err
	}
	_ = os.Setenv("DISPLAY", ":99")
	var children sync.WaitGroup
	defer func() { cancel(); children.Wait() }()
	launch := func(name string, args ...string) error {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/home/app", "USER=app", "LOGNAME=app", "SHELL=/bin/bash", "DISPLAY=:99", "LANG=C.UTF-8", "CUPS_SERVER=127.0.0.1:8631"}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000, Groups: []uint32{1000}}}
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			return err
		}
		children.Add(1)
		go func() {
			defer children.Done()
			err := cmd.Wait()
			if ctx.Err() == nil {
				slog.Warn("application process exited", "process", name, "error", err)
				if err != nil {
					for _, file := range []string{"/sys/fs/cgroup/pids.events", "/sys/fs/cgroup/memory.events"} {
						if data, err := os.ReadFile(file); err == nil {
							slog.Warn("application resource counters", "file", file, "counters", string(data))
						}
					}
				}
			}
			cancel()
		}()
		return nil
	}
	if err := launch("Xvfb", ":99", "-screen", "0", "1280x720x24", "-nolisten", "tcp", "-ac"); err != nil {
		return err
	}
	var keyboard *input.X11
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
waitDisplay:
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("Xvfb did not start")
		case <-tick.C:
			keyboard, err = input.Open(":99", 1280, 720)
			if err == nil {
				break waitDisplay
			}
		}
	}
	defer keyboard.Close()
	if err := launch("openbox", "--config-file", "/opt/mvp1/openbox.xml"); err != nil {
		return err
	}
	if app == "falkon" {
		if err := launch("falkon", "--no-remote", "--profile", "mvp1", "file:///opt/mvp1/welcome.html"); err != nil {
			return err
		}
	} else if app == "firefox" {
		if err := launch("firefox-esr", "--no-remote", "--profile", "/home/app/profile", "--width", "1280", "--height", "720", "file:///opt/mvp1/welcome.html"); err != nil {
			return err
		}
	} else {
		if err := launch("xterm", "-fa", "DejaVu Sans Mono", "-fs", "14", "-geometry", "120x40", "-e", "/bin/bash"); err != nil {
			return err
		}
	}
	readyCtx, readyCancel := context.WithTimeout(ctx, 15*time.Second)
	err = keyboard.WaitForWindow(readyCtx)
	readyCancel()
	if err != nil {
		return err
	}
	var iceServers []webrtc.ICEServer
	if raw := os.Getenv("MVP_ICE_SERVERS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &iceServers); err != nil {
			return err
		}
	}
	w, err := worker.New(worker.Config{Token: os.Getenv("MVP_WORKER_TOKEN"), Port: port, PublicIP: os.Getenv("MVP_PUBLIC_IP"), Input: keyboard, Capture: worker.FFmpegCapture(":99"), ICEServers: iceServers})
	if err != nil {
		return err
	}
	defer w.Close()
	printServer := &http.Server{Addr: "127.0.0.1:8631", Handler: w.PrintHandler(), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	defer printServer.Close()
	srv := &http.Server{Addr: ":8081", Handler: w.Handler(), ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 25 * time.Second, WriteTimeout: 25 * time.Second}
	done := make(chan error, 2)
	go func() { done <- printServer.ListenAndServe() }()
	go func() { done <- srv.ListenAndServe() }()
	idle := time.NewTicker(5 * time.Second)
	defer idle.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			_ = srv.Close()
			return nil
		case <-idle.C:
			if w.IdleFor() > 90*time.Second {
				_ = srv.Close()
				return nil
			}
		}
	}
}
