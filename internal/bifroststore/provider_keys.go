// Package bifroststore contains the MaaS-specific persistence boundary around
// Bifrost's ConfigStore.
package bifroststore

import (
	"context"
	"errors"
	"fmt"

	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configtables "github.com/maximhq/bifrost/framework/configstore/tables"
	"gorm.io/gorm"
)

var errNilTransaction = errors.New("bifroststore: provider configuration write received a nil transaction")

type platformTransactionRunner func(context.Context, *gorm.DB, func(*gorm.DB) error) error

// PlatformProviderKeyStore grants Bifrost's authenticated management API the
// transaction-local platform mode required to mutate shared Provider settings
// and keys.
// Every other ConfigStore operation is forwarded unchanged.
//
// This wrapper must only be installed on the trusted Bifrost management store.
// Tenant data-plane code must keep using tenant-scoped stores and transactions.
type PlatformProviderKeyStore struct {
	configstore.ConfigStore
	runAcrossTenants   platformTransactionRunner
	enablePlatformMode func(*gorm.DB) error
	updateStatus       func(context.Context, *gorm.DB, schemas.ModelProvider, string, string, string) error
}

func NewPlatformProviderKeyStore(inner configstore.ConfigStore) (*PlatformProviderKeyStore, error) {
	if inner == nil {
		return nil, errors.New("bifroststore: ConfigStore is required")
	}
	if inner.DB() == nil {
		return nil, errors.New("bifroststore: ConfigStore database is required")
	}
	store := &PlatformProviderKeyStore{
		ConfigStore:        inner,
		runAcrossTenants:   tenant.RunAcrossTenants,
		enablePlatformMode: tenant.EnablePlatformMode,
	}
	store.updateStatus = store.updateStatusInTransaction
	return store, nil
}

func (s *PlatformProviderKeyStore) UpdateProvider(
	ctx context.Context,
	provider schemas.ModelProvider,
	config configstore.ProviderConfig,
	txs ...*gorm.DB,
) error {
	return s.withPlatformTransaction(ctx, txs, func(tx *gorm.DB) error {
		return s.ConfigStore.UpdateProvider(ctx, provider, config, tx)
	})
}

func (s *PlatformProviderKeyStore) CreateProviderKey(
	ctx context.Context,
	provider schemas.ModelProvider,
	key schemas.Key,
	txs ...*gorm.DB,
) error {
	return s.withPlatformTransaction(ctx, txs, func(tx *gorm.DB) error {
		return s.ConfigStore.CreateProviderKey(ctx, provider, key, tx)
	})
}

func (s *PlatformProviderKeyStore) UpdateProviderKey(
	ctx context.Context,
	provider schemas.ModelProvider,
	keyID string,
	key schemas.Key,
	txs ...*gorm.DB,
) error {
	return s.withPlatformTransaction(ctx, txs, func(tx *gorm.DB) error {
		return s.ConfigStore.UpdateProviderKey(ctx, provider, keyID, key, tx)
	})
}

func (s *PlatformProviderKeyStore) DeleteProviderKey(
	ctx context.Context,
	provider schemas.ModelProvider,
	keyID string,
	txs ...*gorm.DB,
) error {
	return s.withPlatformTransaction(ctx, txs, func(tx *gorm.DB) error {
		return s.ConfigStore.DeleteProviderKey(ctx, provider, keyID, tx)
	})
}

// UpdateStatus persists model-discovery health for a shared Provider key. The
// upstream ConfigStore method does not accept a transaction, so the wrapper
// performs the same targeted update inside the transaction-local platform
// scope required by config_keys RLS.
func (s *PlatformProviderKeyStore) UpdateStatus(
	ctx context.Context,
	provider schemas.ModelProvider,
	keyID string,
	status string,
	description string,
) error {
	return s.withPlatformTransaction(ctx, nil, func(tx *gorm.DB) error {
		return s.updateStatus(ctx, tx, provider, keyID, status, description)
	})
}

func (s *PlatformProviderKeyStore) updateStatusInTransaction(
	ctx context.Context,
	tx *gorm.DB,
	provider schemas.ModelProvider,
	keyID string,
	status string,
	description string,
) error {
	var result *gorm.DB
	updates := map[string]any{"status": status, "description": description}
	if keyID != "" {
		result = tx.WithContext(ctx).Model(&configtables.TableKey{}).
			Where("key_id = ?", keyID).Updates(updates)
	} else if provider != "" {
		result = tx.WithContext(ctx).Model(&configtables.TableProvider{}).
			Where("name = ?", string(provider)).Updates(updates)
	} else {
		return errors.New("bifroststore: either keyID or provider must be non-empty")
	}
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return configstore.ErrNotFound
	}
	return nil
}

func (s *PlatformProviderKeyStore) withPlatformTransaction(
	ctx context.Context,
	txs []*gorm.DB,
	write func(*gorm.DB) error,
) error {
	if id := tenant.FromContext(ctx); id != "" {
		return fmt.Errorf("%w: tenant %q is resolved on this context", tenant.ErrPlatformModeWithTenant, id)
	}
	if len(txs) == 0 {
		return s.runAcrossTenants(ctx, s.ConfigStore.DB(), write)
	}
	if txs[0] == nil {
		return errNilTransaction
	}
	if err := s.enablePlatformMode(txs[0]); err != nil {
		return fmt.Errorf("bifroststore: enable platform mode for Provider configuration write: %w", err)
	}
	return write(txs[0])
}

var _ configstore.ConfigStore = (*PlatformProviderKeyStore)(nil)
