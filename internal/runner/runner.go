package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"mvp1/internal/turn"
	"mvp1/internal/wire"
)

var sessionID = regexp.MustCompile(`^[a-f0-9]{64}$`)
var namespacePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

type Config struct {
	BrowserCPUs     float64
	Backend         string
	ImagePullSecret string
	Image           string
	Namespace       string
	Network         string
	PublicIP        string
	UDPBase         int
	TURNURL         string
	TURNSecret      string
}
type worker struct {
	name, token, address string
	slot                 int
}
type Runner struct {
	kube      *kubernetes
	config    Config
	mu        sync.Mutex
	lifecycle sync.Mutex
	workers   map[string]worker
	http      *http.Client
}

func New(cfg Config) (*Runner, error) {
	if !namespacePattern.MatchString(cfg.Namespace) || cfg.Image == "" {
		return nil, errors.New("invalid runner configuration")
	}
	if cfg.BrowserCPUs == 0 {
		cfg.BrowserCPUs = 1.5
	}
	if cfg.BrowserCPUs < 0.1 || cfg.BrowserCPUs > 64 || math.IsNaN(cfg.BrowserCPUs) {
		return nil, errors.New("invalid browser CPU limit")
	}
	r := &Runner{config: cfg, workers: map[string]worker{}, http: &http.Client{Timeout: 20 * time.Second}}
	switch cfg.Backend {
	case "", "docker":
		if cfg.UDPBase < 1024 || cfg.UDPBase > 65531 || cfg.Network == "" {
			return nil, errors.New("invalid Docker configuration")
		}
	case "kubernetes":
		if cfg.TURNURL == "" || cfg.TURNSecret == "" {
			return nil, errors.New("Kubernetes backend requires TURN")
		}
		k, err := newKubernetes(cfg)
		if err != nil {
			return nil, err
		}
		r.kube = k
	default:
		return nil, errors.New("unknown runner backend")
	}
	return r, nil
}

