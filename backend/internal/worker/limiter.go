package worker

import (
	"sync"

	"github.com/google/uuid"
)

// Limiter bounds checks in flight: in total, per tenant and per target host. It never blocks:
// TryAcquire answers at once, so a saturated tenant or host cannot hold up jobs of others
// (ADR-0020).
type Limiter struct {
	mu                        sync.Mutex
	total, perTenant, perHost int
	inFlight                  int
	tenants                   map[uuid.UUID]int
	hosts                     map[string]int
}

// NewLimiter returns a Limiter; every limit is at least 1.
func NewLimiter(total, perTenant, perHost int) *Limiter {
	return &Limiter{
		total: max(total, 1), perTenant: max(perTenant, 1), perHost: max(perHost, 1),
		tenants: map[uuid.UUID]int{}, hosts: map[string]int{},
	}
}

// TryAcquire takes a slot for one check if all three limits allow it.
func (l *Limiter) TryAcquire(tenant uuid.UUID, host string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight >= l.total || l.tenants[tenant] >= l.perTenant || l.hosts[host] >= l.perHost {
		return false
	}
	l.inFlight++
	l.tenants[tenant]++
	l.hosts[host]++
	return true
}

// Release returns a slot taken by TryAcquire.
func (l *Limiter) Release(tenant uuid.UUID, host string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inFlight--
	if l.tenants[tenant]--; l.tenants[tenant] <= 0 {
		delete(l.tenants, tenant)
	}
	if l.hosts[host]--; l.hosts[host] <= 0 {
		delete(l.hosts, host)
	}
}

// Free is the number of checks that may still start, ignoring per-tenant and per-host limits.
func (l *Limiter) Free() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total - l.inFlight
}
