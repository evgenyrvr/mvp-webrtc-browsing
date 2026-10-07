package input

import (
	"context"
	"errors"
	"math"
	"os/exec"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"github.com/jezek/xgb/xtest"
	"mvp1/internal/wire"
)

type X11 struct {
	mu            sync.Mutex
	conn          *xgb.Conn
	root          xproto.Window
	width, height int
	codes         map[string]byte
	pressed       map[byte]bool
	buttons       map[byte]bool
	lastPointer   uint64
}

var symbols = map[string]uint32{
	"Escape": 0xff1b, "Tab": 0xff09, "Backspace": 0xff08, "Enter": 0xff0d, "Space": 0x20,
	"ShiftLeft": 0xffe1, "ShiftRight": 0xffe2, "ControlLeft": 0xffe3, "ControlRight": 0xffe4,
	"AltLeft": 0xffe9, "AltRight": 0xffea, "MetaLeft": 0xffeb, "MetaRight": 0xffec,
	"ArrowLeft": 0xff51, "ArrowUp": 0xff52, "ArrowRight": 0xff53, "ArrowDown": 0xff54,
	"Home": 0xff50, "End": 0xff57, "PageUp": 0xff55, "PageDown": 0xff56, "Delete": 0xffff, "Insert": 0xff63,
	"CapsLock": 0xffe5, "Minus": '-', "Equal": '=', "BracketLeft": '[', "BracketRight": ']', "Backslash": '\\',
	"Semicolon": ';', "Quote": '\'', "Backquote": '`', "Comma": ',', "Period": '.', "Slash": '/',
}

func Open(display string, width, height int) (*X11, error) {
	c, err := xgb.NewConnDisplay(display)
	if err != nil {
		return nil, err
	}
	if err := xtest.Init(c); err != nil {
		c.Close()
		return nil, err
	}
	setup := xproto.Setup(c)
	m, err := xproto.GetKeyboardMapping(c, setup.MinKeycode, byte(int(setup.MaxKeycode)-int(setup.MinKeycode)+1)).Reply()
	if err != nil {
		c.Close()
		return nil, err
	}
	keys := map[string]uint32{}
	for k, v := range symbols {
		keys[k] = v
	}
	for i := 0; i < 26; i++ {
		keys["Key"+string(rune('A'+i))] = uint32('a' + i)
	}
	for i := 0; i < 10; i++ {
		keys["Digit"+string(rune('0'+i))] = uint32('0' + i)
	}
	keys["F1"] = 0xffbe
	keys["F2"] = 0xffbf
	keys["F3"] = 0xffc0
	keys["F4"] = 0xffc1
	keys["F5"] = 0xffc2
	keys["F6"] = 0xffc3
	keys["F7"] = 0xffc4
	keys["F8"] = 0xffc5
	keys["F9"] = 0xffc6
	keys["F10"] = 0xffc7
	keys["F11"] = 0xffc8
	keys["F12"] = 0xffc9
	codes := map[string]byte{}
	for key, sym := range keys {
		for i, s := range m.Keysyms {
			if uint32(s) == sym {
				codes[key] = byte(int(setup.MinKeycode) + i/int(m.KeysymsPerKeycode))
				break
			}
		}
	}
	return &X11{conn: c, root: setup.DefaultScreen(c).Root, width: width, height: height, codes: codes, pressed: map[byte]bool{}, buttons: map[byte]bool{}}, nil
}
func (x *X11) event(kind, detail byte, xpos, ypos int16) error {
	return xtest.FakeInputChecked(x.conn, kind, detail, 0, x.root, xpos, ypos, 0).Check()
}

// WaitForWindow keeps readiness false until the application has a visible window.
// Xvfb accepting connections does not mean that the application can receive input.
func (x *X11) WaitForWindow(ctx context.Context) error {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			tree, err := xproto.QueryTree(x.conn, x.root).Reply()
			if err != nil {
				return err
			}
			for _, child := range tree.Children {
				attributes, err := xproto.GetWindowAttributes(x.conn, child).Reply()
				if err != nil || attributes.MapState != xproto.MapStateViewable {
					continue
				}
				geometry, err := xproto.GetGeometry(x.conn, xproto.Drawable(child)).Reply()
				if err == nil && geometry.Width >= 100 && geometry.Height >= 100 {
					return nil
				}
			}
		}
	}
}
func Valid(v wire.Input) bool {
	if v.Version != 1 {
		return false
	}
	switch v.Type {
	case "pointer", "button":
		return !math.IsNaN(v.X) && !math.IsNaN(v.Y) && v.X >= 0 && v.X <= 1 && v.Y >= 0 && v.Y <= 1 && (v.Type != "button" || (v.Button >= 0 && v.Button <= 2))
	case "key":
		return len(v.Code) > 0 && len(v.Code) <= 32
	case "wheel":
		return v.Delta >= -3 && v.Delta <= 3
	case "reset":
		return true
	case "layout":
		return v.Code == "us" || v.Code == "ru"
	}
	return false
}
func (x *X11) Apply(v wire.Input) error {
	if !Valid(v) {
		return errors.New("invalid input event")
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	switch v.Type {
	case "reset":
		return x.reset()
	case "layout":
		if err := x.reset(); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return exec.CommandContext(ctx, "setxkbmap", "-layout", v.Code).Run()
	case "key":
		code, ok := x.codes[v.Code]
		if !ok {
			return nil
		}
		kind := byte(xproto.KeyRelease)
		if v.Down {
			kind = xproto.KeyPress
			x.pressed[code] = true
		} else {
			delete(x.pressed, code)
		}
		return x.event(kind, code, 0, 0)
	case "pointer", "button":
		if v.Type == "button" && v.Sequence > x.lastPointer {
			x.lastPointer = v.Sequence
		}
		if v.Type == "pointer" {
			if v.Sequence <= x.lastPointer {
				return nil
			}
			x.lastPointer = v.Sequence
		}
		if err := x.event(xproto.MotionNotify, 0, int16(v.X*float64(x.width-1)), int16(v.Y*float64(x.height-1))); err != nil {
			return err
		}
		if v.Type == "button" {
			buttons := []byte{1, 2, 3}
			b := buttons[v.Button]
			kind := byte(xproto.ButtonRelease)
			if v.Down {
				kind = xproto.ButtonPress
				x.buttons[b] = true
			} else {
				delete(x.buttons, b)
			}
			return x.event(kind, b, 0, 0)
		}
	case "wheel":
		button := byte(5)
		count := v.Delta
		if count < 0 {
			button = 4
			count = -count
		}
		for range count {
			if err := x.event(xproto.ButtonPress, button, 0, 0); err != nil {
				return err
			}
			if err := x.event(xproto.ButtonRelease, button, 0, 0); err != nil {
				return err
			}
		}
	}
	return nil
}
func (x *X11) reset() error {
	var errs []error
	for k := range x.pressed {
		errs = append(errs, x.event(xproto.KeyRelease, k, 0, 0))
	}
	for b := range x.buttons {
		errs = append(errs, x.event(xproto.ButtonRelease, b, 0, 0))
	}
	x.pressed = map[byte]bool{}
	x.buttons = map[byte]bool{}
	x.lastPointer = 0
	return errors.Join(errs...)
}
func (x *X11) Reset() error { x.mu.Lock(); defer x.mu.Unlock(); return x.reset() }
func (x *X11) Close()       { _ = x.Reset(); x.conn.Close() }
