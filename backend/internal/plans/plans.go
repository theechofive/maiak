// Package plans owns the global plans table and resolves a tenant's effective quota: the limits
// of its plan, overridden key by key by tenants.limits (ADR-0093). Quotas are checked inside the
// caller's transaction under the tenant row lock, so concurrent creates cannot overshoot them.
package plans

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/plans/plansdb"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/tenants"
)

// Seeded plan keys.
const (
	KeyFree     = "free"
	KeyInternal = "internal"
)

// Resource is a kind of thing a quota limits. Its value is the name used in the API (usage
// keys and the `limit` member of the plan-limit problem).
type Resource string

// Resources with a quota.
const (
	ResourceMonitor   Resource = "monitors"
	ResourceHeartbeat Resource = "heartbeats"
	ResourceChannel   Resource = "channels"
)

// JSON keys in plans.limits and tenants.limits.
const (
	keyMaxMonitors   = "max_monitors"
	keyMaxHeartbeats = "max_heartbeats"
	keyMaxChannels   = "max_channels"
)

var (
	// ErrUnknownPlan means a tenant refers to a plan that does not exist.
	ErrUnknownPlan = errors.New("plans: unknown plan")
	// ErrInvalidLimits means stored limits are missing a key or hold a value that is not a
	// non-negative integer.
	ErrInvalidLimits = errors.New("plans: invalid limits")
)

// Limits are the effective quotas of a tenant.
type Limits struct {
	MaxMonitors   int
	MaxHeartbeats int
	MaxChannels   int
}

// Of returns the limit for r.
func (l Limits) Of(r Resource) int {
	switch r {
	case ResourceMonitor:
		return l.MaxMonitors
	case ResourceHeartbeat:
		return l.MaxHeartbeats
	case ResourceChannel:
		return l.MaxChannels
	}
	panic(fmt.Sprintf("plans: unknown resource %q", r))
}

// Counts is the current use of each resource.
type Counts struct {
	Monitors   int
	Heartbeats int
	Channels   int
}

// Plan identifies a plan.
type Plan struct {
	Key  string
	Name string
}

// LimitError means creating one more Resource would exceed the tenant's quota.
type LimitError struct {
	Resource Resource
	Max      int
	// PlanName is the name of the tenant's plan.
	PlanName string
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("plans: limit of %d %s reached", e.Max, e.Resource)
}

// Resolve returns the effective limits of a tenant on plan planKey: the plan's limits
// (planLimits, nil when the plan does not exist) with every key present in overrides
// (tenants.limits) taking precedence. Unknown keys are ignored; known keys must hold
// non-negative integers.
func Resolve(planKey string, planLimits, overrides []byte) (Limits, error) {
	if planLimits == nil {
		return Limits{}, fmt.Errorf("%w %q", ErrUnknownPlan, planKey)
	}
	base, err := parse(planLimits)
	if err != nil {
		return Limits{}, fmt.Errorf("plan %q: %w", planKey, err)
	}
	over, err := parse(overrides)
	if err != nil {
		return Limits{}, fmt.Errorf("tenant overrides: %w", err)
	}
	var l Limits
	for _, f := range []struct {
		key string
		dst *int
	}{{keyMaxMonitors, &l.MaxMonitors}, {keyMaxHeartbeats, &l.MaxHeartbeats}, {keyMaxChannels, &l.MaxChannels}} {
		v, ok := over[f.key]
		if !ok {
			if v, ok = base[f.key]; !ok {
				return Limits{}, fmt.Errorf("%w: plan %q has no %s", ErrInvalidLimits, planKey, f.key)
			}
		}
		*f.dst = v
	}
	return l, nil
}

