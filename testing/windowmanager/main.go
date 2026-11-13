// Command windowmanager checks a clean diagnostic application's X11 session.
// Run only inside a disposable test container: it sends keyboard/mouse input.
package main

import (
	"fmt"
	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
	"mvp1/internal/input"
	"mvp1/internal/wire"
	"time"
)

func main() {
	c, err := xgb.NewConnDisplay(":99")
	must(err)
	defer c.Close()
	root := xproto.Setup(c).DefaultScreen(c).Root
	atom, err := xproto.InternAtom(c, false, uint16(len("_NET_CLIENT_LIST")), "_NET_CLIENT_LIST").Reply()
	must(err)
	var windows []xproto.Window
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		p, e := xproto.GetProperty(c, false, root, atom.Atom, xproto.AtomWindow, 0, 128).Reply()
		must(e)
		if len(p.Value) >= 4 {
			for i := 0; i < len(p.Value); i += 4 {
				windows = append(windows, xproto.Window(xgb.Get32(p.Value[i:])))
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(windows) != 1 {
		panic(fmt.Sprintf("expected one application window, got %d", len(windows)))
	}
	window := windows[0]
	check := func() {
		a, e := xproto.GetWindowAttributes(c, window).Reply()
		must(e)
		if a.MapState != xproto.MapStateViewable {
			panic("application was minimized")
		}
		g, e := xproto.GetGeometry(c, xproto.Drawable(window)).Reply()
		must(e)
		p, e := xproto.TranslateCoordinates(c, window, root, 0, 0).Reply()
		must(e)
		if p.DstX < 0 || p.DstY < 0 || int(p.DstX)+int(g.Width) > 1280 || int(p.DstY)+int(g.Height) > 720 {
			panic("application outside video frame")
		}
		if g.Width < 1200 || g.Height < 680 {
			panic(fmt.Sprintf("application not maximized: %dx%d", g.Width, g.Height))
		}
		fmt.Printf("visible application %dx%d at %d,%d\n", g.Width, g.Height, p.DstX, p.DstY)
	}
	k, err := input.Open(":99", 1280, 720)
	must(err)
	defer k.Close()
	key := func(code string, down bool) {
		must(k.Apply(wire.Input{Version: 1, Type: "key", Code: code, Down: down}))
	}
	for _, chord := range [][]string{{"AltLeft", "F9"}, {"MetaLeft", "KeyD"}, {"AltLeft", "Space"}, {"AltLeft", "Tab"}} {
		for _, code := range chord {
			key(code, true)
		}
		for i := len(chord) - 1; i >= 0; i-- {
			key(chord[i], false)
		}
		time.Sleep(300 * time.Millisecond)
		check()
		key("Escape", true)
		key("Escape", false)
	}
	// Expose the root only in this diagnostic container and right-click it.
	// A forbidden desktop launcher would create an additional visible root child.
	must(xproto.UnmapWindowChecked(c, window).Check())
	c.Sync()
	time.Sleep(200 * time.Millisecond)
	visible := func() int {
		tree, e := xproto.QueryTree(c, root).Reply()
		must(e)
		n := 0
		for _, w := range tree.Children {
			a, _ := xproto.GetWindowAttributes(c, w).Reply()
			g, _ := xproto.GetGeometry(c, xproto.Drawable(w)).Reply()
			if a != nil && g != nil && a.MapState == xproto.MapStateViewable && g.Width > 20 && g.Height > 20 {
				n++
			}
		}
		return n
	}
	before := visible()
	must(k.Apply(wire.Input{Version: 1, Type: "pointer", X: 0.5, Y: 0.5}))
	must(k.Apply(wire.Input{Version: 1, Type: "button", X: 0.5, Y: 0.5, Button: 2, Down: true}))
	must(k.Apply(wire.Input{Version: 1, Type: "button", X: 0.5, Y: 0.5, Button: 2}))
	time.Sleep(300 * time.Millisecond)
	if visible() > before {
		panic("desktop context menu appeared")
	}
	fmt.Println("PASS: minimize shortcuts ineffective; desktop has no context menu")
}
func must(err error) {
	if err != nil {
		panic(err)
	}
}
