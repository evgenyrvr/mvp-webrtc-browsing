package printing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func pdf() []byte { return []byte("%PDF-1.7\n" + strings.Repeat("x", 40000) + "\n%%EOF\n") }
func TestQueueBoundsAndAcknowledgement(t *testing.T) {
	q := &Queue{}
	ctx := context.Background()
	first, err := q.Add(ctx, bytes.NewReader(pdf()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.Add(ctx, bytes.NewReader(pdf())); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Add(ctx, bytes.NewReader(pdf())); !errors.Is(err, ErrFull) {
		t.Fatal(err)
	}
	q.Acknowledge("wrong")
	if q.Head() != first {
		t.Fatal("wrong acknowledgement removed job")
	}
	q.Acknowledge(first.ID)
	if q.Head() == first {
		t.Fatal("acknowledged job retained")
	}
	q.Clear()
	if q.Head() != nil {
		t.Fatal("clear retained job")
	}
	for _, data := range [][]byte{[]byte("not a PDF"), []byte("%PDF-1.7 incomplete"), append([]byte("%PDF-"), bytes.Repeat([]byte("x"), MaxPDF)...)} {
		if _, err := q.Add(ctx, bytes.NewReader(data)); !errors.Is(err, ErrPDF) {
			t.Fatal(err)
		}
	}
}
func TestClearInvalidatesConcurrentSubmission(t *testing.T) {
	q := &Queue{}
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	done := make(chan error, 1)
	go func() { _, err := q.Add(context.Background(), reader); done <- err }()
	// Write returns only after Add has started reading, hence reserved its slot.
	if _, err := writer.Write([]byte("%PDF-1.7\n")); err != nil {
		t.Fatal(err)
	}
	q.Clear()
	if _, err := writer.Write([]byte("%%EOF")); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("revoked submission accepted", err)
	}
	if q.Head() != nil {
		t.Fatal("revoked PDF visible")
	}
}
func TestExpiredDocumentNotRedelivered(t *testing.T) {
	q := &Queue{}
	job, err := q.Add(context.Background(), bytes.NewReader(pdf()))
	if err != nil {
		t.Fatal(err)
	}
	job.Created = time.Now().Add(-Lifetime - time.Second)
	if q.Head() != nil {
		t.Fatal("expired document retained")
	}
}

type capture struct {
	texts     []Message
	blocks    [][]byte
	afterSend func()
	buffer    uint64
}

func (c *capture) Send(data []byte) error {
	c.blocks = append(c.blocks, append([]byte(nil), data...))
	if c.afterSend != nil {
		c.afterSend()
	}
	return nil
}
func (c *capture) SendText(data string) error {
	var v Message
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		return err
	}
	c.texts = append(c.texts, v)
	return nil
}
func (c *capture) BufferedAmount() uint64 { return c.buffer }
func TestTransferReassemblesAndRevocationStopsChunks(t *testing.T) {
	q := &Queue{}
	job, err := q.Add(context.Background(), bytes.NewReader(pdf()))
	if err != nil {
		t.Fatal(err)
	}
	dc := &capture{}
	if err := Send(context.Background(), dc, job, func() bool { return true }); err != nil {
		t.Fatal(err)
	}
	if len(dc.texts) != 2 || dc.texts[0].Type != "job-start" || dc.texts[1].Type != "job-end" {
		t.Fatal(dc.texts)
	}
	var restored []byte
	for _, block := range dc.blocks {
		if len(block) > 16<<10 || hex.EncodeToString(block[:16]) != job.ID || int(binary.BigEndian.Uint32(block[16:20])) != len(restored) {
			t.Fatal("invalid frame")
		}
		restored = append(restored, block[20:]...)
	}
	sum := sha256.Sum256(restored)
	if !bytes.Equal(restored, job.Data) || hex.EncodeToString(sum[:]) != dc.texts[0].SHA256 {
		t.Fatal("document corrupted")
	}
	allowed := true
	revoked := &capture{afterSend: func() { allowed = false }}
	if err := Send(context.Background(), revoked, job, func() bool { return allowed }); err == nil {
		t.Fatal("revocation ignored")
	}
	if len(revoked.blocks) != 1 || len(revoked.texts) != 1 {
		t.Fatal("transfer continued after revocation")
	}
}
func TestBackpressureIsCancelable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	dc := &capture{buffer: MaxBuffered + 1}
	err := Send(ctx, dc, &Job{ID: strings.Repeat("a", 32), Data: pdf(), Created: time.Now()}, func() bool { return true })
	if !errors.Is(err, context.DeadlineExceeded) || len(dc.blocks) > 0 || len(dc.texts) > 0 {
		t.Fatal("backpressure not honored", err)
	}
}
