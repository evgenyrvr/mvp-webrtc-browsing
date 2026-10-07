package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/pion/webrtc/v4"
	"mvp1/internal/runner"
	"mvp1/internal/store"
	"mvp1/internal/turn"
	"mvp1/internal/wire"
)

type binding struct {
	generation string
	cancel     context.CancelFunc
	replace    func()
	timer      *time.Timer
}
type Signaling struct {
	Runner              *runner.Client
	TURNURL, TURNSecret string
	RelayOnly           bool
	mu                  sync.Mutex
	bindings            map[string]*binding
}

func (s *Server) signal(w http.ResponseWriter, r *http.Request, identity store.Identity) {
	if s.Signal == nil {
		fail(w, store.ErrNotFound)
		return
	}
	if r.Header.Get("Origin") != s.Origin {
		fail(w, store.ErrForbidden)
		return
	}
	session, err := s.Store.Session(r.Context(), r.PathValue("id"))
	if err != nil || session.UserID != identity.User.ID {
		fail(w, store.ErrNotFound)
		return
	}
	if session.State == "closed" || session.State == "failed" || session.State == "stopping" {
		fail(w, store.ErrNotFound)
		return
	}
	if _, err = s.Store.Application(r.Context(), identity.User.ID, session.AppID); err != nil {
		fail(w, err)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(256 * 1024)
	ctx, cancel := context.WithDeadline(r.Context(), time.Unix(identity.ExpiresAt, 0))
	defer cancel()
	first, done := context.WithTimeout(ctx, 5*time.Second)
	var hello struct {
		CSRF string `json:"csrf_token"`
	}
	err = wsjson.Read(first, conn, &hello)
	done()
	if err != nil || subtle.ConstantTimeCompare([]byte(hello.CSRF), []byte(identity.CSRF)) != 1 {
		_ = conn.Close(websocket.StatusPolicyViolation, "authentication required")
		return
	}
	generation, err := store.Token()
	if err != nil {
		return
	}
	active := &binding{generation: generation, cancel: cancel, replace: func() {
		// Send a terminal reason before cancellation: cancelling a websocket read
		// closes the transport and would hide the reason from the old client.
		_ = conn.Close(websocket.StatusCode(4001), "session opened elsewhere")
		cancel()
	}}
	b := s.Signal
	b.mu.Lock()
	if b.bindings == nil {
		b.bindings = map[string]*binding{}
	}
	old := b.bindings[session.ID]
	if old != nil {
		if old.timer != nil {
			old.timer.Stop()
		}
	}
	b.bindings[session.ID] = active
	b.mu.Unlock()
	if old != nil {
		slog.Info("signaling connection replaced", "session", session.ID)
		old.replace()
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = b.Runner.Disconnect(cleanup, session.ID, generation)
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.bindings[session.ID] != active {
			return
		}
		live, err := s.Store.LiveState(cleanup, session.ID, "reconnecting")
		if err != nil || !live {
			delete(b.bindings, session.ID)
			return
		}
		active.timer = time.AfterFunc(60*time.Second, func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if b.bindings[session.ID] != active {
				return
			}
			end, stop := context.WithTimeout(context.Background(), 15*time.Second)
			defer stop()
			_ = s.Sessions.Stop(end, session.ID, identity.User.ID, false)
			delete(b.bindings, session.ID)
		})
	}()
	send := func(v any) error {
		sendCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		return wsjson.Write(sendCtx, conn, v)
	}
	policy := "all"
	if b.RelayOnly {
		policy = "relay"
	}
	if send(map[string]any{"type": "config", "generation": generation, "rtc": wire.Config{ICEServers: turn.Credentials(b.TURNURL, b.TURNSecret, generation, time.Now()), ICETransportPolicy: policy}}) != nil {
		return
	}
	type incoming struct {
		Type string                    `json:"type"`
		SDP  webrtc.SessionDescription `json:"sdp"`
	}
	messages := make(chan incoming, 4)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			var v incoming
			if err := wsjson.Read(ctx, conn, &v); err != nil {
				slog.Info("signaling read ended", "session", session.ID, "error", err)
				return
			}
			select {
			case messages <- v:
			case <-ctx.Done():
				return
			}
		}
	}()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	cookie, _ := r.Cookie("mvp_session")
	authExpiry := time.NewTimer(time.Until(time.Unix(identity.ExpiresAt, 0)))
	defer authExpiry.Stop()
	negotiated := false
	lastPing := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-readDone:
			return
		case msg := <-messages:
			switch msg.Type {
			case "ping":
				lastPing = time.Now()
				if send(map[string]string{"type": "pong"}) != nil {
					return
				}
			case "offer":
				if negotiated {
					return
				}
				permission, err := s.Store.Application(ctx, identity.User.ID, session.AppID)
				if err != nil {
					slog.Warn("signaling permission check failed", "session", session.ID, "error", err)
					return
				}
				exchange, stop := context.WithTimeout(ctx, 20*time.Second)
				answer, err := b.Runner.Offer(exchange, session.ID, wire.Offer{AuthExpiresAt: identity.ExpiresAt, Generation: generation, SDP: msg.SDP, Control: permission.Control, Print: permission.Print, PolicyVersion: permission.PolicyVersion})
				stop()
				if err != nil {
					_ = send(map[string]string{"type": "error", "message": "Не удалось подключить приложение"})
					return
				}
				negotiated = true
				if send(map[string]any{"type": "answer", "sdp": answer}) != nil {
					return
				}
				b.mu.Lock()
				same := b.bindings[session.ID] == active
				live := false
				if same {
					live, err = s.Store.LiveState(ctx, session.ID, "active")
				}
				b.mu.Unlock()
				if !same || !live || err != nil {
					return
				}
			default:
				return
			}
		case <-authExpiry.C:
			_ = conn.Close(websocket.StatusCode(4003), "authentication expired")
			return
		case <-ticker.C:
			if time.Since(lastPing) > 30*time.Second {
				return
			}
			if _, err = s.Store.Identity(ctx, cookie.Value); err != nil {
				if errors.Is(err, store.ErrUnauthorized) {
					_ = conn.Close(websocket.StatusCode(4003), "authentication expired")
				}
				return
			}
			current, err := s.Store.Session(ctx, session.ID)
			if err != nil || current.State == "stopping" || current.State == "closed" || current.State == "failed" {
				if err == nil {
					_ = conn.Close(websocket.StatusCode(4002), "application ended")
				}
				return
			}
			permissions, err := s.Store.Application(ctx, identity.User.ID, session.AppID)
			if err != nil {
				return
			}
			if negotiated {
				lease, stop := context.WithTimeout(ctx, 3*time.Second)
				err = b.Runner.Lease(lease, session.ID, wire.Lease{Generation: generation, Control: permissions.Control, Print: permissions.Print, PolicyVersion: permissions.PolicyVersion})
				stop()
				if err != nil {
					slog.Warn("signaling lease failed", "session", session.ID, "error", err)
					return
				}
			}
		}
	}
}
func (b *Signaling) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, v := range b.bindings {
		v.cancel()
		if v.timer != nil {
			v.timer.Stop()
		}
	}
	b.bindings = map[string]*binding{}
}
