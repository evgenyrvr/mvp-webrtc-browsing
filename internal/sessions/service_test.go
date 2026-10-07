package sessions

import (
	"context"
	"errors"
	"mvp1/internal/store"
	"path/filepath"
	"testing"
	"time"
)

type fakeRuntime struct {
	startErr, stopErr error
	started, stopped  int
}

func (*fakeRuntime) Available() bool { return true }
func (f *fakeRuntime) Start(context.Context, store.Session, store.Application) error {
	f.started++
	return f.startErr
}
func (f *fakeRuntime) Stop(context.Context, string) error { f.stopped++; return f.stopErr }
func fixture(t *testing.T) (*store.Store, *Service, *fakeRuntime) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Bootstrap(context.Background(), "admin", "test-only"); err != nil {
		t.Fatal(err)
	}
	f := &fakeRuntime{}
	return db, New(db, f), f
}
func TestFailedStartCleansAndFreesSlot(t *testing.T) {
	db, s, f := fixture(t)
	f.startErr = errors.New("start failed")
	if _, err := s.Launch(context.Background(), 1, "firefox"); err == nil {
		t.Fatal("expected failure")
	}
	active, err := db.Sessions(context.Background(), 1)
	if err != nil || len(active) != 0 || f.stopped != 1 {
		t.Fatal(active, err, f.stopped)
	}
}
func TestFailedCleanupKeepsSlot(t *testing.T) {
	db, s, f := fixture(t)
	f.startErr = errors.New("start failed")
	f.stopErr = errors.New("cleanup failed")
	if _, err := s.Launch(context.Background(), 1, "firefox"); err == nil {
		t.Fatal("expected failure")
	}
	active, err := db.Sessions(context.Background(), 1)
	if err != nil || len(active) != 1 || active[0].State != "stopping" {
		t.Fatal(active, err)
	}
}
func TestIndependentApplicationsOwnershipAndRevocation(t *testing.T) {
	ctx := context.Background()
	db, s, f := fixture(t)
	one, err := s.Launch(ctx, 1, "firefox")
	if err != nil {
		t.Fatal(err)
	}
	two, err := s.Launch(ctx, 1, "terminal")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(ctx, one.ID, 999, false); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("foreign stop", err)
	}
	if err := s.Stop(ctx, one.ID, 1, false); err != nil {
		t.Fatal(err)
	}
	state, err := db.Session(ctx, two.ID)
	if err != nil || state.State != "ready" {
		t.Fatal(state, err)
	}
	if err := db.SetApplication(ctx, "terminal", false); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	active, err := db.Sessions(ctx, 1)
	if err != nil || len(active) != 0 || f.stopped != 2 {
		t.Fatal(active, err, f.stopped)
	}
}
func TestUnavailableRuntimeDoesNotReserve(t *testing.T) {
	db, _, _ := fixture(t)
	s := New(db, Unavailable{})
	if _, err := s.Launch(context.Background(), 1, "firefox"); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	active, err := db.Sessions(context.Background(), 1)
	if err != nil || len(active) != 0 {
		t.Fatal(active, err)
	}
}

func TestReconcileRetriesFailedStop(t *testing.T) {
	db, s, f := fixture(t)
	ctx := context.Background()
	v, err := s.Launch(ctx, 1, "firefox")
	if err != nil {
		t.Fatal(err)
	}
	f.stopErr = errors.New("runtime temporarily unavailable")
	if err := s.Stop(ctx, v.ID, 1, false); err == nil {
		t.Fatal("expected stop error")
	}
	f.stopErr = nil
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	active, err := db.Sessions(ctx, 1)
	if err != nil || len(active) != 0 {
		t.Fatal(active, err)
	}
}

type timeoutRuntime struct {
	timeout bool
	stops   int
}

func (*timeoutRuntime) Available() bool { return true }
func (*timeoutRuntime) Start(context.Context, store.Session, store.Application) error {
	return errors.New("start failed")
}
func (r *timeoutRuntime) Stop(ctx context.Context, _ string) error {
	r.stops++
	if r.timeout {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}
func TestTimedOutCleanupRemainsRetryable(t *testing.T) {
	db, _, _ := fixture(t)
	runtime := &timeoutRuntime{timeout: true}
	s := New(db, runtime)
	s.cleanupTimeout = 100 * time.Millisecond
	if _, err := s.Launch(context.Background(), 1, "firefox"); err == nil {
		t.Fatal("expected failure")
	}
	list, err := db.Sessions(context.Background(), 1)
	if err != nil || len(list) != 1 || list[0].State != "stopping" {
		t.Fatal(list, err)
	}
	runtime.timeout = false
	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	list, err = db.Sessions(context.Background(), 1)
	if err != nil || len(list) != 0 || runtime.stops != 2 {
		t.Fatal(list, err, runtime.stops)
	}
}

func TestLifetimeClosesEveryApplicationAndAllowsNewLaunch(t *testing.T) {
	for _, app := range []string{"falkon", "firefox", "terminal"} {
		t.Run(app, func(t *testing.T) {
			db, s, f := fixture(t)
			ctx := context.Background()
			v, err := s.Launch(ctx, 1, app)
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Unix(v.CreatedAt, 0).Add(30 * time.Minute)
			s.now = func() time.Time { return deadline.Add(-time.Second) }
			if err := s.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if f.stopped != 0 {
				t.Fatal("closed before deadline")
			}
			// Recent state updates or activity must not extend the original deadline.
			if err := db.State(ctx, v.ID, "active"); err != nil {
				t.Fatal(err)
			}
			s.now = func() time.Time { return deadline }
			f.stopErr = errors.New("temporary runtime failure")
			if err := s.Reconcile(ctx); err == nil {
				t.Fatal("missing stop failure")
			}
			saved, err := db.Session(ctx, v.ID)
			if err != nil || saved.State != "stopping" {
				t.Fatal(saved, err)
			}
			f.stopErr = nil
			if err := s.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			saved, err = db.Session(ctx, v.ID)
			if err != nil || saved.State != "closed" {
				t.Fatal(saved, err)
			}
			stops := f.stopped
			if err := s.Reconcile(ctx); err != nil || f.stopped != stops {
				t.Fatal("repeated cleanup", err)
			}
			next, err := s.Launch(ctx, 1, app)
			if err != nil || next.ID == v.ID {
				t.Fatal("new launch rejected", err)
			}
		})
	}
}
