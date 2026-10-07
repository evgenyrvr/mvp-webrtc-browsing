package worker

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"golang.org/x/time/rate"
	"mvp1/internal/printing"
	"mvp1/internal/wire"
)

type Input interface {
	Apply(wire.Input) error
	Reset() error
}
type Capture func(context.Context, *webrtc.TrackLocalStaticSample) error
type Config struct {
	Token      string
	Port       int
	PublicIP   string
	ICEServers []webrtc.ICEServer
	Input      Input
	Capture    Capture
}
type peer struct {
	pc             *webrtc.PeerConnection
	cancel         context.CancelFunc
	generation     string
	lease          time.Time
	authExpires    time.Time
	control, print bool
	printChannel   bool
	limiter        *rate.Limiter
	pointerLimiter *rate.Limiter
}
type Worker struct {
	printQueue printing.Queue
	cfg        Config
	api        *webrtc.API
	mu         sync.Mutex
	negotiate  sync.Mutex
	peer       *peer
	policy     wire.Lease
	udp        io.Closer
	ctx        context.Context
	cancel     context.CancelFunc
	lastLease  time.Time
	done       chan struct{}
}

func New(cfg Config) (*Worker, error) {
	if cfg.Token == "" || cfg.Input == nil || cfg.Capture == nil {
		return nil, errors.New("invalid worker config")
	}
	var settings webrtc.SettingEngine
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	var udp io.Closer
	if cfg.Port > 0 {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: cfg.Port})
		if err != nil {
			return nil, err
		}
		mux := webrtc.NewICEUDPMux(nil, conn)
		udp = mux
		settings.SetICEUDPMux(mux)
	}
	if cfg.PublicIP != "" {
		settings.SetNAT1To1IPs([]string{cfg.PublicIP}, webrtc.ICECandidateTypeHost)
	}
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		if udp != nil {
			udp.Close()
		}
		return nil, err
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, ir); err != nil {
		if udp != nil {
			udp.Close()
		}
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	w := &Worker{cfg: cfg, api: webrtc.NewAPI(webrtc.WithSettingEngine(settings), webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(ir)), udp: udp, ctx: ctx, cancel: cancel, lastLease: time.Now(), done: make(chan struct{})}
	go w.watchdog()
	return w, nil
}
func (w *Worker) watchdog() {
	defer close(w.done)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-w.ctx.Done():
			return
		case now := <-t.C:
			w.mu.Lock()
			p := w.peer
			expired := p != nil && now.After(p.lease)
			if expired {
				w.peer = nil
				_ = w.cfg.Input.Reset()
			}
			w.mu.Unlock()
			if expired {
				p.cancel()
				_ = p.pc.Close()
			}
		}
	}
}
func (w *Worker) Close() {
	w.cancel()
	w.printQueue.Clear()
	<-w.done
	w.negotiate.Lock()
	defer w.negotiate.Unlock()
	w.mu.Lock()
	p := w.peer
	w.peer = nil
	_ = w.cfg.Input.Reset()
	w.mu.Unlock()
	if p != nil {
		p.cancel()
		_ = p.pc.Close()
	}
	if w.udp != nil {
		_ = w.udp.Close()
	}
}
func (w *Worker) IdleFor() time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return time.Since(w.lastLease)
}
func (w *Worker) Offer(ctx context.Context, offer wire.Offer) (webrtc.SessionDescription, error) {
	w.negotiate.Lock()
	defer w.negotiate.Unlock()
	if err := ctx.Err(); err != nil {
		return webrtc.SessionDescription{}, err
	}
	if err := w.ctx.Err(); err != nil {
		return webrtc.SessionDescription{}, err
	}
	if len(offer.Generation) != 64 || offer.SDP.Type != webrtc.SDPTypeOffer {
		return webrtc.SessionDescription{}, errors.New("invalid offer")
	}
	pc, err := w.api.NewPeerConnection(webrtc.Configuration{ICEServers: w.cfg.ICEServers})
	if err != nil {
		return webrtc.SessionDescription{}, err
	}
	captureCtx, cancel := context.WithCancel(w.ctx)
	var authExpires time.Time
	if offer.AuthExpiresAt != 0 {
		authExpires = time.Unix(offer.AuthExpiresAt, 0)
		cancel()
		if !time.Now().Before(authExpires) {
			_ = pc.Close()
			return webrtc.SessionDescription{}, errors.New("authentication expired")
		}
		captureCtx, cancel = context.WithDeadline(w.ctx, authExpires)
	}
	p := &peer{authExpires: authExpires, pc: pc, cancel: cancel, generation: offer.Generation, lease: time.Now().Add(30 * time.Second), control: offer.Control, print: offer.Print, limiter: rate.NewLimiter(250, 500), pointerLimiter: rate.NewLimiter(90, 180)}
	if !p.authExpires.IsZero() && p.authExpires.Before(p.lease) {
		p.lease = p.authExpires
	}
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, "screen", "application")
	if err != nil {
		cancel()
		pc.Close()
		return webrtc.SessionDescription{}, err
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		cancel()
		pc.Close()
		return webrtc.SessionDescription{}, err
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := sender.Read(buf); err != nil {
				return
			}
		}
	}()
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() == "print" {
			w.printChannel(captureCtx, p, dc)
			return
		}
		if dc.Label() != "control" && dc.Label() != "pointer" {
			_ = dc.Close()
			return
		}
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if !msg.IsString || len(msg.Data) > 2048 {
				return
			}
			var event wire.Input
			if json.Unmarshal(msg.Data, &event) != nil || event.Version != 1 {
				return
			}
			if dc.Label() == "pointer" && event.Type != "pointer" {
				return
			}
			w.apply(p, dc.Label(), event)
		})
	})
	w.mu.Lock()
	if err := ctx.Err(); err != nil {
		w.mu.Unlock()
		cancel()
		_ = pc.Close()
		return webrtc.SessionDescription{}, err
	}
	old := w.peer
	w.peer = p
	w.updatePolicy(wire.Lease{PolicyVersion: offer.PolicyVersion, Control: offer.Control, Print: offer.Print})
	p.control = w.policy.Control
	p.print = w.policy.Print
	_ = w.cfg.Input.Reset()
	w.lastLease = time.Now()
	w.mu.Unlock()
	if old != nil {
		old.cancel()
		_ = old.pc.Close()
	}
	failed := true
	defer func() {
		if failed {
			w.disconnect(p.generation)
		}
	}()
	if err = pc.SetRemoteDescription(offer.SDP); err != nil {
		return webrtc.SessionDescription{}, err
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return webrtc.SessionDescription{}, err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(answer); err != nil {
		return webrtc.SessionDescription{}, err
	}
	select {
	case <-ctx.Done():
		return webrtc.SessionDescription{}, ctx.Err()
	case <-gathered:
	}
	if err := ctx.Err(); err != nil {
		return webrtc.SessionDescription{}, err
	}
	go func() {
		if err := w.cfg.Capture(captureCtx, track); err != nil && captureCtx.Err() == nil {
			slog.Error("video capture stopped", "error", err)
			w.disconnect(p.generation)
		}
	}()
	context.AfterFunc(captureCtx, func() { w.disconnect(p.generation) })
	failed = false
	return *pc.LocalDescription(), nil
}
func (w *Worker) disconnect(generation string) {
	w.mu.Lock()
	p := w.peer
	if p == nil || p.generation != generation {
		w.mu.Unlock()
		return
	}
	w.peer = nil
	_ = w.cfg.Input.Reset()
	w.mu.Unlock()
	p.cancel()
	_ = p.pc.Close()
}

