package results

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/results/resultsdb"
)

// Row is one raw check result. StatusCode and LatencyMs are zero when no response arrived and
// are then stored as NULL; ErrorKind is empty for a successful check.
type Row struct {
	TenantID    uuid.UUID
	MonitorID   uuid.UUID
	LocationID  uuid.UUID
	CheckedAt   time.Time
	OK          bool
	StatusCode  int
	LatencyMs   int
	ErrorKind   string
	TLSNotAfter *time.Time
}

// Insert stores r in tx, a tenant transaction of r.TenantID.
func Insert(ctx context.Context, tx pgx.Tx, r Row) error {
	p := resultsdb.InsertResultParams{
		TenantID: r.TenantID, MonitorID: r.MonitorID, LocationID: r.LocationID, CheckedAt: r.CheckedAt,
		Ok: r.OK, TlsNotAfter: r.TLSNotAfter,
	}
	if r.StatusCode > 0 {
		code, latency := int16(r.StatusCode), int32(max(r.LatencyMs, 0)) //nolint:gosec // HTTP status and a latency bounded by the 10 s timeout
		p.StatusCode, p.LatencyMs = &code, &latency
	}
	if r.ErrorKind != "" {
		p.ErrorKind = &r.ErrorKind
	}
	if err := resultsdb.New(tx).InsertResult(ctx, p); err != nil {
		return fmt.Errorf("results: insert: %w", err)
	}
	return nil
}
