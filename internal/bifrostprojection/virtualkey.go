// Package bifrostprojection contains the data-plane adapter for MaaS control
// plane snapshots. It is compiled into maas-gateway only; maas-api does not
// import Bifrost implementation packages.
package bifrostprojection

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"github.com/luojinghua50/Joysteed-MaaS/internal/virtualkey"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configtables "github.com/maximhq/bifrost/framework/configstore/tables"
	"gorm.io/gorm"
)

type Store interface {
	DB() *gorm.DB
	UpdateVirtualKey(context.Context, *configtables.TableVirtualKey, ...*gorm.DB) error
	DeleteVirtualKey(context.Context, string, ...*gorm.DB) error
}

// RuntimeCache is the in-process governance view used on the request path.
// Updating ConfigStore alone is insufficient because Bifrost does not query the
// database for every request.
type RuntimeCache interface {
	UpsertVirtualKey(context.Context, *configtables.TableVirtualKey)
	DeleteVirtualKey(context.Context, string)
}

type Target struct {
	store   Store
	runtime RuntimeCache
}

func New(store Store, runtime RuntimeCache) (*Target, error) {
	if store == nil || store.DB() == nil {
		return nil, errors.New("bifrostprojection: config store is required")
	}
	if runtime == nil {
		return nil, errors.New("bifrostprojection: runtime cache is required")
	}
	return &Target{store: store, runtime: runtime}, nil
}

func (t *Target) Upsert(ctx context.Context, projection virtualkey.Projection) error {
	if projection.ID == "" || projection.TenantID == "" || projection.Secret == "" {
		return errors.New("bifrostprojection: incomplete virtual key projection")
	}
	scopedCtx, err := tenant.ScopedContext(ctx, projection.TenantID)
	if err != nil {
		return err
	}
	active := true
	vk := &configtables.TableVirtualKey{
		ID:                projection.ID,
		Name:              "maas-" + projection.ID,
		Description:       "Managed by MaaS control plane: " + projection.Name,
		Value:             *schemas.NewSecretVar(projection.Secret),
		IsActive:          &active,
		AllowAllProviders: true,
		ConfigHash:        fmt.Sprintf("maas:%d", projection.Generation),
		CreatedAt:         time.Now().UTC(),
		UpdatedAt:         time.Now().UTC(),
	}
	var loaded configtables.TableVirtualKey
	err = tenant.RunInTenantTx(scopedCtx, t.store.DB(), func(tx *gorm.DB) error {
		var count int64
		if err := tx.Table(vk.TableName()).Where("id = ?", vk.ID).Count(&count).Error; err != nil {
			return fmt.Errorf("count projected virtual key: %w", err)
		}
		if count == 0 {
			// Upstream's model intentionally has no tenant_id. Invoke its secret
			// hook explicitly, then include the MaaS extension column in the same
			// INSERT so strict RLS never observes an orphan key.
			if err := vk.BeforeSave(tx.WithContext(scopedCtx)); err != nil {
				return fmt.Errorf("prepare virtual key: %w", err)
			}
			values := map[string]any{
				"id": vk.ID, "name": vk.Name, "description": vk.Description,
				"value": vk.Value, "value_hash": vk.ValueHash,
				"encryption_status": vk.EncryptionStatus, "is_active": true,
				"allow_all_providers": true, "calendar_aligned": false,
				"config_hash": vk.ConfigHash, "tenant_id": string(projection.TenantID),
				"created_at": vk.CreatedAt, "updated_at": vk.UpdatedAt,
			}
			if err := tx.Table(vk.TableName()).Create(values).Error; err != nil {
				return fmt.Errorf("create projected virtual key: %w", err)
			}
		} else if err := t.store.UpdateVirtualKey(scopedCtx, vk, tx); err != nil {
			return fmt.Errorf("update projected virtual key: %w", err)
		}
		if err := tx.Where("id = ?", vk.ID).First(&loaded).Error; err != nil {
			return fmt.Errorf("reload projected virtual key: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	t.runtime.UpsertVirtualKey(ctx, &loaded)
	return nil
}

func (t *Target) Delete(ctx context.Context, tenantID tenant.ID, keyID string) error {
	if tenantID == "" || keyID == "" {
		return errors.New("bifrostprojection: tenant id and key id are required")
	}
	scopedCtx, err := tenant.ScopedContext(ctx, tenantID)
	if err != nil {
		return err
	}
	err = tenant.RunInTenantTx(scopedCtx, t.store.DB(), func(tx *gorm.DB) error {
		err := t.store.DeleteVirtualKey(scopedCtx, keyID, tx)
		if errors.Is(err, configstore.ErrNotFound) {
			return nil
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("delete projected virtual key: %w", err)
	}
	t.runtime.DeleteVirtualKey(ctx, keyID)
	return nil
}
