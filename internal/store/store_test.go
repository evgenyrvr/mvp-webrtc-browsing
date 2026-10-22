package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func fixture(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Bootstrap(context.Background(), "admin", "test-only-admin"); err != nil {
		t.Fatal(err)
	}
	return s
}
func addUser(t *testing.T, s *Store, name string) int64 {
	t.Helper()
	id, err := s.SaveUser(context.Background(), UserInput{Login: name, Password: "test-only", Enabled: true, RoleIDs: []int64{2}})
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func grant(t *testing.T, s *Store, app string) {
	t.Helper()
	if err := s.SaveGrant(context.Background(), Grant{RoleID: 2, AppID: app, Launch: true, Control: true}); err != nil {
		t.Fatal(err)
	}
}

func TestAuthAndPasswordUpdate(t *testing.T) {
	ctx := context.Background()
	s := fixture(t)
	if _, err := s.Login(ctx, "admin", "wrong"); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	token, err := s.Login(ctx, "admin", "test-only-admin")
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.Identity(ctx, token)
	if err != nil || !v.User.Admin || v.CSRF == "" {
		t.Fatalf("identity: %+v %v", v, err)
	}
	if err := s.Bootstrap(ctx, "admin", "new-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, "admin", "new-password"); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("bootstrap reset password", err)
	}
	if _, err := s.SaveUser(ctx, UserInput{ID: v.User.ID, Login: "admin", Password: "changed", Enabled: true, RoleIDs: []int64{1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Identity(ctx, token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("old token remains valid", err)
	}
	token, err = s.Login(ctx, "admin", "changed")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.LogoutUser(ctx, v.User.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Identity(ctx, token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
}
func TestLastAdministratorTransaction(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	if _, err := s.SaveUser(ctx, UserInput{ID: 1, Login: "admin", Enabled: false, RoleIDs: []int64{2}}); !errors.Is(err, ErrLastAdmin) {
		t.Fatal(err)
	}
	if err := s.SaveRole(ctx, Role{ID: 1, Name: "admin", Admin: false}); !errors.Is(err, ErrLastAdmin) {
		t.Fatal(err)
	}
	token, err := s.Login(ctx, "admin", "test-only-admin")
	if err != nil {
		t.Fatal("transaction did not rollback", err)
	}
	v, err := s.Identity(ctx, token)
	if err != nil || !v.User.Admin {
		t.Fatal(v, err)
	}
}
func TestRBACUnionAndDisabledApplication(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	id := addUser(t, s, "user")
	apps, err := s.Applications(ctx, id)
	if err != nil || len(apps) != 0 {
		t.Fatal(apps, err)
	}
	if _, err := s.ReserveSession(ctx, id, "firefox"); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
	grant(t, s, "firefox")
	if err := s.SaveRole(ctx, Role{Name: "printers"}); err != nil {
		t.Fatal(err)
	}
	roles, _ := s.Roles(ctx)
	printRole := roles[len(roles)-1].ID
	if err := s.SaveGrant(ctx, Grant{RoleID: printRole, AppID: "firefox", Launch: true, Print: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveUser(ctx, UserInput{ID: id, Login: "user", Enabled: true, RoleIDs: []int64{2, printRole}}); err != nil {
		t.Fatal(err)
	}
	a, err := s.Application(ctx, id, "firefox")
	if err != nil || !a.Control || !a.Print {
		t.Fatal(a, err)
	}
	if err := s.SetApplication(ctx, "firefox", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveSession(ctx, id, "firefox"); !errors.Is(err, ErrForbidden) {
		t.Fatal(err)
	}
}
func TestConcurrentReservationLimits(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	grant(t, s, "firefox")
	u := addUser(t, s, "one")
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			_, err := s.ReserveSession(ctx, u, "firefox")
			if err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrLimit) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if successes.Load() != 2 {
		t.Fatalf("reserved %d sessions", successes.Load())
	}
	u2, u3 := addUser(t, s, "two"), addUser(t, s, "three")
	second, err := s.ReserveSession(ctx, u2, "firefox")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveSession(ctx, u3, "firefox"); !errors.Is(err, ErrLimit) {
		t.Fatal("third active user admitted", err)
	}
	if _, err := s.ReserveSession(ctx, u2, "firefox"); err != nil {
		t.Fatal(err)
	}
	if err := s.State(ctx, second.ID, "stopping"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveSession(ctx, u2, "firefox"); !errors.Is(err, ErrLimit) {
		t.Fatal("stopping freed slot", err)
	}
	if err := s.State(ctx, second.ID, "closed"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReserveSession(ctx, u2, "firefox"); err != nil {
		t.Fatal("closed did not free slot", err)
	}
}
func TestReopenPreservesData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persistent.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Bootstrap(context.Background(), "operator", "test-password"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Login(context.Background(), "operator", "test-password"); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapNormalizesLogin(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "bootstrap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Bootstrap(context.Background(), " admin ", "test-only"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(context.Background(), "admin", "test-only"); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyRevisionFollowsRevocation(t *testing.T) {
	ctx := context.Background()
	s := fixture(t)
	user := addUser(t, s, "operator")
	grant(t, s, "firefox")
	before, err := s.Application(ctx, user, "firefox")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveGrant(ctx, Grant{RoleID: 2, AppID: "firefox", Launch: true}); err != nil {
		t.Fatal(err)
	}
	after, err := s.Application(ctx, user, "firefox")
	if err != nil {
		t.Fatal(err)
	}
	if !before.Control || after.Control || after.PolicyVersion <= before.PolicyVersion {
		t.Fatalf("revocation did not advance policy: before=%+v after=%+v", before, after)
	}
}

func TestLateSignalingCannotResurrectSession(t *testing.T) {
	ctx := context.Background()
	s := fixture(t)
	user := addUser(t, s, "operator")
	grant(t, s, "firefox")
	for _, state := range []string{"stopping", "closed", "failed"} {
		t.Run(state, func(t *testing.T) {
			session, err := s.ReserveSession(ctx, user, "firefox")
			if err != nil {
				t.Fatal(err)
			}
			if err := s.State(ctx, session.ID, state); err != nil {
				t.Fatal(err)
			}
			for _, late := range []string{"active", "reconnecting"} {
				changed, err := s.LiveState(ctx, session.ID, late)
				if err != nil || changed {
					t.Fatalf("late %s changed %s: %v", late, state, err)
				}
			}
			got, err := s.Session(ctx, session.ID)
			if err != nil || got.State != state {
				t.Fatalf("session=%+v error=%v", got, err)
			}
			if err := s.State(ctx, session.ID, "closed"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestFalkonMigrationPreservesPermissions(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	user := addUser(t, s, "legacy")
	if err := s.SaveGrant(ctx, Grant{RoleID: 2, AppID: "firefox", Launch: true, Control: false, Print: false}); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the previous catalog without changing users or Firefox grants.
	if _, err := s.db.Exec("ALTER TABLE applications DROP COLUMN start_url; ALTER TABLE applications DROP COLUMN access_policy; ALTER TABLE users RENAME COLUMN password_hash TO password; UPDATE users SET password='test-only'; DELETE FROM role_app_permissions WHERE app_id='falkon'; DELETE FROM applications WHERE id='falkon'; PRAGMA user_version=2"); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(); err != nil {
		t.Fatal(err)
	}
	apps, err := s.Applications(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 2 || apps[0].ID != "falkon" || apps[0].Template != "falkon" || apps[0].Control || apps[0].Print {
		t.Fatalf("migration expanded rights or lost browser: %+v", apps)
	}
	if _, err := s.Login(ctx, "legacy", "test-only"); err != nil {
		t.Fatal("credentials changed", err)
	}
	if err := s.SaveGrant(ctx, Grant{RoleID: 2, AppID: "falkon", Launch: false}); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(); err != nil {
		t.Fatal(err)
	}
	apps, err = s.Applications(ctx, user)
	if err != nil || len(apps) != 1 || apps[0].ID != "firefox" {
		t.Fatal("migration restored revoked permission", apps, err)
	}
}
