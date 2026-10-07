package input

import (
	"mvp1/internal/wire"
	"testing"
)

func TestRejectInvalidEvents(t *testing.T) {
	for _, v := range []wire.Input{{Version: 2, Type: "reset"}, {Version: 1, Type: "pointer", X: 2}, {Version: 1, Type: "button", Button: 4}, {Version: 1, Type: "wheel", Delta: 100}, {Version: 1, Type: "layout", Code: "us;anything"}, {Version: 1, Type: "command"}} {
		if Valid(v) {
			t.Fatalf("accepted %+v", v)
		}
	}
	for _, v := range []wire.Input{{Version: 1, Type: "reset"}, {Version: 1, Type: "pointer", X: .5, Y: .5}, {Version: 1, Type: "key", Code: "KeyA"}, {Version: 1, Type: "layout", Code: "ru"}} {
		if !Valid(v) {
			t.Fatalf("rejected %+v", v)
		}
	}
}
