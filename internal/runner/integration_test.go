package runner

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"mvp1/internal/wire"
)

// Real containers, Xvfb, FFmpeg, Pion RTP and DataChannel -> XTEST -> shell.
func TestDockerApplicationPath(t *testing.T) {
	if os.Getenv("MVP_DOCKER_TEST") != "1" {
		t.Skip("set MVP_DOCKER_TEST=1 for actual Docker application integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	namespace := "mvp1-integration"
	if _, err := docker(ctx, "network", "inspect", namespace); err != nil {
		if _, err := docker(ctx, "network", "create", "--internal", namespace); err != nil {
			t.Fatal(err)
		}
	}
	r, err := New(Config{Image: "mvp1-worker:pilot", Namespace: namespace, Network: namespace, PublicIP: "127.0.0.1", UDPBase: 50100})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Reset(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := r.Reset(cleanup); err != nil {
			t.Error(err)
		}
	}()
	var counters []*atomic.Uint64
	connect := func(id, app string) (*webrtc.PeerConnection, *webrtc.DataChannel, chan struct{}) {
		t.Helper()
		if err := r.Start(ctx, wire.Start{ID: id, Template: app, Control: true}); err != nil {
			logs, _ := docker(ctx, "logs", namespace+"-"+id)
			t.Fatalf("start %s: %v\n%s", app, err, logs)
		}
		settings := webrtc.SettingEngine{}
		settings.SetIncludeLoopbackCandidate(true)
		settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
		pc, err := webrtc.NewAPI(webrtc.WithSettingEngine(settings)).NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pc.Close() })
		if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
			t.Fatal(err)
		}
		dc, err := pc.CreateDataChannel("control", nil)
		if err != nil {
			t.Fatal(err)
		}
		opened := make(chan struct{})
		dc.OnOpen(func() { close(opened) })
		counter := &atomic.Uint64{}
		counters = append(counters, counter)
		packets := make(chan struct{}, 1)
		pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
			for {
				packet, _, err := track.ReadRTP()
				if err != nil {
					return
				}
				if len(packet.Payload) > 100 {
					counter.Add(1)
					select {
					case packets <- struct{}{}:
					default:
					}
				}
			}
		})
		offer, err := pc.CreateOffer(nil)
		if err != nil {
			t.Fatal(err)
		}
		gathered := webrtc.GatheringCompletePromise(pc)
		if err := pc.SetLocalDescription(offer); err != nil {
			t.Fatal(err)
		}
		select {
		case <-gathered:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		r.mu.Lock()
		w := r.workers[id]
		r.mu.Unlock()
		var answer webrtc.SessionDescription
		if err := r.workerCall(ctx, w, "/offer", wire.Offer{Generation: id, SDP: *pc.LocalDescription(), Control: true}, &answer); err != nil {
			t.Fatal(err)
		}
		if err := pc.SetRemoteDescription(answer); err != nil {
			t.Fatal(err)
		}
		select {
		case <-opened:
		case <-ctx.Done():
			t.Fatal("datachannel", ctx.Err())
		}
		select {
		case <-packets:
		case <-ctx.Done():
			t.Fatal("video RTP", ctx.Err())
		}
		leaseCtx, stop := context.WithCancel(ctx)
		t.Cleanup(stop)
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-leaseCtx.Done():
					return
				case <-ticker.C:
					var result any
					_ = r.workerCall(leaseCtx, w, "/lease", wire.Lease{Generation: id, Control: true}, &result)
				}
			}
		}()
		return pc, dc, packets
	}
	firefox := strings.Repeat("a", 64)
	browser, _, _ := connect(firefox, "firefox")
	terminal := strings.Repeat("b", 64)
	_, dc, _ := connect(terminal, "terminal")
	if browser.ConnectionState() != webrtc.PeerConnectionStateConnected {
		t.Fatal("second app disconnected first")
	}
	sendCommand := func(dc *webrtc.DataChannel, command string) {
		t.Helper()
		for _, char := range command {
			code := ""
			switch {
			case char >= 'a' && char <= 'z':
				code = "Key" + strings.ToUpper(string(char))
			case char >= '0' && char <= '9':
				code = "Digit" + string(char)
			case char == ' ':
				code = "Space"
			case char == '/':
				code = "Slash"
			case char == '-':
				code = "Minus"
			}
			for _, down := range []bool{true, false} {
				data, _ := json.Marshal(wire.Input{Version: 1, Type: "key", Code: code, Down: down})
				if err := dc.SendText(string(data)); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, down := range []bool{true, false} {
			data, _ := json.Marshal(wire.Input{Version: 1, Type: "key", Code: "Enter", Down: down})
			if err := dc.SendText(string(data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	sendCommand(dc, "touch /tmp/mvp1-input-proof")
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
proof:
	for {
		select {
		case <-deadline.C:
			logs, _ := docker(ctx, "logs", namespace+"-"+terminal)
			t.Logf("terminal logs: %s", logs)
			snapshot, err := docker(ctx, "exec", namespace+"-"+terminal, "ffmpeg", "-loglevel", "quiet", "-f", "x11grab", "-i", ":99", "-frames:v", "1", "-f", "image2pipe", "-vcodec", "png", "pipe:1")
			if err == nil {
				file, err := os.CreateTemp("", "mvp1-terminal-*.png")
				if err == nil {
					_, _ = file.Write(snapshot)
					_ = file.Close()
					t.Logf("diagnostic screenshot: %s", file.Name())
				}
			}
			t.Fatal("keyboard did not create proof file")
		case <-tick.C:
			if _, err := docker(ctx, "exec", namespace+"-"+terminal, "test", "-f", "/tmp/mvp1-input-proof"); err == nil {
				break proof
			}
		}
	}
	secondFirefox := strings.Repeat("c", 64)
	secondTerminal := strings.Repeat("d", 64)
	_, _, _ = connect(secondFirefox, "firefox")
	_, secondDC, _ := connect(secondTerminal, "terminal")
	if _, err := docker(ctx, "exec", namespace+"-"+secondTerminal, "test", "!", "-e", "/tmp/mvp1-input-proof"); err != nil {
		t.Fatal("terminal filesystem isolation failed", err)
	}
	// An old heartbeat must not restore a permission revoked at a newer revision.
	r.mu.Lock()
	firstWorker := r.workers[terminal]
	r.mu.Unlock()
	var result any
	if err := r.workerCall(ctx, firstWorker, "/permissions", wire.Lease{PolicyVersion: 1, Control: false}, &result); err != nil {
		t.Fatal(err)
	}
	sendCommand(dc, "touch /tmp/mvp1-revoked")
	sendCommand(secondDC, "touch /tmp/mvp1-independent")
	baseline := make([]uint64, len(counters))
	for i, c := range counters {
		baseline[i] = c.Load()
	}
	observation := time.NewTicker(time.Second)
	defer observation.Stop()
	for sample := 0; sample < 10; sample++ {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-observation.C:
		}
		for i, c := range counters {
			current := c.Load()
			if current <= baseline[i] {
				t.Fatalf("stream %d stalled in second %d", i, sample+1)
			}
			baseline[i] = current
		}
	}
	if _, err := docker(ctx, "exec", namespace+"-"+terminal, "test", "!", "-e", "/tmp/mvp1-revoked"); err != nil {
		t.Fatal("revoked input still accepted", err)
	}
	if _, err := docker(ctx, "exec", namespace+"-"+secondTerminal, "test", "-f", "/tmp/mvp1-independent"); err != nil {
		t.Fatal("other terminal input stopped", err)
	}
	stats, err := docker(ctx, "stats", "--no-stream", "--format", "{{.Name}} CPU={{.CPUPerc}} RAM={{.MemUsage}} PIDs={{.PIDs}}", namespace+"-"+firefox, namespace+"-"+terminal, namespace+"-"+secondFirefox, namespace+"-"+secondTerminal)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("four streams active for 10s; resource snapshot:\n%s", stats)
	if err := r.Stop(ctx, firefox); err != nil {
		t.Fatal(err)
	}
	for sample := 0; sample < 3; sample++ {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-observation.C:
		}
		for i := 1; i < len(counters); i++ {
			current := counters[i].Load()
			if current <= baseline[i] {
				t.Fatalf("stream %d stopped after closing first application", i)
			}
			baseline[i] = current
		}
	}
	t.Log("closing first application preserved the other three streams")

}
