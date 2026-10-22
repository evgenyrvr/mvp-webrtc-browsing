package appbridge

import (
	"context"
	"net/http/httptest"
	"testing"

	"mvp1/internal/wire"
)

type fakeInput struct{ events int }

func (f *fakeInput) Apply(wire.Input) error { f.events++; return nil }
func (f *fakeInput) Reset() error           { return nil }
func TestAgentAuthenticationAndIdempotentAttach(t *testing.T) {
	input := &fakeInput{}
	srv := httptest.NewServer(Handler("agent-secret", input))
	defer srv.Close()
	c := NewClient()
	if err := c.Attach(srv.URL, "agent-secret"); err != nil {
		t.Fatal(err)
	}
	if err := c.Attach(srv.URL, "agent-secret"); err != nil {
		t.Fatal("retry rejected", err)
	}
	if c.Attach(srv.URL, "other-secret") == nil {
		t.Fatal("attachment replaced")
	}
	if err := c.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Apply(wire.Input{Version: 1, Type: "key", Code: "KeyA", Down: true}); err != nil {
		t.Fatal(err)
	}
	bad := NewClient()
	bad.Attach(srv.URL, "wrong-secret")
	if bad.Apply(wire.Input{}) == nil {
		t.Fatal("unauthorized input accepted")
	}
	if input.events != 1 {
		t.Fatalf("input events: %d", input.events)
	}
}