// parse reads the known keys of a limits object.
func parse(raw []byte) (map[string]int, error) {
	out := map[string]int{}
	if len(bytes.TrimSpace(raw)) == 0 {
		return out, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidLimits, err)
	}
	for _, key := range []string{keyMaxMonitors, keyMaxHeartbeats, keyMaxChannels} {
		v, ok := obj[key]
		if !ok {
			continue
		}
		d := json.NewDecoder(bytes.NewReader(v))
		d.UseNumber()
		var n any
		if err := d.Decode(&n); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrInvalidLimits, key, err)
		}
		num, isNum := n.(json.Number)
		i, err := num.Int64()
		if !isNum || err != nil || i < 0 || i > math.MaxInt32 {
			return nil, fmt.Errorf("%w: %s must be a non-negative integer, got %s", ErrInvalidLimits, key, v)
		}
		out[key] = int(i)
	}
	return out, nil
}

// CheckCreate locks the tenant row until the end of tx and returns a *LimitError when the
// tenant already uses all of its quota for r. Call it in the transaction that inserts the new
// row, before inserting: the lock serialises concurrent creates of one tenant, so the count
// stays true until commit. tx must be a tenant transaction of tenantID.
func CheckCreate(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, r Resource) error {
	q, err := tenants.LockQuota(ctx, tx, tenantID)
	if err != nil {
		return err
	}
	plan, limits, err := resolveQuota(ctx, tx, q)
	if err != nil {
		return err
	}
	n, err := count(ctx, tx, tenantID, r)
	if err != nil {
		return err
	}
	if limit := limits.Of(r); n >= limit {
		return &LimitError{Resource: r, Max: limit, PlanName: plan.Name}
	}
	return nil
}

// Lock locks the tenant row until the end of tx, for callers that must decide whether they
// create anything at all before calling CheckCreate (which locks the same row again).
func Lock(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	_, err := tenants.LockQuota(ctx, tx, tenantID)
	return err
}

func resolveQuota(ctx context.Context, tx pgx.Tx, q tenants.Quota) (Plan, Limits, error) {
	row, err := plansdb.New(tx).GetPlan(ctx, q.PlanKey)
	var raw []byte
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return Plan{}, Limits{}, fmt.Errorf("plans: get plan: %w", err)
	default:
		raw = row.Limits
	}
	l, err := Resolve(q.PlanKey, raw, q.Overrides)
	if err != nil {
		return Plan{}, Limits{}, fmt.Errorf("plans: %w", err)
	}
	return Plan{Key: row.Key, Name: row.Name}, l, nil
}

func count(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, r Resource) (int, error) {
	q := plansdb.New(tx)
	var n int64
	var err error
	switch r {
	case ResourceMonitor:
		n, err = q.CountLiveMonitors(ctx, tenantID)
	case ResourceChannel:
		n, err = q.CountChannels(ctx, tenantID)
	case ResourceHeartbeat:
		n, err = q.CountLiveHeartbeats(ctx, tenantID)
	default:
		return 0, fmt.Errorf("plans: unknown resource %q", r)
	}
	if err != nil {
		return 0, fmt.Errorf("plans: count %s: %w", r, err)
	}
	return int(n), nil
}

// Usage is a tenant's plan, effective limits and current use.
type Usage struct {
	Plan   Plan
	Limits Limits
	Counts Counts
}

// Service answers usage questions for the api.
type Service struct {
	pool *db.Pool
}

// NewService returns a Service.
func NewService(pool *db.Pool) *Service {
	return &Service{pool: pool}
}

// Usage returns the plan, effective limits and current use of a tenant.
func (s *Service) Usage(ctx context.Context, tenantID uuid.UUID) (Usage, error) {
	var u Usage
	err := db.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		q, err := tenants.GetQuota(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		if u.Plan, u.Limits, err = resolveQuota(ctx, tx, q); err != nil {
			return err
		}
		for _, c := range []struct {
			r   Resource
			dst *int
		}{{ResourceMonitor, &u.Counts.Monitors}, {ResourceHeartbeat, &u.Counts.Heartbeats}, {ResourceChannel, &u.Counts.Channels}} {
			if *c.dst, err = count(ctx, tx, tenantID, c.r); err != nil {
				return err
			}
		}
		return nil
	})
	return u, err
}
