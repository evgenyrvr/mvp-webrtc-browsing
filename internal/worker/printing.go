package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"mvp1/internal/printing"
)

func (w *Worker) printAllowed(p *peer) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ctx.Err() == nil && w.peer != nil && (p == nil || w.peer == p) && w.peer.print && time.Now().Before(w.peer.lease)
}
func (w *Worker) PrintHandler() http.Handler {
	return &printing.IPP{Queue: &w.printQueue, Allowed: func() bool { return w.printAllowed(nil) }}
}
func (w *Worker) printChannel(ctx context.Context, p *peer, dc *webrtc.DataChannel) {
	if !dc.Ordered() || dc.MaxRetransmits() != nil || dc.MaxPacketLifeTime() != nil {
		_ = dc.Close()
		return
	}
	w.mu.Lock()
	if p.printChannel {
		w.mu.Unlock()
		_ = dc.Close()
		return
	}
	p.printChannel = true
	w.mu.Unlock()
	var mu sync.Mutex
	delivered := ""
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		if !msg.IsString || len(msg.Data) > 256 {
			return
		}
		var v printing.Message
		if json.Unmarshal(msg.Data, &v) != nil || v.Version != 1 || v.Type != "job-ack" {
			return
		}
		if !w.printAllowed(p) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if delivered != "" && v.ID == delivered {
			w.printQueue.Acknowledge(v.ID)
			delivered = ""
		}
	})
	dc.OnOpen(func() {
		go func() {
			tick := time.NewTicker(200 * time.Millisecond)
			defer tick.Stop()
			denied := false
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
				if dc.ReadyState() != webrtc.DataChannelStateOpen {
					return
				}
				if !w.printAllowed(p) {
					if !denied {
						_ = dc.SendText(`{"v":1,"type":"print-revoked"}`)
						denied = true
					}
					continue
				}
				denied = false
				job := w.printQueue.Head()
				if job == nil {
					continue
				}
				mu.Lock()
				waiting := delivered == job.ID
				mu.Unlock()
				if waiting {
					continue
				}
				// Publish expected acknowledgement before job-end can reach the browser.
				mu.Lock()
				delivered = job.ID
				mu.Unlock()
				allowed := func() bool {
					return w.printAllowed(p) && w.printQueue.Head() == job && time.Since(job.Created) < printing.Lifetime
				}
				if err := printing.Send(ctx, dc, job, allowed); err != nil {
					mu.Lock()
					delivered = ""
					mu.Unlock()
					if ctx.Err() != nil {
						return
					}
				}
			}
		}()
	})
}
