// Package appbridge transports encoded video and structured input between the
// isolated application and its WebRTC worker. It never accepts shell commands.
package appbridge

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"mvp1/internal/wire"
)

type Input interface {
	Apply(wire.Input) error
	Reset() error
}

func Command(ctx context.Context) *exec.Cmd {
	return exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-loglevel", "error", "-f", "x11grab", "-draw_mouse", "1", "-framerate", "20", "-video_size", "1280x720", "-i", ":99", "-an", "-c:v", "libvpx", "-deadline", "realtime", "-cpu-used", "8", "-threads", "2", "-b:v", "1800k", "-maxrate", "2400k", "-bufsize", "1200k", "-g", "20", "-lag-in-frames", "0", "-f", "ivf", "pipe:1")
}
func Handler(token string, input Input) http.Handler {
	mux := http.NewServeMux()
	video := make(chan struct{}, 1)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("POST /input", func(w http.ResponseWriter, r *http.Request) {
		var v wire.Input
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048)).Decode(&v) != nil {
			http.Error(w, "invalid input", 400)
			return
		}
		if input.Apply(v) != nil {
			http.Error(w, "input rejected", 400)
			return
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("POST /reset", func(w http.ResponseWriter, r *http.Request) {
		if input.Reset() != nil {
			http.Error(w, "reset failed", 500)
			return
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("GET /video", func(w http.ResponseWriter, r *http.Request) {
		select {
		case video <- struct{}{}:
			defer func() { <-video }()
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Second):
			http.Error(w, "capture busy", 409)
			return
		}
		cmd := Command(r.Context())
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			http.Error(w, "capture failed", 500)
			return
		}
		if cmd.Start() != nil {
			http.Error(w, "capture failed", 500)
			return
		}
		defer func() { stdout.Close(); cmd.Process.Kill(); cmd.Wait() }()
		w.Header().Set("Content-Type", "video/x-ivf")
		buf := make([]byte, 32<<10)
		for {
			n, err := stdout.Read(buf)
			if n > 0 {
				if _, e := w.Write(buf[:n]); e != nil {
					return
				}
				if http.NewResponseController(w).Flush() != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

type Client struct {
	mu             sync.RWMutex
	address, token string
	http           *http.Client
}

func NewClient() *Client {
	return &Client{http: &http.Client{Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: 5 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *Client) Attach(address, token string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.address == address && c.token == token {
		return nil
	}
	if c.address != "" {
		return errors.New("already attached")
	}
	c.address = address
	c.token = token
	return nil
}
func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	c.mu.RLock()
	address, token := c.address, c.token
	c.mu.RUnlock()
	if address == "" {
		return nil, errors.New("app not attached")
	}
	req, err := http.NewRequestWithContext(ctx, method, address+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		resp.Body.Close()
		return nil, errors.New("app agent unavailable")
	}
	return resp, nil
}
func (c *Client) call(path string, v any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	body, _ := json.Marshal(v)
	resp, err := c.request(ctx, "POST", path, bytes.NewReader(body))
	if err == nil {
		resp.Body.Close()
	}
	return err
}
func (c *Client) Apply(v wire.Input) error { return c.call("/input", v) }
func (c *Client) Reset() error             { return c.call("/reset", nil) }
func (c *Client) Health(ctx context.Context) error {
	resp, err := c.request(ctx, "GET", "/healthz", nil)
	if err == nil {
		resp.Body.Close()
	}
	return err
}
func (c *Client) Capture(ctx context.Context, track *webrtc.TrackLocalStaticSample) error {
	resp, err := c.request(ctx, "GET", "/video", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	header := make([]byte, 32)
	if _, err = io.ReadFull(resp.Body, header); err != nil {
		return err
	}
	if string(header[:4]) != "DKIF" || string(header[8:12]) != "VP80" || binary.LittleEndian.Uint16(header[6:8]) != 32 {
		return errors.New("invalid IVF header")
	}
	var frameHeader [12]byte
	for {
		if _, err = io.ReadFull(resp.Body, frameHeader[:]); err != nil {
			return err
		}
		size := binary.LittleEndian.Uint32(frameHeader[:4])
		if size == 0 || size > 4<<20 {
			return errors.New("invalid IVF frame size")
		}
		frame := make([]byte, size)
		if _, err = io.ReadFull(resp.Body, frame); err != nil {
			return err
		}
		if err = track.WriteSample(media.Sample{Data: frame, Duration: 50 * time.Millisecond}); err != nil {
			return err
		}
	}
}
