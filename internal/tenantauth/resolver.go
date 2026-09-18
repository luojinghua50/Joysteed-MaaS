// Package tenantauth resolves data-plane credentials to control-plane tenants.
package tenantauth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/controlplane"
	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/encrypt"
	"gorm.io/gorm"
)

var (
	ErrInvalidVirtualKey = errors.New("tenantauth: invalid virtual key")
	ErrUnattributedKey   = errors.New("tenantauth: virtual key has no tenant")
	ErrTenantCannotServe = errors.New("tenantauth: tenant cannot serve traffic")
)

type ConfigDatabase interface {
	DB() *gorm.DB
}

type TenantRegistry interface {
	Get(context.Context, tenant.ID) (*controlplane.Tenant, error)
}

type Resolver struct {
	config  ConfigDatabase
	tenants TenantRegistry
}

// NewResolver uses the current configstore pool on every call, including after
// a RefreshConnectionPool. Credentials are never loaded into audit/log payloads.
func NewResolver(config ConfigDatabase, tenants TenantRegistry) (*Resolver, error) {
	if config == nil || tenants == nil {
		return nil, errors.New("tenantauth: config database and tenant registry are required")
	}
	return &Resolver{config: config, tenants: tenants}, nil
}

// keyIdentity deliberately excludes the secret columns and all relationships.
type keyIdentity struct {
	ID                     string
	TenantID               tenant.ID
	IsActive               *bool
	ExpiresAt              *time.Time
	PreviousValueExpiresAt *time.Time
}

// ResolveVirtualKey performs the one privileged lookup needed before a tenant
// can be bound. All subsequent tenant queries must use RunInTenantTx instead.
func (r *Resolver) ResolveVirtualKey(ctx context.Context, value string) (tenant.ID, error) {
	if value == "" {
		return "", ErrInvalidVirtualKey
	}
	db := r.config.DB()
	if db == nil || db.Dialector.Name() != "postgres" {
		return "", errors.New("tenantauth: a Postgres config database is required")
	}
	var key keyIdentity
	now := time.Now().UTC()
	err := tenant.RunAcrossTenants(ctx, db, func(tx *gorm.DB) error {
		query := tx.Table("governance_virtual_keys").Select(
			"id, tenant_id, is_active, expires_at, previous_value_expires_at")
		hash := encrypt.HashSHA256(value)
		err := query.Session(&gorm.Session{}).Where("value_hash = ?", hash).Take(&key).Error
		if err == nil {
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		// Match upstream's rotation lookup order: a live current hash always
		// wins over another VK's retired hash; equality at expiry is invalid.
		err = query.Session(&gorm.Session{}).Where("previous_value_hash = ?", hash).
			Order("previous_value_expires_at DESC").Take(&key).Error
		if err == nil && key.PreviousValueExpiresAt != nil && now.Before(*key.PreviousValueExpiresAt) {
			return nil
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if schemas.IsSecretRef(value) {
			return ErrInvalidVirtualKey
		}
		key = keyIdentity{}
		err = query.Session(&gorm.Session{}).Where("value = ?", value).Take(&key).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrInvalidVirtualKey
		}
		return err
	})
	if err != nil {
		return "", fmt.Errorf("tenantauth: resolve credential: %w", err)
	}
	if key.ID == "" {
		return "", fmt.Errorf("%w: lookup returned no key identity", ErrInvalidVirtualKey)
	}
	if key.IsActive != nil && !*key.IsActive {
		return "", fmt.Errorf("%w: key is inactive", ErrInvalidVirtualKey)
	}
	if key.ExpiresAt != nil && !now.Before(*key.ExpiresAt) {
		return "", fmt.Errorf("%w: key is expired", ErrInvalidVirtualKey)
	}
	if key.TenantID == "" {
		return "", ErrUnattributedKey
	}
	owner, err := r.tenants.Get(ctx, key.TenantID)
	if errors.Is(err, controlplane.ErrTenantNotFound) {
		return "", ErrTenantCannotServe
	}
	if err != nil {
		return "", fmt.Errorf("tenantauth: read tenant registry: %w", err)
	}
	if owner == nil || owner.ID != key.TenantID || !owner.CanServeTraffic(time.Now().UTC()) {
		return "", ErrTenantCannotServe
	}
	return key.TenantID, nil
}
