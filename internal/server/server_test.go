package server

import (
	"bytes"
	"context"
	"encoding/json"
	"mvp1/internal/sessions"
	"mvp1/internal/store"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Bootstrap(context.Background(), "admin", "test-only"); err != nil {
		t.Fatal(err)
	}
	s := &Server{Store: db, Sessions: sessions.New(db, sessions.Unavailable{}), Origin: "http://127.0.0.1:8080"}
	return s.Handler(), db
}
func req(h http.Handler, method, path, body string, cookie *http.Cookie, csrf, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	r.Header.Set("Content-Type", "application/json")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func login(t *testing.T, h http.Handler, user, password string) (*http.Cookie, store.Identity) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"login": user, "password": password})
	w := req(h, "POST", "/api/v1/login", string(b), nil, "", "http://127.0.0.1:8080")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var v store.Identity
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return w.Result().Cookies()[0], v
}
func TestAuthCSRFAndUnavailableLaunch(t *testing.T) {
	h, _ := fixture(t)
	if w := req(h, "GET", "/api/v1/applications", "", nil, "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	cookie, v := login(t, h, "admin", "test-only")
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge != 3600 {
		t.Fatal(cookie)
	}
	for _, attempt := range []struct{ csrf, origin string }{{"", "http://127.0.0.1:8080"}, {v.CSRF, "https://evil.invalid"}, {v.CSRF, ""}} {
		w := req(h, "POST", "/api/v1/sessions", `{"application_id":"firefox"}`, cookie, attempt.csrf, attempt.origin)
		if w.Code != 403 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w := req(h, "POST", "/api/v1/sessions", `{"application_id":"firefox"}`, cookie, v.CSRF, "http://127.0.0.1:8080")
	if w.Code != 503 || !strings.Contains(w.Body.String(), "runtime_unavailable") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = req(h, "POST", "/api/v1/logout", "", cookie, v.CSRF, "http://127.0.0.1:8080")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w = req(h, "GET", "/api/v1/me", "", cookie, "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
}
func TestAdminWorkflowAndNoPasswordDisclosure(t *testing.T) {
	h, db := fixture(t)
	cookie, admin := login(t, h, "admin", "test-only")
	w := req(h, "PUT", "/api/v1/admin/users", `{"login":"alice","password":"alice-private","enabled":true,"role_ids":[2]}`, cookie, admin.CSRF, "http://127.0.0.1:8080")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	userCookie, user := login(t, h, "alice", "alice-private")
	w = req(h, "GET", "/api/v1/admin", "", userCookie, "", "")
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	w = req(h, "POST", "/api/v1/sessions", `{"application_id":"terminal"}`, userCookie, user.CSRF, "http://127.0.0.1:8080")
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	w = req(h, "PUT", "/api/v1/admin/grants", `{"role_id":2,"application_id":"firefox","can_launch":true,"can_control":true,"can_print":true}`, cookie, admin.CSRF, "http://127.0.0.1:8080")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = req(h, "GET", "/api/v1/applications", "", userCookie, "", "")
	var apps []store.Application
	_ = json.Unmarshal(w.Body.Bytes(), &apps)
	if len(apps) != 1 || apps[0].ID != "firefox" {
		t.Fatal(w.Body.String())
	}
	w = req(h, "GET", "/api/v1/admin", "", cookie, "", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "password") || strings.Contains(w.Body.String(), "alice-private") {
		t.Fatal(w.Code, w.Body.String())
	}
	foreign, err := db.ReserveSession(context.Background(), 1, "firefox")
	if err != nil {
		t.Fatal(err)
	}
	w = req(h, "GET", "/api/v1/sessions/"+foreign.ID, "", userCookie, "", "")
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
}
func TestStrictJSONAndStaticUI(t *testing.T) {
	h, _ := fixture(t)
	for _, body := range []string{`{"login":"admin","password":"test-only","admin":true}`, `{"login":"admin","password":"test-only"} {}`, strings.Repeat("x", 20000)} {
		if w := req(h, "POST", "/api/v1/login", body, nil, "", "http://127.0.0.1:8080"); w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		w := req(h, "GET", path, "", nil, "", "")
		if w.Code != 200 || w.Body.Len() == 0 {
			t.Fatal(path, w.Code)
		}
		if w.Header().Get("Content-Security-Policy") == "" {
			t.Fatal("missing CSP")
		}
	}
}

type cleanupRuntime struct{ stopped bool }

func TestReadinessRequiresConfiguredRuntime(t *testing.T) {
	_, db := fixture(t)
	s := &Server{Store: db, Sessions: sessions.New(db, sessions.Unavailable{}), Signal: &Signaling{}}
	if w := req(s.Handler(), "GET", "/readyz", "", nil, "", ""); w.Code != 503 {
		t.Fatalf("unavailable runtime readiness: %d", w.Code)
	}
	if w := req(s.Handler(), "GET", "/healthz", "", nil, "", ""); w.Code != 200 {
		t.Fatalf("runtime outage must not fail liveness: %d", w.Code)
	}
	s.Sessions = sessions.New(db, &cleanupRuntime{})
	if w := req(s.Handler(), "GET", "/readyz", "", nil, "", ""); w.Code != 200 {
		t.Fatalf("available runtime readiness: %d", w.Code)
	}
}

func (*cleanupRuntime) Available() bool                                               { return true }
func (*cleanupRuntime) Start(context.Context, store.Session, store.Application) error { return nil }
func (r *cleanupRuntime) Stop(ctx context.Context, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.stopped = true
	return nil
}
func TestCommittedRevocationSurvivesRequestCancellation(t *testing.T) {
	_, db := fixture(t)
	runtime := &cleanupRuntime{}
	service := sessions.New(db, runtime)
	app := &Server{Store: db, Sessions: service}
	if _, err := service.Launch(context.Background(), 1, "firefox"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetApplication(context.Background(), "firefox", false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := app.reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if !runtime.stopped {
		t.Fatal("committed revocation did not clean runtime")
	}
}