func (w *Worker) apply(p *peer, channel string, event wire.Input) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.peer != p || !time.Now().Before(p.lease) || !p.control {
		return false
	}
	limiter := p.limiter
	if channel == "pointer" {
		limiter = p.pointerLimiter
	}
	if !limiter.Allow() {
		if channel == "control" {
			_ = w.cfg.Input.Reset()
		}
		return false
	}
	return w.cfg.Input.Apply(event) == nil
}
func (w *Worker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { reply(w, map[string]bool{"ready": true}) })
	mux.HandleFunc("POST /offer", func(out http.ResponseWriter, r *http.Request) {
		var v wire.Offer
		if json.NewDecoder(http.MaxBytesReader(out, r.Body, 256*1024)).Decode(&v) != nil {
			http.Error(out, "bad offer", 400)
			return
		}
		answer, err := w.Offer(r.Context(), v)
		if err != nil {
			http.Error(out, "negotiation failed", 500)
			return
		}
		reply(out, answer)
	})
	mux.HandleFunc("POST /lease", func(out http.ResponseWriter, r *http.Request) {
		var v wire.Lease
		if json.NewDecoder(http.MaxBytesReader(out, r.Body, 4096)).Decode(&v) != nil {
			http.Error(out, "bad lease", 400)
			return
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		p := w.peer
		if p == nil || p.generation != v.Generation {
			http.Error(out, "stale generation", 409)
			return
		}
		w.updatePolicy(v)
		p.lease = time.Now().Add(8 * time.Second)
		if !p.authExpires.IsZero() && p.authExpires.Before(p.lease) {
			p.lease = p.authExpires
		}
		w.lastLease = time.Now()
		reply(out, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /permissions", func(out http.ResponseWriter, r *http.Request) {
		var v wire.Lease
		if json.NewDecoder(http.MaxBytesReader(out, r.Body, 4096)).Decode(&v) != nil {
			http.Error(out, "bad permissions", 400)
			return
		}
		w.mu.Lock()
		w.updatePolicy(v)
		w.mu.Unlock()
		reply(out, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /disconnect", func(out http.ResponseWriter, r *http.Request) {
		var v wire.Lease
		if json.NewDecoder(http.MaxBytesReader(out, r.Body, 4096)).Decode(&v) != nil {
			http.Error(out, "bad generation", 400)
			return
		}
		w.disconnect(v.Generation)
		reply(out, map[string]bool{"ok": true})
	})
	return http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+w.cfg.Token)) != 1 {
			http.Error(out, "unauthorized", 401)
			return
		}
		mux.ServeHTTP(out, r)
	})
}
func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// Called under mu. A delayed heartbeat must not undo a newer revocation.
func (w *Worker) updatePolicy(v wire.Lease) {
	if v.PolicyVersion < w.policy.PolicyVersion {
		return
	}
	if !v.Print {
		w.printQueue.Clear()
	}
	w.policy = v
	if p := w.peer; p != nil {
		if p.control && !v.Control {
			_ = w.cfg.Input.Reset()
		}
		p.control = v.Control
		p.print = v.Print
	}
}

