// Package configbus contains the durable configuration-change boundary between
// the control plane and data-plane nodes.
//
// The outbox is the source of truth. A notification (Redis or otherwise) only
// tells a node to reconcile sooner; it never carries configuration data and it
// is safe to lose. This is the D11a/D12 shape from MAAS_TECH_DESIGN.md.
package configbus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	GenerationTable = "config_generations"
	OutboxTable     = "config_change_outbox"
)

var (
	ErrTenantRequired = errors.New("configbus: tenant id is required")
	ErrEntityRequired = errors.New("configbus: entity is required")
)

// Generation is the current configuration version for one tenant. The value
// is monotonic and is the fencing token used by data-plane reloaders.
type Generation struct {
	TenantID   tenant.ID `gorm:"column:tenant_id;primaryKey;type:text"`
	Generation uint64    `gorm:"column:generation;not null"`
	UpdatedAt  time.Time `gorm:"column:updated_at;not null"`
}

func (Generation) TableName() string { return GenerationTable }

// Change is an immutable durable notification. Entity is deliberately opaque:
// callers can publish "governance", "routing", or a finer-grained resource
// without changing the transport contract.
type Change struct {
	ID          uint64     `gorm:"column:id;primaryKey;autoIncrement"`
	TenantID    tenant.ID  `gorm:"column:tenant_id;type:text;not null;index"`
	Generation  uint64     `gorm:"column:generation;not null"`
	Entity      string     `gorm:"column:entity;type:text;not null"`
	CreatedAt   time.Time  `gorm:"column:created_at;not null;index"`
	PublishedAt *time.Time `gorm:"column:published_at;index"`
}

func (Change) TableName() string { return OutboxTable }

// Store owns the control-plane outbox tables. It can be used directly by
// control-plane handlers, or its PublishTx method can be enlisted in a larger
// transaction that writes the configuration it describes.
type Store struct{ db *gorm.DB }

// Notifier is intentionally narrower than RedisNotifier. Implementations may
// use Redis, an in-process test double, or a future transport; notification is
// an acceleration path and must never become the source of truth.
type Notifier interface {
	Publish(context.Context, Change) error
}

func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

func (s *Store) DB() *gorm.DB {
	if s == nil {
		return nil
	}
	return s.db
}

func (s *Store) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("configbus: database is nil")
	}
	if err := s.db.WithContext(ctx).AutoMigrate(&Generation{}, &Change{}); err != nil {
		return fmt.Errorf("configbus: migrate outbox: %w", err)
	}
	// One generation per tenant and one durable event per generation/entity.
	if err := s.db.WithContext(ctx).Exec(
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_config_outbox_generation_entity ON " + OutboxTable + " (tenant_id, generation, entity)").Error; err != nil {
		return fmt.Errorf("configbus: index outbox: %w", err)
	}
	return nil
}

func validateChange(id tenant.ID, entity string) error {
	if strings.TrimSpace(string(id)) == "" {
		return ErrTenantRequired
	}
	if strings.TrimSpace(entity) == "" {
		return ErrEntityRequired
	}
	return nil
}

// Publish allocates the next tenant generation and records a durable change in
// one transaction. The returned Change is also suitable for immediate Redis
// notification after this method commits.
func (s *Store) Publish(ctx context.Context, id tenant.ID, entity string) (*Change, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("configbus: database is nil")
	}
	if err := validateChange(id, entity); err != nil {
		return nil, err
	}
	var out *Change
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		out, err = s.PublishTx(tx, id, entity)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PublishAndNotify commits the outbox row before sending the best-effort
// acceleration notification. If notification fails, the durable change is
// still present and the caller can report/alert while reconciliation catches
// it later.
func (s *Store) PublishAndNotify(ctx context.Context, id tenant.ID, entity string, notifier Notifier) (*Change, error) {
	change, err := s.Publish(ctx, id, entity)
	if err != nil {
		return nil, err
	}
	if notifier != nil {
		if err := notifier.Publish(ctx, *change); err != nil {
			return change, fmt.Errorf("configbus: durable change %d committed but notification failed: %w", change.ID, err)
		}
	}
	return change, nil
}

// PublishTx is the transaction-enlistment form of Publish. The caller owns
// commit/rollback; tx must already be bound to the control-plane database.
func (s *Store) PublishTx(tx *gorm.DB, id tenant.ID, entity string) (*Change, error) {
	if tx == nil {
		return nil, errors.New("configbus: transaction is nil")
	}
	if err := validateChange(id, entity); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	// The upsert serializes concurrent publishers at the tenant's primary key.
	// RETURNING populates row.Generation on PostgreSQL and SQLite alike.
	row := &Generation{TenantID: id, Generation: 1, UpdatedAt: now}
	if err := tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "tenant_id"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"generation": gorm.Expr("generation + 1"),
			"updated_at": now,
		}),
	}, clause.Returning{Columns: []clause.Column{{Name: "generation"}}}).Create(row).Error; err != nil {
		return nil, fmt.Errorf("configbus: allocate generation for %s: %w", id, err)
	}
	change := &Change{
		TenantID: id, Generation: row.Generation, Entity: strings.TrimSpace(entity), CreatedAt: now,
	}
	if err := tx.Create(change).Error; err != nil {
		return nil, fmt.Errorf("configbus: write outbox change for %s: %w", id, err)
	}
	return change, nil
}

// Current returns the latest generation, or zero when the tenant has no
// configuration change yet.
func (s *Store) Current(ctx context.Context, id tenant.ID) (uint64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("configbus: database is nil")
	}
	if strings.TrimSpace(string(id)) == "" {
		return 0, ErrTenantRequired
	}
	var row Generation
	err := s.db.WithContext(ctx).Where("tenant_id = ?", id).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("configbus: read generation for %s: %w", id, err)
	}
	return row.Generation, nil
}

// ListGenerations returns the cheap reconciliation projection. It never loads
// configuration snapshots.
func (s *Store) ListGenerations(ctx context.Context) ([]Generation, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("configbus: database is nil")
	}
	var rows []Generation
	if err := s.db.WithContext(ctx).Order("tenant_id ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("configbus: list generations: %w", err)
	}
	return rows, nil
}

// ListChanges returns durable changes after an outbox id, oldest first. It is
// useful for an optional dispatcher or audit tooling; correctness does not
// depend on a dispatcher successfully delivering every row.
func (s *Store) ListChanges(ctx context.Context, afterID uint64, limit int) ([]Change, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("configbus: database is nil")
	}
	if limit <= 0 {
		limit = 100
	}
	var rows []Change
	q := s.db.WithContext(ctx).Where("id > ?", afterID).Order("id ASC").Limit(limit)
	if err := q.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("configbus: list outbox: %w", err)
	}
	return rows, nil
}

// MarkPublished is advisory bookkeeping for dispatch metrics. It never gates
// reconciliation and is intentionally separate from the publishing transaction.
func (s *Store) MarkPublished(ctx context.Context, id uint64) error {
	if s == nil || s.db == nil {
		return errors.New("configbus: database is nil")
	}
	now := time.Now().UTC()
	if err := s.db.WithContext(ctx).Model(&Change{}).Where("id = ?", id).
		Updates(map[string]interface{}{"published_at": now}).Error; err != nil {
		return fmt.Errorf("configbus: mark outbox %d published: %w", id, err)
	}
	return nil
}
