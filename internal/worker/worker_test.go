package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"golang.org/x/time/rate"
	"mvp1/internal/wire"
)

type testInput struct {
	mu     sync.Mutex
	events int
	resets int
}

func (i *testInput) Apply(wire.Input) error { i.mu.Lock(); defer i.mu.Unlock(); i.events++; return nil }
func (i *testInput) Reset() error           { i.mu.Lock(); defer i.mu.Unlock(); i.resets++; return nil }
func (i *testInput) count() int             { i.mu.Lock(); defer i.mu.Unlock(); return i.events }
func TestPeerMediaAndInput(t *testing.T) {
	if os.Getenv("MVP_RTC_TEST") != "1" {
		t.Skip("set MVP_RTC_TEST=1 to run loopback UDP integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	input := &testInput{}
	worker, err := New(Config{Token: "test-token", Input: input, Capture: func(ctx context.Context, track *webrtc.TrackLocalStaticSample) error {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				if err := track.WriteSample(media.Sample{Data: []byte{0x10, 0, 0}, Duration: 20 * time.Millisecond}); err != nil {
					return err
				}
			}
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	settings := webrtc.SettingEngine{}
	settings.SetIncludeLoopbackCandidate(true)
	settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	client, err := webrtc.NewAPI(webrtc.WithSettingEngine(settings)).NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err = client.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	dc, err := client.CreateDataChannel("control", nil)
	if err != nil {
		t.Fatal(err)
	}
	opened := make(chan struct{})
	dc.OnOpen(func() { close(opened) })
	packets := make(chan struct{}, 1)
	client.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if _, _, err := track.ReadRTP(); err == nil {
			packets <- struct{}{}
		}
	})
	offer, err := client.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(client)
	if err = client.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	generation := strings.Repeat("a", 64)
	answer, err := worker.Offer(ctx, wire.Offer{AuthExpiresAt: time.Now().Add(8 * time.Second).Unix(), Generation: generation, SDP: *client.LocalDescription(), Control: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = client.SetRemoteDescription(answer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-opened:
	case <-ctx.Done():
		t.Fatal("datachannel:", ctx.Err())
	}
	select {
	case <-packets:
	case <-ctx.Done():
		t.Fatal("RTP:", ctx.Err())
	}
	if err = dc.SendText(`{"v":1,"type":"key","code":"KeyA","down":true}`); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for input.count() == 0 {
		select {
		case <-deadline.C:
			t.Fatal("no input received")
		case <-tick.C:
		}
	}
	revoke, _ := json.Marshal(wire.Lease{Control: false})
	req := httptest.NewRequest("POST", "/permissions", bytes.NewReader(revoke))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	worker.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	before := input.count()
	if err = dc.SendText(`{"v":1,"type":"key","code":"KeyB","down":true}`); err != nil {
		t.Fatal(err)
	}
	worker.mu.Lock()
	p := worker.peer
	worker.mu.Unlock()
	if worker.apply(p, "control", wire.Input{Version: 1, Type: "key", Code: "KeyB", Down: true}) {
		t.Fatal("revoked input accepted")
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err := worker.Offer(canceled, wire.Offer{Generation: strings.Repeat("b", 64), SDP: *client.LocalDescription(), Control: true}); err == nil {
		t.Fatal("cancelled offer accepted")
	}
	worker.disconnect("old-generation")
	worker.mu.Lock()
	same := worker.peer == p
	worker.mu.Unlock()
	if !same {
		t.Fatal("stale generation closed current peer")
	}
	// Permission state is checked under the same mutex as input dispatch.
	worker.mu.Lock()
	denied := !worker.peer.control
	worker.mu.Unlock()
	if !denied || input.count() != before {
		t.Fatal("revoked input was accepted")
	}
	// An idle gateway cannot keep media alive beyond the immutable auth expiry.
	expiryWait := time.NewTicker(20 * time.Millisecond)
	defer expiryWait.Stop()
	for {
		worker.mu.Lock()
		expired := worker.peer == nil
		worker.mu.Unlock()
		if expired {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("peer survived authentication expiry")
		case <-expiryWait.C:
		}
	}
}

func TestRevocationCannotBeUndoneByDelayedLease(t *testing.T) {
	input := &testInput{}
	p := &peer{control: true, lease: time.Now().Add(time.Minute), limiter: rate.NewLimiter(250, 500), pointerLimiter: rate.NewLimiter(90, 180)}
	w := &Worker{cfg: Config{Input: input}, peer: p}
	w.mu.Lock()
	w.updatePolicy(wire.Lease{PolicyVersion: 10, Control: false})
	w.updatePolicy(wire.Lease{PolicyVersion: 9, Control: true})
	w.mu.Unlock()
	if w.apply(p, "control", wire.Input{Version: 1, Type: "key", Code: "KeyA", Down: true}) {
		t.Fatal("old policy restored control")
	}
	if input.count() != 0 {
		t.Fatal("input dispatched after revocation")
	}
}
func TestPointerFloodDoesNotLoseKeyRelease(t *testing.T) {
	input := &testInput{}
	p := &peer{control: true, lease: time.Now().Add(time.Minute), limiter: rate.NewLimiter(0, 2), pointerLimiter: rate.NewLimiter(0, 1)}
	w := &Worker{cfg: Config{Input: input}, peer: p}
	if !w.apply(p, "control", wire.Input{Version: 1, Type: "key", Code: "KeyA", Down: true}) {
		t.Fatal("keydown rejected")
	}
	for range 500 {
		w.apply(p, "pointer", wire.Input{Version: 1, Type: "pointer", X: .5, Y: .5})
	}
	if !w.apply(p, "control", wire.Input{Version: 1, Type: "key", Code: "KeyA", Down: false}) {
		t.Fatal("keyup lost to pointer flood")
	}
	w.apply(p, "control", wire.Input{Version: 1, Type: "reset"})
	input.mu.Lock()
	resets := input.resets
	input.mu.Unlock()
	if resets == 0 {
		t.Fatal("dropped control did not reset held keys")
	}
}

func TestLeaseCannotExtendAuthentication(t *testing.T) {
	input := &testInput{}
	deadline := time.Now().Add(time.Second)
	p := &peer{generation: "generation", authExpires: deadline, lease: deadline, control: true, print: true, limiter: rate.NewLimiter(250, 500)}
	w := &Worker{ctx: context.Background(), cfg: Config{Token: "test-token", Input: input}, peer: p}
	renew := func() {
		req := httptest.NewRequest("POST", "/lease", strings.NewReader(`{"generation":"generation","control":true,"print":true}`))
		req.Header.Set("Authorization", "Bearer test-token")
		res := httptest.NewRecorder()
		w.Handler().ServeHTTP(res, req)
		if res.Code != 200 {
			t.Fatal(res.Code)
		}
	}
	renew()
	if !p.lease.Equal(deadline) {
		t.Fatal("lease extended past auth deadline")
	}
	p.authExpires = time.Now().Add(-time.Second)
	renew()
	if w.apply(p, "control", wire.Input{Version: 1, Type: "key", Code: "KeyA", Down: true}) || w.printAllowed(p) {
		t.Fatal("expired identity retains input or print")
	}
}