func FFmpegCapture(display string) Capture {
	return func(ctx context.Context, track *webrtc.TrackLocalStaticSample) error {
		cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-loglevel", "error", "-f", "x11grab", "-draw_mouse", "1", "-framerate", "20", "-video_size", "1280x720", "-i", display, "-an", "-c:v", "libvpx", "-deadline", "realtime", "-cpu-used", "8", "-threads", "2", "-b:v", "1800k", "-maxrate", "2400k", "-bufsize", "1200k", "-g", "20", "-lag-in-frames", "0", "-f", "ivf", "pipe:1")
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		if err = cmd.Start(); err != nil {
			return err
		}
		defer func() {
			_ = pipe.Close()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = cmd.Wait()
		}()
		header := make([]byte, 32)
		if _, err = io.ReadFull(pipe, header); err != nil {
			return err
		}
		if string(header[:4]) != "DKIF" || string(header[8:12]) != "VP80" {
			return errors.New("invalid IVF stream")
		}
		for {
			frameHeader := make([]byte, 12)
			if _, err = io.ReadFull(pipe, frameHeader); err != nil {
				return err
			}
			size := binary.LittleEndian.Uint32(frameHeader[:4])
			if size > 4<<20 {
				return errors.New("video frame exceeds limit")
			}
			frame := make([]byte, size)
			if _, err = io.ReadFull(pipe, frame); err != nil {
				return err
			}
			if err = track.WriteSample(media.Sample{Data: frame, Duration: 50 * time.Millisecond}); err != nil {
				return err
			}
		}
	}
}
