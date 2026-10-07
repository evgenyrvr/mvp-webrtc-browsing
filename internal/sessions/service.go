package sessions

import (
	"context"
	"errors"
	"mvp1/internal/store"
	"mvp1/internal/wire"
	"sync"
	"time"
)

var ErrUnavailable = errors.New("runtime is not configured")

// Runtime.Start must clean up partial resources on failure; Stop must be idempotent.
// No user-supplied command, image, mount or network option crosses this boundary.
type Runtime interface {
	Available() bool
	Start(context.Context, store.Session, store.Application) error
	Stop(context.Context, string) error
}
type Unavailable struct{}

func (Unavailable) Available() bool { return false }
func (Unavailable) Start(context.Context, store.Session, store.Application) error {
	return ErrUnavailable
}
func (Unavailable) Stop(context.Context, string) error { return ErrUnavailable }

type Service struct {
	store          *store.Store
	runtime        Runtime
	mu             sync.Mutex
	cleanupTimeout time.Duration
	now            func() time.Time
}

func New(s *store.Store, r Runtime) *Service {
	return &Service{store: s, runtime: r, cleanupTimeout: 5 * time.Second, now: time.Now}
}
func (s *Service) Available() bool { return s.runtime.Available() }

// Serialize lifecycle operations for the four-session pilot. This does not hold
// the database connection during runtime I/O; all runtime calls have deadlines.
func (s *Service) Launch(ctx context.Context, user int64, app string) (store.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.store.Application(ctx, user, app)
	if err != nil {
		return store.Session{}, err
	}
	if !s.Available() {
		return store.Session{}, ErrUnavailable
	}
	v, err := s.store.ReserveSession(ctx, user, app)
	if err != nil {
		return v, err
	}
	start, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err = s.runtime.Start(start, v, a); err != nil {
		return v, errors.Join(err, s.cleanupFailedLaunch(v.ID, "failed"))
	}
	if err = s.store.State(ctx, v.ID, "ready"); err != nil {
		return v, errors.Join(err, s.cleanupFailedLaunch(v.ID, "closed"))
	}
	v.State = "ready"
	return v, nil
}

func (s *Service) cleanupFailedLaunch(id, terminalState string) error {
	// Persist retryable state before a potentially timing-out runtime operation.
	mark, cancelMark := context.WithTimeout(context.Background(), s.cleanupTimeout)
	err := s.store.State(mark, id, "stopping")
	cancelMark()
	stop, cancelStop := context.WithTimeout(context.Background(), s.cleanupTimeout)
	stopErr := s.runtime.Stop(stop, id)
	cancelStop()
	if stopErr != nil {
		return errors.Join(err, stopErr)
	}
	finish, cancelFinish := context.WithTimeout(context.Background(), s.cleanupTimeout)
	defer cancelFinish()
	return errors.Join(err, s.store.State(finish, id, terminalState))
}
func (s *Service) Stop(ctx context.Context, id string, user int64, admin bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.store.Session(ctx, id)
	if err != nil {
		return err
	}
	if v.UserID != user && !admin {
		return store.ErrNotFound
	}
	return s.stop(ctx, v)
}
func (s *Service) stop(ctx context.Context, v store.Session) error {
	if v.State == "closed" || v.State == "failed" {
		return nil
	}
	if err := s.store.State(ctx, v.ID, "stopping"); err != nil {
		return err
	}
	stop, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := s.runtime.Stop(stop, v.ID); err != nil {
		return err
	}
	return s.store.State(ctx, v.ID, "closed")
}
func (s *Service) StopUser(ctx context.Context, user int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.store.Sessions(ctx, user)
	if err != nil {
		return err
	}
	var errs []error
	for _, v := range list {
		if err := s.stop(ctx, v); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Reconcile closes revoked/stopping sessions and pushes current input permissions.
func (s *Service) Reconcile(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := s.store.Sessions(ctx, 0)
	if err != nil {
		return err
	}
	var errs []error
	for _, v := range list {
		// Check before runtime health: an unavailable health endpoint must not
		// exempt an expired session from cleanup. Reconnect does not reset age.
		if v.State == "stopping" || !s.now().Before(time.Unix(v.CreatedAt, 0).Add(wire.MaxSessionLifetime)) {
			errs = append(errs, s.stop(ctx, v))
			continue
		}
		if runtime, ok := s.runtime.(interface {
			Alive(context.Context, string) (bool, error)
		}); ok {
			alive, err := runtime.Alive(ctx, v.ID)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if !alive {
				errs = append(errs, s.stop(ctx, v))
				continue
			}
		}
		permission, err := s.store.Application(ctx, v.UserID, v.AppID)
		if errors.Is(err, store.ErrForbidden) || (err == nil && v.State == "stopping") {
			err = s.stop(ctx, v)
		} else if err == nil {
			if runtime, ok := s.runtime.(interface {
				Permissions(context.Context, string, store.Application) error
			}); ok {
				err = runtime.Permissions(ctx, v.ID, permission)
			}
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
