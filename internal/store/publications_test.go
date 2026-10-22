package store

import (
	"context"
	"path/filepath"
	"testing"

	"mvp1/internal/egress"
)

func TestPublicationIsolationAndRevocation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err = s.Bootstrap(ctx, "admin", "test-password"); err != nil {
		t.Fatal(err)
	}
	users, err := s.Users(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a := Application{ID: "yandex", Name: "Яндекс", Template: "firefox", Enabled: true, StartURL: "https://ya.ru", Access: &egress.Policy{Domains: []string{"ya.ru"}, Ports: []uint16{443}}}
	if err = s.SavePublication(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Application(ctx, users[0].ID, a.ID); err != ErrForbidden {
		t.Fatalf("unassigned publication visible: %v", err)
	}
	if err = s.SaveGrant(ctx, Grant{RoleID: 1, AppID: a.ID, Launch: true, Control: true}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Application(ctx, users[0].ID, a.ID)
	if err != nil || got.StartURL != a.StartURL || got.Access == nil {
		t.Fatalf("publication missing: %+v %v", got, err)
	}
	session, err := s.ReserveSession(ctx, users[0].ID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	a.Name = "Другое название"
	if err = s.SavePublication(ctx, a); err != nil {
		t.Fatal(err)
	}
	updated, err := s.Session(ctx, session.ID)
	if err != nil || updated.State != "stopping" {
		t.Fatalf("existing session not revoked: %+v %v", updated, err)
	}
	a.StartURL = "https://evil.example"
	if err = s.SavePublication(ctx, a); err != ErrInvalid {
		t.Fatalf("unapproved start URL accepted: %v", err)
	}
}
