package worker

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestBatcherFlushesWhenFull(t *testing.T) {
	b := newBatcher(50)
	flushed := make(chan int, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.loop(ctx, time.Hour, func(items []outcome) { flushed <- len(items) })

	for range 49 {
		b.add(outcome{})
	}
	select {
	case n := <-flushed:
		t.Fatalf("flushed %d before the batch was full", n)
	case <-time.After(50 * time.Millisecond):
	}
	b.add(outcome{})
	select {
	case n := <-flushed:
		if n != 50 {
			t.Fatalf("flushed %d, want 50", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a full batch was not flushed")
	}
}

func TestBatcherFlushesOnInterval(t *testing.T) {
	b := newBatcher(50)
	var mu sync.Mutex
	var got []int
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.loop(ctx, 20*time.Millisecond, func(items []outcome) {
			mu.Lock()
			got = append(got, len(items))
			mu.Unlock()
		})
	}()
	start := time.Now()
	for range 3 {
		b.add(outcome{})
	}
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Since(start) > 2*time.Second {
			t.Fatal("partial batch not flushed on the interval")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	b.add(outcome{})
	if left := b.take(); len(left) != 1 || got[0] != 3 {
		t.Fatalf("flushed %v, left %d; the loop must not flush after it stopped", got, len(left))
	}
}
