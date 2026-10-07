package printing

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"golang.org/x/time/rate"
)

// Binary messages: 16-byte job ID, uint32 big-endian offset, PDF bytes.
const ChunkSize = (16 << 10) - 20
const MaxBuffered = 64 << 10

type Channel interface {
	Send([]byte) error
	SendText(string) error
	BufferedAmount() uint64
}

type Message struct {
	Version int    `json:"v"`
	Type    string `json:"type"`
	ID      string `json:"id,omitempty"`
	Size    int    `json:"size,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}

func Send(ctx context.Context, dc Channel, job *Job, allowed func() bool) error {
	id, err := hex.DecodeString(job.ID)
	if err != nil || len(id) != 16 {
		return errors.New("invalid print job ID")
	}
	limiter := rate.NewLimiter(256<<10, 16<<10)
	ready := func() error {
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !allowed() {
				return errors.New("printing permission revoked")
			}
			if dc.BufferedAmount() <= MaxBuffered {
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	text := func(v Message) error {
		if err := ready(); err != nil {
			return err
		}
		data, _ := json.Marshal(v)
		return dc.SendText(string(data))
	}
	if err := text(Message{Version: 1, Type: "job-start", ID: job.ID, Size: len(job.Data), SHA256: job.SHA256}); err != nil {
		return err
	}
	for offset := 0; offset < len(job.Data); {
		n := min(ChunkSize, len(job.Data)-offset)
		if err := limiter.WaitN(ctx, n); err != nil {
			return err
		}
		if err := ready(); err != nil {
			return err
		}
		block := make([]byte, 20+n)
		copy(block, id)
		binary.BigEndian.PutUint32(block[16:20], uint32(offset))
		copy(block[20:], job.Data[offset:offset+n])
		if err := dc.Send(block); err != nil {
			return err
		}
		offset += n
	}
	return text(Message{Version: 1, Type: "job-end", ID: job.ID})
}
