package notify

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/theechofive/maiak/backend/internal/audit"
	"github.com/theechofive/maiak/backend/internal/notify/notifydb"
	"github.com/theechofive/maiak/backend/internal/platform/crypto"
	"github.com/theechofive/maiak/backend/internal/platform/db"
	"github.com/theechofive/maiak/backend/internal/platform/uuidv7"
	"github.com/theechofive/maiak/backend/internal/platform/validation"
)

// Channel statuses (notification_channels.status).
const (
	StatusActive = "active"
	StatusFailed = "failed"
)

const typeSlack = "slack_webhook"

// Audit actions written by this module. Their metadata never contains the webhook URL.
const (
	ActionSlackUpdated = "slack.updated"
	ActionSlackTested  = "slack.tested"
)

var (
	// ErrNotConfigured means the tenant has no Slack channel.
	ErrNotConfigured = errors.New("notify: slack is not configured")
	// ErrChannelNotFound means a delivery names a channel that does not exist in its tenant.
	ErrChannelNotFound = errors.New("notify: channel not found")
	// ErrUndecryptable means the stored config cannot be decrypted (unknown key or tampering).
	ErrUndecryptable = errors.New("notify: channel config cannot be decrypted")
)

// sender sends one Slack message; *SlackNotifier implements it.
type sender interface {
	Send(ctx context.Context, webhookURL, text string) error
}

// Actor is who changes the settings; it scopes every operation to one tenant.
type Actor struct {
	TenantID uuid.UUID
	UserID   uuid.UUID
	IP       netip.Addr
}

// Settings is what the api may show about the Slack channel: never the URL.
type Settings struct {
	Configured bool
	// Status is StatusActive or StatusFailed; empty when not configured.
	Status string
}

// Target is a channel resolved for delivery.
type Target struct {
	Status     string
	WebhookURL string
}

// Service manages the tenant's Slack channel.
type Service struct {
	pool      *db.Pool
	keys      *crypto.Keyring
	sender    sender
	publicURL string
}

// NewService returns a Service. sender may be nil in processes that never send (it is used by
// Test only).
func NewService(pool *db.Pool, keys *crypto.Keyring, s sender, publicURL string) *Service {
	return &Service{pool: pool, keys: keys, sender: s, publicURL: publicURL}
}

// channelAAD binds a ciphertext to its row, so a value copied to another channel (or tenant)
// does not decrypt.
func channelAAD(id uuid.UUID) []byte {
	return []byte("notification_channels.config_encrypted:" + id.String())
}

// Get returns whether the tenant has a Slack channel and its status.
func (s *Service) Get(ctx context.Context, tenantID uuid.UUID) (Settings, error) {
	var out Settings
	err := db.WithTenantTx(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		ch, err := notifydb.New(tx).GetSlackChannel(ctx, tenantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("notify: get slack channel: %w", err)
		}
		out = Settings{Configured: true, Status: ch.Status}
		return nil
	})
	return out, err
}

// Put validates webhookURL and stores it encrypted with the active key, creating the tenant's
// Slack channel or replacing its URL (which makes a failed channel active again). Monitors are
// routed to it by their notify_slack flag (ADR-0019); monitor_channels is not written.
func (s *Service) Put(ctx context.Context, a Actor, webhookURL string) (Settings, error) {
	if msg := ValidateWebhookURL(webhookURL); msg != "" {
		var verr validation.Error
		verr.Add("webhookUrl", msg)
		return Settings{}, &verr
	}
	err := db.WithTenantTx(ctx, s.pool, a.TenantID, func(tx pgx.Tx) error {
		q := notifydb.New(tx)
		id := uuidv7.New()
		sealed, err := s.keys.Encrypt([]byte(webhookURL), channelAAD(id))
		if err != nil {
			return fmt.Errorf("notify: encrypt: %w", err)
		}
		n, err := q.InsertSlackChannel(ctx, notifydb.InsertSlackChannelParams{ID: id, TenantID: a.TenantID, ConfigEncrypted: sealed})
		if err != nil {
			return fmt.Errorf("notify: insert slack channel: %w", err)
		}
		created := n == 1
		if !created {
			if id, err = q.LockSlackChannel(ctx, a.TenantID); err != nil {
				return fmt.Errorf("notify: lock slack channel: %w", err)
			}
			if sealed, err = s.keys.Encrypt([]byte(webhookURL), channelAAD(id)); err != nil {
				return fmt.Errorf("notify: encrypt: %w", err)
			}
			if err := q.ReplaceChannelConfig(ctx, notifydb.ReplaceChannelConfigParams{TenantID: a.TenantID, ID: id, ConfigEncrypted: sealed}); err != nil {
				return fmt.Errorf("notify: replace slack channel: %w", err)
			}
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: a.TenantID, ActorUserID: &a.UserID, Action: ActionSlackUpdated,
			EntityType: "notification_channel", EntityID: id.String(),
			Metadata: map[string]any{"created": created, "keyId": s.keys.ActiveKeyID()}, IP: a.IP,
		})
	})
	if err != nil {
		return Settings{}, err
	}
	return Settings{Configured: true, Status: StatusActive}, nil
}

