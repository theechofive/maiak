// Package public is the HTTP side of cmd/statuspage (ADR-0103): it serves the prepared snapshots
// of published status pages as server-rendered HTML, JSON, RSS, a badge for iframes and an embed
// script. It has no sessions and reads only status_page_snapshot(slug) as maiak_statuspage, so a
// bug here can at most show what a snapshot already holds.
package public

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/singleflight"

	"github.com/theechofive/maiak/backend/internal/platform/clock"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/statuspages"
)

// Entry is what the renderer knows about a slug.
type Entry struct {
	Found    bool
	Snapshot statuspages.Snapshot
	// JSON is the snapshot re-encoded from the typed struct, so only allow-listed fields can ever
	// be served, whatever the stored payload holds.
	JSON []byte
	ETag string
	// PasswordHash is the argon2id hash of a protected page, "" for a public one.
	PasswordHash string
}

// Source looks a slug up. The database implementation calls status_page_snapshot; tests use a fake.
type Source interface {
	Lookup(ctx context.Context, slug string) (Entry, error)
}

// DBSource reads snapshots through the definer function, the role's only privilege.
type DBSource struct {
	Pool *db.Pool
}

// Lookup implements Source. An unknown, unpublished or deleted page is Found=false, never an error.
func (s DBSource) Lookup(ctx context.Context, slug string) (Entry, error) {
	var payload []byte
	var etag string
	var hash *string
	err := db.WithTx(ctx, s.Pool, func(tx pgx.Tx) error {
		var generated time.Time
		return tx.QueryRow(ctx, `SELECT payload, etag, generated_at, password_hash FROM status_page_snapshot($1)`, slug).
			Scan(&payload, &etag, &generated, &hash)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Entry{}, nil
	}
	if err != nil {
		return Entry{}, fmt.Errorf("public: snapshot: %w", err)
	}
	return newEntry(payload, etag, hash)
}

func newEntry(payload []byte, etag string, hash *string) (Entry, error) {
	var snap statuspages.Snapshot
	if err := json.Unmarshal(payload, &snap); err != nil {
		return Entry{}, fmt.Errorf("public: decode snapshot: %w", err)
	}
	out, err := json.Marshal(snap)
	if err != nil {
		return Entry{}, err
	}
	e := Entry{Found: true, Snapshot: snap, JSON: out, ETag: etag}
	if hash != nil {
		e.PasswordHash = *hash
	}
	return e, nil
}

// CacheTTL is how long a lookup is reused, found or not (ADR-0103: 15 s).
const CacheTTL = 15 * time.Second

// maxCacheEntries bounds memory under a flood of random slugs (negative answers are cached too).
const maxCacheEntries = 10_000

// Cache is an in-process cache per slug in front of a Source, per replica (L-274). Concurrent misses
// of one slug make one lookup.
type Cache struct {
	src   Source
	clock clock.Clock
	group singleflight.Group
	mu    sync.Mutex
	items map[string]cacheItem
	// OnLookup, when set, is told whether a lookup was a hit.
	OnLookup func(hit bool)
}

type cacheItem struct {
	entry   Entry
	expires time.Time
}

// NewCache wraps src.
func NewCache(src Source, c clock.Clock) *Cache {
	if c == nil {
		c = clock.System{}
	}
	return &Cache{src: src, clock: c, items: map[string]cacheItem{}}
}

// Lookup returns the cached entry or asks the source.
func (c *Cache) Lookup(ctx context.Context, slug string) (Entry, error) {
	now := c.clock.Now()
	c.mu.Lock()
	it, ok := c.items[slug]
	c.mu.Unlock()
	if ok && now.Before(it.expires) {
		c.observe(true)
		return it.entry, nil
	}
	c.observe(false)
	v, err, _ := c.group.Do(slug, func() (any, error) {
		e, err := c.src.Lookup(context.WithoutCancel(ctx), slug)
		if err != nil {
			return Entry{}, err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if len(c.items) >= maxCacheEntries {
			for k, v := range c.items {
				if !now.Before(v.expires) {
					delete(c.items, k)
				}
			}
			// Still full: drop arbitrary entries down to half (bounded memory over accuracy).
			for k := range c.items {
				if len(c.items) < maxCacheEntries/2 {
					break
				}
				delete(c.items, k)
			}
		}
		c.items[slug] = cacheItem{entry: e, expires: c.clock.Now().Add(CacheTTL)}
		return e, nil
	})
	if err != nil {
		return Entry{}, err
	}
	return v.(Entry), nil
}

func (c *Cache) observe(hit bool) {
	if c.OnLookup != nil {
		c.OnLookup(hit)
	}
}
