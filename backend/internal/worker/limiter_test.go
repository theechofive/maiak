package worker

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLimiter(t *testing.T) {
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	l := NewLimiter(4, 2, 2)

	if !l.TryAcquire(a, "x.example") || !l.TryAcquire(a, "y.example") {
		t.Fatal("tenant a below its limit was refused")
	}
	if l.TryAcquire(a, "z.example") {
		t.Fatal("tenant a above its limit of 2 was admitted")
	}
	// A saturated tenant does not hold up others, but a saturated host does.
	if !l.TryAcquire(b, "x.example") {
		t.Fatal("tenant b refused while only tenant a is saturated")
	}
	if l.TryAcquire(c, "x.example") {
		t.Fatal("host x.example above its limit of 2 was admitted")
	}
	if !l.TryAcquire(c, "w.example") {
		t.Fatal("tenant c on another host refused")
	}
	if l.Free() != 0 || l.TryAcquire(c, "v.example") {
		t.Fatalf("total limit of 4 not enforced (free %d)", l.Free())
	}
	l.Release(a, "x.example")
	if l.Free() != 1 || !l.TryAcquire(c, "x.example") {
		t.Fatal("released slot not reusable")
	}
	for _, r := range []struct {
		tenant uuid.UUID
		host   string
	}{{a, "y.example"}, {b, "x.example"}, {c, "w.example"}, {c, "x.example"}} {
		l.Release(r.tenant, r.host)
	}
	if l.Free() != 4 || len(l.tenants) != 0 || len(l.hosts) != 0 {
		t.Fatalf("after releasing everything: free %d tenants %v hosts %v", l.Free(), l.tenants, l.hosts)
	}
}

func TestLimiterConcurrent(t *testing.T) {
	l := NewLimiter(10, 3, 100)
	tenants := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	var mu sync.Mutex
	inFlight, peak := map[uuid.UUID]int{}, map[uuid.UUID]int{}
	total, peakTotal := 0, 0
	var wg sync.WaitGroup
	for i := range 400 {
		tenant := tenants[i%len(tenants)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !l.TryAcquire(tenant, "h") {
				time.Sleep(time.Microsecond)
			}
			mu.Lock()
			inFlight[tenant]++
			total++
			peak[tenant] = max(peak[tenant], inFlight[tenant])
			peakTotal = max(peakTotal, total)
			mu.Unlock()
			time.Sleep(50 * time.Microsecond)
			mu.Lock()
			inFlight[tenant]--
			total--
			mu.Unlock()
			l.Release(tenant, "h")
		}()
	}
	wg.Wait()
	if peakTotal > 10 {
		t.Fatalf("peak in flight %d > 10", peakTotal)
	}
	for tenant, p := range peak {
		if p > 3 {
			t.Fatalf("tenant %s peaked at %d > 3", tenant, p)
		}
	}
}
