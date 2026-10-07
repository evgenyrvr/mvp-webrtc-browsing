// Package printing holds complete PDFs privately in a session worker.
package printing

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"time"
)

const MaxPDF = 25 << 20
const MaxJobs = 2
const Lifetime = 10 * time.Minute

var ErrFull = errors.New("print queue full")
var ErrPDF = errors.New("invalid or oversized PDF")

type Job struct {
	ID      string
	SHA256  string
	Data    []byte
	Created time.Time
}

type Queue struct {
	mu      sync.Mutex
	jobs    []*Job
	pending int
	epoch   uint64
}

// Add reserves capacity before reading. Clear invalidates in-flight submissions.
func (q *Queue) Add(ctx context.Context, source io.Reader) (*Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	q.mu.Lock()
	q.expire(time.Now())
	if len(q.jobs)+q.pending >= MaxJobs {
		q.mu.Unlock()
		return nil, ErrFull
	}
	q.pending++
	epoch := q.epoch
	q.mu.Unlock()
	defer func() { q.mu.Lock(); q.pending--; q.mu.Unlock() }()
	data, err := io.ReadAll(io.LimitReader(source, MaxPDF+1))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(data) > MaxPDF || !bytes.HasPrefix(data, []byte("%PDF-")) || !bytes.HasSuffix(bytes.TrimSpace(data), []byte("%%EOF")) {
		return nil, ErrPDF
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	job := &Job{ID: hex.EncodeToString(id), SHA256: hex.EncodeToString(sum[:]), Data: data, Created: time.Now()}
	q.mu.Lock()
	defer q.mu.Unlock()
	if epoch != q.epoch {
		return nil, context.Canceled
	}
	q.jobs = append(q.jobs, job)
	return job, nil
}
func (q *Queue) expire(now time.Time) {
	kept := q.jobs[:0]
	for _, job := range q.jobs {
		if now.Sub(job.Created) < Lifetime {
			kept = append(kept, job)
		}
	}
	clear(q.jobs[len(kept):])
	q.jobs = kept
}
func (q *Queue) Head() *Job {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.expire(time.Now())
	if len(q.jobs) == 0 {
		return nil
	}
	return q.jobs[0]
}
func (q *Queue) Acknowledge(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.jobs) > 0 && q.jobs[0].ID == id {
		q.jobs[0] = nil
		q.jobs = q.jobs[1:]
	}
}
func (q *Queue) Clear() { q.mu.Lock(); defer q.mu.Unlock(); clear(q.jobs); q.jobs = nil; q.epoch++ }