func docker(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker operation failed: %w: %.800s", err, out)
	}
	return out, nil
}
func (r *Runner) Health(ctx context.Context) error {
	if r.kube != nil {
		_, err := r.kube.list(ctx)
		return err
	}
	if _, err := docker(ctx, "info", "--format", "{{.ServerVersion}}"); err != nil {
		return err
	}
	_, err := docker(ctx, "image", "inspect", r.config.Image, "--format", "{{.Id}}")
	return err
}
func (r *Runner) Start(ctx context.Context, v wire.Start) error {
	if !sessionID.MatchString(v.ID) || (v.Template != "falkon" && v.Template != "firefox" && v.Template != "terminal") {
		return errors.New("invalid session template")
	}
	r.lifecycle.Lock()
	locked := true
	defer func() {
		if locked {
			r.lifecycle.Unlock()
		}
	}()
	internalNetwork := false
	publicIP := r.config.PublicIP
	if r.kube == nil {
		networkInfo, err := docker(ctx, "network", "inspect", r.config.Network, "--format", "{{.Internal}}")
		if err != nil {
			return err
		}
		internalNetwork = strings.TrimSpace(string(networkInfo)) == "true"
		if internalNetwork {
			if ip := net.ParseIP(publicIP); ip == nil || !ip.IsLoopback() {
				return errors.New("internal network supports local clients only: set public-ip to loopback or use an approved routed network")
			}
			publicIP = "" // Host reaches the container bridge IP directly; Docker does not publish internal-network ports.
		}

	}
	r.mu.Lock()
	if _, ok := r.workers[v.ID]; ok {
		r.mu.Unlock()
		return errors.New("session already started")
	}
	used := [4]bool{}
	for _, w := range r.workers {
		used[w.slot] = true
	}
	slot := -1
	for i, b := range used {
		if !b {
			slot = i
			break
		}
	}
	if slot < 0 {
		r.mu.Unlock()
		return errors.New("runner capacity reached")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		r.mu.Unlock()
		return err
	}
	w := worker{name: r.config.Namespace + "-" + v.ID, token: hex.EncodeToString(secret), slot: slot}
	// Track before invoking Docker so a failed/ambiguous create can be cleaned up.
	r.workers[v.ID] = w
	r.mu.Unlock()
	if r.kube != nil {
		credentials, _ := json.Marshal(turn.Credentials(r.config.TURNURL, r.config.TURNSecret, v.ID, time.Now()))
		if err := r.kube.create(ctx, v.ID, w.token, v.Template, string(credentials)); err != nil {
			return err
		}
		r.lifecycle.Unlock()
		locked = false
		address, err := r.kube.waitRunning(ctx, v.ID)
		if err != nil {
			return err
		}
		w.address = address
	} else {
		port := strconv.Itoa(r.config.UDPBase + slot)
		pidsLimit := "256"
		if v.Template == "firefox" || v.Template == "falkon" {
			pidsLimit = "512"
		} // Firefox startup can exceed 256 processes + threads.
		cpus := "1.5"
		if v.Template == "falkon" || v.Template == "firefox" {
			cpus = strconv.FormatFloat(r.config.BrowserCPUs, 'f', -1, 64)
		}
		args := []string{"run", "--detach", "--rm", "--init", "--name", w.name, "--label", "mvp1.namespace=" + r.config.Namespace, "--network", r.config.Network, "--read-only", "--cap-drop=ALL", "--cap-add=SETUID", "--cap-add=SETGID", "--cap-add=KILL", "--security-opt=no-new-privileges", "--pids-limit=" + pidsLimit, "--memory=1g", "--cpus=" + cpus, "--shm-size=256m", "--tmpfs", "/tmp:rw,nosuid,nodev,size=256m", "--tmpfs", "/home/app:rw,nosuid,nodev,size=256m,uid=1000,gid=1000", "-p", "127.0.0.1::8081/tcp", "-p", port + ":" + port + "/udp", "-e", "MVP_WORKER_TOKEN", "-e", "MVP_ICE_SERVERS", "-e", "MVP_APP=" + v.Template, "-e", "MVP_UDP_PORT=" + port, "-e", "MVP_PUBLIC_IP=" + publicIP, r.config.Image}
		cmd := exec.CommandContext(ctx, "docker", args...)
		credentials, _ := json.Marshal(turn.Credentials(r.config.TURNURL, r.config.TURNSecret, v.ID, time.Now()))
		cmd.Env = append(os.Environ(), "MVP_WORKER_TOKEN="+w.token, "MVP_ICE_SERVERS="+string(credentials))
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("container start failed: %w: %.500s", err, out)
		}
		if internalNetwork {
			out, err := docker(ctx, "inspect", w.name, "--format", "{{json .NetworkSettings.Networks}}")
			if err != nil {
				return err
			}
			var networks map[string]struct{ IPAddress string }
			if err := json.Unmarshal(out, &networks); err != nil {
				return err
			}
			ip := net.ParseIP(networks[r.config.Network].IPAddress)
			if ip == nil {
				return errors.New("worker has no bridge address")
			}
			w.address = "http://" + net.JoinHostPort(ip.String(), "8081")
		} else {
			out, err := docker(ctx, "port", w.name, "8081/tcp")
			if err != nil {
				return err
			}
			w.address = "http://" + strings.TrimSpace(string(out))
			if !strings.HasPrefix(w.address, "http://127.0.0.1:") {
				return errors.New("worker port must be loopback")
			}
		}

	}
	r.mu.Lock()
	if current, ok := r.workers[v.ID]; !ok || current.token != w.token {
		r.mu.Unlock()
		return errors.New("session was stopped during startup")
	}
	r.workers[v.ID] = w
	r.mu.Unlock()
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		r.mu.Lock()
		current, exists := r.workers[v.ID]
		r.mu.Unlock()
		if !exists || current.token != w.token {
			return errors.New("session was stopped during readiness")
		}
		health, cancel := context.WithTimeout(ctx, time.Second)
		err := r.workerCall(health, w, "/healthz", nil, nil)
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (r *Runner) Stop(ctx context.Context, id string) error {
	if !sessionID.MatchString(id) {
		return errors.New("invalid session id")
	}
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	r.mu.Lock()
	w, ok := r.workers[id]
	r.mu.Unlock()
	if !ok {
		w.name = r.config.Namespace + "-" + id
	}
	if r.kube != nil {
		if err := r.kube.stop(ctx, id); err != nil {
			return err
		}
	} else {
		// Inspect via label/name list distinguishes absent containers from daemon errors.
		out, err := docker(ctx, "ps", "-aq", "--filter", "label=mvp1.namespace="+r.config.Namespace, "--filter", "name=^/"+w.name+"$")
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(out)) != "" {
			if _, err := docker(ctx, "rm", "-f", w.name); err != nil {
				return err
			}
		}

	}
	r.mu.Lock()
	delete(r.workers, id)
	r.mu.Unlock()
	return nil
}
func (r *Runner) Reset(ctx context.Context) error {
	r.lifecycle.Lock()
	defer r.lifecycle.Unlock()
	if r.kube != nil {
		if err := r.kube.reset(ctx); err != nil {
			return err
		}
	} else {
		out, err := docker(ctx, "ps", "-aq", "--filter", "label=mvp1.namespace="+r.config.Namespace)
		if err != nil {
			return err
		}
		ids := strings.Fields(string(out))
		if len(ids) > 0 {
			if _, err := docker(ctx, append([]string{"rm", "-f"}, ids...)...); err != nil {
				return err
			}
		}

	}
	r.mu.Lock()
	r.workers = map[string]worker{}
	r.mu.Unlock()
	return nil
}
func (r *Runner) workerCall(ctx context.Context, w worker, path string, in, out any) error {
	var body io.Reader
	method := "GET"
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
		method = "POST"
	}
	req, err := http.NewRequestWithContext(ctx, method, w.address+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+w.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("worker HTTP %d", resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
	}
	return nil
}
func (r *Runner) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions/{id}/alive", func(w http.ResponseWriter, q *http.Request) {
		id := q.PathValue("id")
		if !sessionID.MatchString(id) {
			http.Error(w, "invalid id", 400)
			return
		}
		var alive bool
		var err error
		if r.kube != nil {
			alive, err = r.kube.alive(q.Context(), id)
		} else {
			var out []byte
			out, err = docker(q.Context(), "ps", "-q", "--filter", "label=mvp1.namespace="+r.config.Namespace, "--filter", "name=^/"+r.config.Namespace+"-"+id+"$")
			alive = strings.TrimSpace(string(out)) != ""
		}
		if err != nil {
			http.Error(w, "runtime unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"alive": alive})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, q *http.Request) {
		ctx, cancel := context.WithTimeout(q.Context(), 2*time.Second)
		defer cancel()
		if r.Health(ctx) != nil {
			http.Error(w, "runtime unavailable", 503)
			return
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, q *http.Request) {
		var v wire.Start
		if json.NewDecoder(http.MaxBytesReader(w, q.Body, 4096)).Decode(&v) != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		if err := r.Start(q.Context(), v); err != nil {
			http.Error(w, "start failed", 503)
			return
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("DELETE /sessions/{id}", func(w http.ResponseWriter, q *http.Request) {
		if err := r.Stop(q.Context(), q.PathValue("id")); err != nil {
			http.Error(w, "stop failed", 503)
			return
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("POST /reset", func(w http.ResponseWriter, q *http.Request) {
		if err := r.Reset(q.Context()); err != nil {
			http.Error(w, "reset failed", 503)
			return
		}
		w.WriteHeader(200)
	})
	for _, action := range []string{"offer", "lease", "disconnect", "permissions"} {
		mux.HandleFunc("POST /sessions/{id}/"+action, func(w http.ResponseWriter, q *http.Request) {
			r.mu.Lock()
			worker, ok := r.workers[q.PathValue("id")]
			r.mu.Unlock()
			if !ok {
				http.Error(w, "not found", 404)
				return
			}
			var body json.RawMessage
			if json.NewDecoder(http.MaxBytesReader(w, q.Body, 256*1024)).Decode(&body) != nil {
				http.Error(w, "invalid request", 400)
				return
			}
			var result json.RawMessage
			if err := r.workerCall(q.Context(), worker, "/"+action, body, &result); err != nil {
				slog.Warn("worker request failed", "action", action, "error", err)
				http.Error(w, "worker unavailable", 503)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(result)
		})
	}
	return mux
}
