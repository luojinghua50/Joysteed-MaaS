package virtualkey

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	tenant "github.com/luojinghua50/Joysteed-MaaS/internal/tenantid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Projection struct {
	ID         string
	TenantID   tenant.ID
	Name       string
	Secret     string
	Generation uint64
}

// Target is the data-plane boundary. Implementations must be idempotent: a
// successful data-plane write can be retried when the control-plane commit is
// interrupted.
type Target interface {
	Upsert(context.Context, Projection) error
	Delete(context.Context, tenant.ID, string) error
}

type Projector struct {
	db     *gorm.DB
	cipher *Cipher
	target Target

	restoreMu sync.Mutex
	restored  map[tenant.ID]struct{}
}

func NewProjector(db *gorm.DB, cipher *Cipher, target Target) (*Projector, error) {
	if db == nil {
		return nil, ErrDatabaseRequired
	}
	if cipher == nil {
		return nil, ErrCipherRequired
	}
	if target == nil {
		return nil, errors.New("virtualkey: projection target is required")
	}
	return &Projector{db: db, cipher: cipher, target: target, restored: make(map[tenant.ID]struct{})}, nil
}

// ProjectTenant converges every dirty key for a tenant. generation is the
// configbus fence that triggered this snapshot; loading the latest desired rows
// is intentional and may safely project a newer generation in one pass.
func (p *Projector) ProjectTenant(ctx context.Context, tenantID tenant.ID, generation uint64) error {
	if tenantID == "" || generation == 0 {
		return fmt.Errorf("virtualkey: invalid projection fence (%q, %d)", tenantID, generation)
	}
	if err := p.restoreTenantOnce(ctx, tenantID); err != nil {
		return err
	}
	var ids []string
	if err := p.db.WithContext(ctx).Model(&Key{}).
		Where("tenant_id = ? AND applied_generation < desired_generation", tenantID).
		Order("desired_generation ASC, id ASC").Pluck("id", &ids).Error; err != nil {
		return fmt.Errorf("virtualkey: list dirty projections: %w", err)
	}
	var joined error
	for _, id := range ids {
		if err := p.projectOne(ctx, tenantID, id); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}

// restoreTenantOnce hydrates a fresh gateway process from the MaaS source of
// truth before generation fencing turns an already-applied snapshot into a
// no-op. Replaying through Target.Upsert repairs both a cold runtime cache and a
// missing/drifted Bifrost row; Target's idempotency makes a retry safe.
func (p *Projector) restoreTenantOnce(ctx context.Context, tenantID tenant.ID) error {
	p.restoreMu.Lock()
	defer p.restoreMu.Unlock()
	if _, ok := p.restored[tenantID]; ok {
		return nil
	}
	var rows []Key
	if err := p.db.WithContext(ctx).
		Where("tenant_id = ? AND desired_state = ? AND status = ? AND applied_generation = desired_generation",
			tenantID, DesiredActive, StatusActive).
		Order("id ASC").Find(&rows).Error; err != nil {
		return fmt.Errorf("virtualkey: load active tenant snapshot: %w", err)
	}
	for i := range rows {
		row := &rows[i]
		secret, err := p.cipher.Decrypt(row.SecretCiphertext, associatedData(string(row.TenantID), row.ID))
		if err == nil {
			err = p.target.Upsert(ctx, Projection{
				ID: row.ID, TenantID: row.TenantID, Name: row.Name,
				Secret: secret, Generation: row.DesiredGeneration,
			})
		}
		if err != nil {
			return fmt.Errorf("virtualkey: restore key %s: %w", row.ID, err)
		}
	}
	p.restored[tenantID] = struct{}{}
	return nil
}

func (p *Projector) projectOne(ctx context.Context, tenantID tenant.ID, keyID string) error {
	var projectionErr error
	txErr := p.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row Key
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("tenant_id = ? AND id = ?", tenantID, keyID).Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("virtualkey: lock projection %s: %w", keyID, err)
		}
		if row.AppliedGeneration >= row.DesiredGeneration {
			return nil
		}

		switch row.DesiredState {
		case DesiredActive:
			secret, err := p.cipher.Decrypt(row.SecretCiphertext, associatedData(string(row.TenantID), row.ID))
			if err == nil {
				err = p.target.Upsert(ctx, Projection{
					ID: row.ID, TenantID: row.TenantID, Name: row.Name,
					Secret: secret, Generation: row.DesiredGeneration,
				})
			}
			if err != nil {
				projectionErr = fmt.Errorf("virtualkey: project key %s: %w", row.ID, err)
				return markFailed(tx, &row, sanitizedError(err, secret))
			}
			now := time.Now().UTC()
			updates := map[string]any{
				"status": StatusActive, "applied_generation": row.DesiredGeneration,
				"last_error": "", "updated_at": now,
			}
			if row.ActivatedAt == nil {
				updates["activated_at"] = now
			}
			return tx.Model(&Key{}).Where("id = ? AND desired_generation = ?", row.ID, row.DesiredGeneration).Updates(updates).Error

		case DesiredRevoked:
			if err := p.target.Delete(ctx, row.TenantID, row.ID); err != nil {
				projectionErr = fmt.Errorf("virtualkey: revoke key %s: %w", row.ID, err)
				return markFailed(tx, &row, sanitizedError(err, ""))
			}
			now := time.Now().UTC()
			return tx.Model(&Key{}).Where("id = ? AND desired_generation = ?", row.ID, row.DesiredGeneration).Updates(map[string]any{
				"status": StatusRevoked, "applied_generation": row.DesiredGeneration,
				"last_error": "", "revoked_at": now, "updated_at": now,
			}).Error

		default:
			return fmt.Errorf("virtualkey: key %s has invalid desired state %q", row.ID, row.DesiredState)
		}
	})
	if txErr != nil {
		return txErr
	}
	return projectionErr
}

func markFailed(tx *gorm.DB, row *Key, reason string) error {
	now := time.Now().UTC()
	return tx.Model(&Key{}).Where("id = ? AND desired_generation = ?", row.ID, row.DesiredGeneration).Updates(map[string]any{
		"status": StatusFailed, "last_error": reason, "updated_at": now,
	}).Error
}

func sanitizedError(err error, secret string) string {
	if err == nil {
		return ""
	}
	message := err.Error()
	if secret != "" {
		message = strings.ReplaceAll(message, secret, "[redacted]")
	}
	message = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, message)
	if len(message) > 500 {
		message = message[:500]
	}
	return message
}
