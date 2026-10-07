package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPasswordHashesAndFixedLoginLifetime(t *testing.T) {
	s := fixture(t)
	ctx := context.Background()
	a := addUser(t, s, "alice")
	b := addUser(t, s, "bob")
	var one, two string
	if err := s.db.QueryRow("SELECT password_hash FROM users WHERE id=?", a).Scan(&one); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT password_hash FROM users WHERE id=?", b).Scan(&two); err != nil {
		t.Fatal(err)
	}
	if one == two || !strings.HasPrefix(one, passwordPrefix) {
		t.Fatal("passwords are not independently salted")
	}
	for _, v := range [][2]string{{"alice", "wrong"}, {"missing", "test-only"}, {"alice", one}} {
		if _, err := s.Login(ctx, v[0], v[1]); !errors.Is(err, ErrUnauthorized) {
			t.Fatal("invalid login accepted", err)
		}
	}
	start := time.Unix(1800000000, 0)
	s.now = func() time.Time { return start }
	token, err := s.Login(ctx, "alice", "test-only")
	if err != nil {
		t.Fatal(err)
	}
	for _, age := range []time.Duration{0, 30 * time.Minute, time.Hour - time.Second} {
		s.now = func() time.Time { return start.Add(age) }
		identity, err := s.Identity(ctx, token)
		if err != nil || identity.ExpiresAt != start.Add(time.Hour).Unix() {
			t.Fatal("expiry changed", identity.ExpiresAt, err)
		}
	}
	s.now = func() time.Time { return start.Add(time.Hour) }
	if _, err = s.Identity(ctx, token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("expired token accepted", err)
	}
	if _, err = s.Login(ctx, "alice", "test-only"); err != nil {
		t.Fatal("cannot login again", err)
	}
	// Blank edit preserves the hash; a changed password revokes current logins.
	token, err = s.Login(ctx, "alice", "test-only")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveUser(ctx, UserInput{ID: a, Login: "alice", Enabled: true, RoleIDs: []int64{2}}); err != nil {
		t.Fatal(err)
	}
	var unchanged string
	s.db.QueryRow("SELECT password_hash FROM users WHERE id=?", a).Scan(&unchanged)
	if unchanged != one {
		t.Fatal("blank edit changed password")
	}
	if _, err = s.SaveUser(ctx, UserInput{ID: a, Login: "alice", Password: "new secret", Enabled: true, RoleIDs: []int64{2}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Identity(ctx, token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("password change kept token")
	}
	if _, err = s.Login(ctx, "alice", "test-only"); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("old password accepted")
	}
	if _, err = s.Login(ctx, "alice", "new secret"); err != nil {
		t.Fatal(err)
	}
}

func TestPlaintextMigrationPreservesPasswordsAndInvalidatesCookies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = s.Bootstrap(ctx, "admin", "initial"); err != nil {
		t.Fatal(err)
	}
	token, err := s.Login(ctx, "admin", "initial")
	if err != nil {
		t.Fatal(err)
	}
	// Includes a hash-looking password: it is plaintext in schema 3 and must be hashed again.
	plain := "$argon2id$v=19$m=19456,t=2,p=1$legacy-password-marker"
	if _, err = s.db.Exec("ALTER TABLE users RENAME COLUMN password_hash TO password; UPDATE users SET password=?; PRAGMA user_version=3", plain); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Identity(ctx, token); !errors.Is(err, ErrUnauthorized) {
		t.Fatal("old cookie survived migration")
	}
	if _, err = s.Login(ctx, "admin", plain); err != nil {
		t.Fatal("legacy password not preserved", err)
	}
	var encoded string
	if err = s.db.QueryRow("SELECT password_hash FROM users WHERE login='admin'").Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	s.Close()
	// Check the current database/WAL, not SSD blocks or external backups.
	for _, name := range []string{path, path + "-wal"} {
		data, err := os.ReadFile(name)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(plain)) {
			t.Fatal("plaintext remains in database", name)
		}
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var again string
	s.db.QueryRow("SELECT password_hash FROM users WHERE login='admin'").Scan(&again)
	if again != encoded {
		t.Fatal("reopening rehashed existing hash")
	}
	if _, err = s.Login(ctx, "admin", plain); err != nil {
		t.Fatal(err)
	}
}
