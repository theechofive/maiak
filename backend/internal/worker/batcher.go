package worker

import (
	"context"
	"sync"
	"time"
)

// batcher buffers finished checks and hands them to flush when maxSize are buffered or every
// interval, whichever comes first.
type batcher struct {
	mu      sync.Mutex
	buf     []outcome
	maxSize int
	full    chan struct{}
}

func newBatcher(maxSize int) *batcher {
	return &batcher{maxSize: max(maxSize, 1), full: make(chan struct{}, 1)}
}

func (b *batcher) add(v outcome) {
	b.mu.Lock()
	b.buf = append(b.buf, v)
	full := len(b.buf) >= b.maxSize
	b.mu.Unlock()
	if full {
		select {
		case b.full <- struct{}{}:
		default:
		}
	}
}

// take empties the buffer and returns what it held.
func (b *batcher) take() []outcome {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.buf
	b.buf = nil
	return out
}

// loop flushes until ctx is done. It does not flush on exit; the owner takes the rest.
func (b *batcher) loop(ctx context.Context, interval time.Duration, flush func([]outcome)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-b.full:
		}
		if items := b.take(); len(items) > 0 {
			flush(items)
		}
	}
}