// Test sends a test message to the tenant's webhook. It returns ErrNotConfigured without a
// channel and a *SendError when Slack does not accept the message; a revoked webhook (404/410)
// marks the channel failed, like a failed delivery.
func (s *Service) Test(ctx context.Context, a Actor) error {
	var (
		id     uuid.UUID
		target Target
	)
	err := db.WithTenantTx(ctx, s.pool, a.TenantID, func(tx pgx.Tx) error {
		ch, err := notifydb.New(tx).GetSlackChannel(ctx, a.TenantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotConfigured
		}
		if err != nil {
			return fmt.Errorf("notify: get slack channel: %w", err)
		}
		id = ch.ID
		target, err = s.open(ch.ID, ch.Status, ch.ConfigEncrypted)
		return err
	})
	if err != nil {
		return err
	}

	sendErr := s.sender.Send(ctx, target.WebhookURL, TestMessage(s.publicURL))
	result := "ok"
	var se *SendError
	if errors.As(sendErr, &se) {
		result = se.Kind
	} else if sendErr != nil {
		result = KindOther
	}
	// The outcome is recorded even when the request was cancelled meanwhile.
	bg := context.WithoutCancel(ctx)
	err = db.WithTenantTx(bg, s.pool, a.TenantID, func(tx pgx.Tx) error {
		if se != nil && se.Permanent {
			if _, err := s.MarkFailed(bg, tx, a.TenantID, id); err != nil {
				return err
			}
		}
		return audit.Record(bg, tx, audit.Entry{
			TenantID: a.TenantID, ActorUserID: &a.UserID, Action: ActionSlackTested,
			EntityType: "notification_channel", EntityID: id.String(),
			Metadata: map[string]any{"result": result}, IP: a.IP,
		})
	})
	if sendErr != nil {
		return sendErr
	}
	return err
}

// DeliveryTarget reads and decrypts a channel for delivery. tx must be a tenant transaction of
// tenantID (jobs uses the tenant returned by claim_deliveries, so RLS still scopes the read).
func (s *Service) DeliveryTarget(ctx context.Context, tx pgx.Tx, tenantID, channelID uuid.UUID) (Target, error) {
	ch, err := notifydb.New(tx).GetChannel(ctx, notifydb.GetChannelParams{TenantID: tenantID, ID: channelID})
	if errors.Is(err, pgx.ErrNoRows) {
		return Target{}, ErrChannelNotFound
	}
	if err != nil {
		return Target{}, fmt.Errorf("notify: get channel: %w", err)
	}
	if ch.Type != typeSlack {
		return Target{}, ErrChannelNotFound
	}
	return s.open(channelID, ch.Status, ch.ConfigEncrypted)
}

func (s *Service) open(id uuid.UUID, status string, sealed []byte) (Target, error) {
	plain, err := s.keys.Decrypt(sealed, channelAAD(id))
	if err != nil {
		return Target{}, ErrUndecryptable
	}
	return Target{Status: status, WebhookURL: string(plain)}, nil
}

// MarkFailed flags an active channel as failed after a permanent delivery failure; the api
// shows the status and a new PUT makes it active again. tx must be a tenant transaction of
// tenantID. It reports whether the status changed.
func (s *Service) MarkFailed(ctx context.Context, tx pgx.Tx, tenantID, channelID uuid.UUID) (bool, error) {
	n, err := notifydb.New(tx).MarkChannelFailed(ctx, notifydb.MarkChannelFailedParams{TenantID: tenantID, ID: channelID})
	if err != nil {
		return false, fmt.Errorf("notify: mark channel failed: %w", err)
	}
	return n == 1, nil
}
