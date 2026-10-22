package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"mvp1/internal/sessions"
	"mvp1/internal/store"
	"mvp1/web"
)

type Server struct {
	Store     *store.Store
	Sessions  *sessions.Service
	Origin    string
	Secure    bool
	Signal    *Signaling
	mutations sync.Mutex
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { write(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Store.Ping(r.Context()); err != nil {
			write(w, 503, map[string]string{"status": "unavailable"})
			return
		}
		available := s.Sessions.Available()
		if s.Signal != nil && !available {
			write(w, 503, map[string]any{"status": "unavailable", "runtime_available": false})
			return
		}
		write(w, 200, map[string]any{"status": "ok", "runtime_available": available})
	})
	mux.HandleFunc("POST /api/v1/login", s.login)
	mux.HandleFunc("GET /api/v1/me", s.auth(false, func(w http.ResponseWriter, r *http.Request, v store.Identity) {
		write(w, 200, map[string]any{"user": v.User, "csrf_token": v.CSRF, "expires_at": v.ExpiresAt, "server_time": v.ServerTime, "runtime_available": s.Sessions.Available(), "limits": map[string]int{"users": 2, "per_user": 2, "total": 4}})
	}))
	mux.HandleFunc("POST /api/v1/logout", s.auth(false, s.logout))
	mux.HandleFunc("GET /api/v1/applications", s.auth(false, func(w http.ResponseWriter, r *http.Request, v store.Identity) {
		a, err := s.Store.Applications(r.Context(), v.User.ID)
		respond(w, a, err)
	}))
	mux.HandleFunc("GET /api/v1/sessions", s.auth(false, func(w http.ResponseWriter, r *http.Request, v store.Identity) {
		a, err := s.Store.Sessions(r.Context(), v.User.ID)
		respond(w, a, err)
	}))
	mux.HandleFunc("POST /api/v1/sessions", s.auth(false, s.launch))
	mux.HandleFunc("GET /api/v1/sessions/{id}/signal", s.auth(false, s.signal))
	mux.HandleFunc("GET /api/v1/sessions/{id}", s.auth(false, func(w http.ResponseWriter, r *http.Request, v store.Identity) {
		a, err := s.Store.Session(r.Context(), r.PathValue("id"))
		if err == nil && a.UserID != v.User.ID {
			err = store.ErrNotFound
		}
		respond(w, a, err)
	}))
	mux.HandleFunc("DELETE /api/v1/sessions/{id}", s.auth(false, func(w http.ResponseWriter, r *http.Request, v store.Identity) {
		respond(w, map[string]bool{"ok": true}, s.Sessions.Stop(r.Context(), r.PathValue("id"), v.User.ID, false))
	}))
	mux.HandleFunc("GET /api/v1/admin", s.auth(true, s.adminData))
	mux.HandleFunc("PUT /api/v1/admin/users", s.auth(true, s.saveUser))
	mux.HandleFunc("PUT /api/v1/admin/roles", s.auth(true, s.saveRole))
	mux.HandleFunc("PUT /api/v1/admin/grants", s.auth(true, s.saveGrant))
	mux.HandleFunc("PUT /api/v1/admin/publications/{id}", s.auth(true, s.savePublication))
	mux.HandleFunc("PUT /api/v1/admin/applications/{id}", s.auth(true, s.saveApplication))
	mux.HandleFunc("DELETE /api/v1/admin/sessions/{id}", s.auth(true, func(w http.ResponseWriter, r *http.Request, v store.Identity) {
		respond(w, map[string]bool{"ok": true}, s.Sessions.Stop(r.Context(), r.PathValue("id"), v.User.ID, true))
	}))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { fail(w, store.ErrNotFound) })
	mux.Handle("/", web.Handler())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; media-src 'self' blob:; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" {
			if r.Header.Get("Origin") != s.Origin {
				fail(w, store.ErrForbidden)
				return
			}
			// Read network input before acquiring the mutation lock: a slow client
			// must not block all other users' writes. Auth is checked after locking.
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16*1024))
			if err != nil {
				fail(w, store.ErrInvalid)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			s.mutations.Lock()
			defer s.mutations.Unlock()
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) auth(admin bool, next func(http.ResponseWriter, *http.Request, store.Identity)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("mvp_session")
		if err != nil {
			fail(w, store.ErrUnauthorized)
			return
		}
		v, err := s.Store.Identity(r.Context(), c.Value)
		if err != nil {
			fail(w, err)
			return
		}
		if admin && !v.User.Admin {
			fail(w, store.ErrForbidden)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && subtle.ConstantTimeCompare([]byte(v.CSRF), []byte(r.Header.Get("X-CSRF-Token"))) != 1 {
			fail(w, store.ErrForbidden)
			return
		}
		next(w, r, v)
	}
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Login    string `json:"login"`
		Password string `json:"password"`
	}
	if !decode(w, r, &b) {
		return
	}
	if len(b.Login) > 80 || len(b.Password) > 1024 {
		fail(w, store.ErrInvalid)
		return
	}
	token, err := s.Store.Login(r.Context(), strings.TrimSpace(b.Login), b.Password)
	if err != nil {
		fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "mvp_session", Value: token, Path: "/", HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteStrictMode, MaxAge: int(store.AuthSessionLifetime.Seconds())})
	v, err := s.Store.Identity(r.Context(), token)
	respond(w, v, err)
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request, v store.Identity) {
	err := s.Store.LogoutUser(r.Context(), v.User.ID)
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 45*time.Second)
	defer cancel()
	stopErr := s.Sessions.StopUser(cleanup, v.User.ID)
	http.SetCookie(w, &http.Cookie{Name: "mvp_session", Value: "", Path: "/", HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteStrictMode, MaxAge: -1})
	respond(w, map[string]bool{"ok": true}, errors.Join(err, stopErr))
}
func (s *Server) launch(w http.ResponseWriter, r *http.Request, v store.Identity) {
	var b struct {
		AppID string `json:"application_id"`
	}
	if !decode(w, r, &b) {
		return
	}
	result, err := s.Sessions.Launch(r.Context(), v.User.ID, b.AppID)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, 201, result)
}
func (s *Server) adminData(w http.ResponseWriter, r *http.Request, v store.Identity) {
	users, err := s.Store.Users(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	roles, err := s.Store.Roles(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	grants, err := s.Store.Grants(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	apps, err := s.Store.AllApplications(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	list, err := s.Store.Sessions(r.Context(), 0)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, 200, map[string]any{"users": users, "roles": roles, "grants": grants, "applications": apps, "sessions": list})
}
func (s *Server) saveUser(w http.ResponseWriter, r *http.Request, v store.Identity) {
	var b store.UserInput
	if !decode(w, r, &b) {
		return
	}
	id, err := s.Store.SaveUser(r.Context(), b)
	if err == nil {
		err = s.reconcile(r.Context())
	}
	respond(w, map[string]int64{"id": id}, err)
}
func (s *Server) saveRole(w http.ResponseWriter, r *http.Request, v store.Identity) {
	var b store.Role
	if !decode(w, r, &b) {
		return
	}
	err := s.Store.SaveRole(r.Context(), b)
	if err == nil {
		err = s.reconcile(r.Context())
	}
	respond(w, map[string]bool{"ok": true}, err)
}
func (s *Server) saveGrant(w http.ResponseWriter, r *http.Request, v store.Identity) {
	var b store.Grant
	if !decode(w, r, &b) {
		return
	}
	err := s.Store.SaveGrant(r.Context(), b)
	if err == nil {
		err = s.reconcile(r.Context())
	}
	respond(w, map[string]bool{"ok": true}, err)
}
func (s *Server) saveApplication(w http.ResponseWriter, r *http.Request, v store.Identity) {
	var b struct {
		Enabled bool `json:"enabled"`
	}
	if !decode(w, r, &b) {
		return
	}
	err := s.Store.SetApplication(r.Context(), r.PathValue("id"), b.Enabled)
	if err == nil {
		err = s.reconcile(r.Context())
	}
	respond(w, map[string]bool{"ok": true}, err)
}

func (s *Server) reconcile(parent context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 45*time.Second)
	defer cancel()
	return s.Sessions.Reconcile(ctx)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		write(w, 415, map[string]any{"error": map[string]string{"code": "content_type", "message": "Ожидается application/json"}})
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		fail(w, store.ErrInvalid)
		return false
	}
	if err := d.Decode(new(any)); err != io.EOF {
		fail(w, store.ErrInvalid)
		return false
	}
	return true
}
func respond(w http.ResponseWriter, v any, err error) {
	if err != nil {
		fail(w, err)
		return
	}
	write(w, 200, v)
}
func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, err error) {
	status, code, message := 500, "internal", "Внутренняя ошибка сервера"
	switch {
	case errors.Is(err, store.ErrUnauthorized):
		status, code, message = 401, "unauthorized", "Неверный логин или пароль, либо сеанс входа завершён"
	case errors.Is(err, store.ErrForbidden):
		status, code, message = 403, "forbidden", "Недостаточно прав для этого действия"
	case errors.Is(err, store.ErrNotFound):
		status, code, message = 404, "not_found", "Объект не найден"
	case errors.Is(err, store.ErrLimit):
		status, code, message = 409, "session_limit", "Достигнут лимит: два пользователя, два приложения у каждого"
	case errors.Is(err, store.ErrLastAdmin):
		status, code, message = 409, "last_admin", "Должен остаться хотя бы один активный администратор"
	case errors.Is(err, store.ErrInvalid):
		status, code, message = 400, "invalid_input", "Проверьте поля, уникальность имени и назначенные роли"
	case errors.Is(err, sessions.ErrUnavailable):
		status, code, message = 503, "runtime_unavailable", "Среда запуска приложений ещё не подключена"
	}
	if status == 500 {
		slog.Error("request failed", "error", err)
	}
	write(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func (s *Server) savePublication(w http.ResponseWriter, r *http.Request, v store.Identity) {
	var a store.Application
	if !decode(w, r, &a) {
		return
	}
	a.ID = r.PathValue("id")
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 45*time.Second)
	defer cancel()
	err := s.Sessions.SavePublication(ctx, a)
	respond(w, map[string]bool{"ok": true}, err)
}
