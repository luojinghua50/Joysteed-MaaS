package tenant_test

import (
	"context"
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/queryscope"
	"github.com/stretchr/testify/require"
)

type fakeConfigStore struct {
	configstore.ConfigStore
	got context.Context
}

func (s *fakeConfigStore) GetVirtualKeys(ctx context.Context) ([]tables.TableVirtualKey, error) {
	s.got = ctx
	return nil, nil
}

func TestTenantScopedStoreFailsClosedWithoutIdentity(t *testing.T) {
	inner := &fakeConfigStore{}
	store := tenant.NewTenantScopedStore(inner)
	_, err := store.GetVirtualKeys(context.Background())
	require.ErrorIs(t, err, tenant.ErrMissingTenant)
	require.Nil(t, inner.got, "the wrapped store must not be called")
}

func TestTenantScopedStoreInjectsTenantAndKeyScope(t *testing.T) {
	inner := &fakeConfigStore{}
	store := tenant.NewTenantScopedStore(inner)
	ctx := tenant.WithTenant(context.Background(), "tenant-a")
	_, err := store.GetVirtualKeys(ctx)
	require.NoError(t, err)
	require.Equal(t, tenant.ID("tenant-a"), tenant.FromContext(inner.got))
	require.NotNil(t, queryscope.FromContext(inner.got))
}

func TestTenantScopedStoreRequiresExplicitPlatformScope(t *testing.T) {
	inner := &fakeConfigStore{}
	store := tenant.NewTenantScopedStore(inner)
	_, err := store.GetVirtualKeys(tenant.WithPlatformScope(context.Background()))
	require.NoError(t, err)
	require.True(t, tenant.IsPlatformScope(inner.got))
}

func TestTenantScopedStorePreservesConfigStoreInterface(t *testing.T) {
	var _ configstore.ConfigStore = tenant.NewTenantScopedStore(&fakeConfigStore{})
}
