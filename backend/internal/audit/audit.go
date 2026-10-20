// Package audit owns the append-only audit_log table. Entries are written in the caller's
// tenant transaction, so an audited change and its audit row commit or roll back together.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/audit/auditdb"
	"github.com/theechofive/maiak/backend/internal/platform/uuidv7"
)

// Entry is one audit record. Metadata must never contain secrets (passwords, tokens).
type Entry struct {
	TenantID    uuid.UUID
	ActorUserID *uuid.UUID
	Action      string
	EntityType  string
	EntityID    string
	Metadata    map[string]any
	IP          netip.Addr
}

// Record inserts e. tx must be a tenant transaction for e.TenantID (RLS checks it).
func Record(ctx context.Context, tx pgx.Tx, e Entry) error {
	meta := []byte("{}")
	if len(e.Metadata) > 0 {
		var err error
		if meta, err = json.Marshal(e.Metadata); err != nil {
			return fmt.Errorf("audit: metadata: %w", err)
		}
	}
	p := auditdb.InsertAuditEntryParams{
		ID:          uuidv7.New(),
		TenantID:    e.TenantID,
		ActorUserID: e.ActorUserID,
		Action:      e.Action,
		EntityType:  e.EntityType,
		Metadata:    meta,
	}
	if e.EntityID != "" {
		p.EntityID = &e.EntityID
	}
	if e.IP.IsValid() {
		p.Ip = &e.IP
	}
	if err := auditdb.New(tx).InsertAuditEntry(ctx, p); err != nil {
		return fmt.Errorf("audit: insert %s: %w", e.Action, err)
	}
	return nil
}
