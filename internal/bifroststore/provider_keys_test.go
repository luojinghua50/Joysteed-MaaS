package bifroststore

import (
	"context"
	"errors"
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type fakeConfigStore struct {
	configstore.ConfigStore
	db        *gorm.DB
	operation string
	tx        *gorm.DB
}

func (s *fakeConfigStore) DB() *gorm.DB { return s.db }

func (s *fakeConfigStore) UpdateProvider(_ context.Context, _ schemas.ModelProvider, _ configstore.ProviderConfig, txs ...*gorm.DB) error {
	s.operation = "provider"
	s.tx = txs[0]
	return nil
}

func (s *fakeConfigStore) CreateProviderKey(_ context.Context, _ schemas.ModelProvider, _ schemas.Key, txs ...*gorm.DB) error {
	s.operation = "create"
	s.tx = txs[0]
	return nil
}

func (s *fakeConfigStore) UpdateProviderKey(_ context.Context, _ schemas.ModelProvider, _ string, _ schemas.Key, txs ...*gorm.DB) error {
	s.operation = "update"
	s.tx = txs[0]
	return nil
}

func (s *fakeConfigStore) DeleteProviderKey(_ context.Context, _ schemas.ModelProvider, _ string, txs ...*gorm.DB) error {
	s.operation = "delete"
	s.tx = txs[0]
	return nil
}

func newTestStore(t *testing.T) (*PlatformProviderKeyStore, *fakeConfigStore, *gorm.DB) {
	t.Helper()
	db := &gorm.DB{}
	inner := &fakeConfigStore{db: db}
	store, err := NewPlatformProviderKeyStore(inner)
	require.NoError(t, err)
	store.updateStatus = func(_ context.Context, tx *gorm.DB, _ schemas.ModelProvider, _ string, _, _ string) error {
		inner.operation = "status"
		inner.tx = tx
		return nil
	}
	return store, inner, db
}

func TestPlatformProviderKeyStoreRunsWritesInPlatformTransaction(t *testing.T) {
	store, inner, db := newTestStore(t)
	platformTx := &gorm.DB{}
	runs := 0
	store.runAcrossTenants = func(ctx context.Context, gotDB *gorm.DB, write func(*gorm.DB) error) error {
		runs++
		require.Equal(t, context.Background(), ctx)
		require.Same(t, db, gotDB)
		return write(platformTx)
	}

	tests := []struct {
		name string
		want string
		run  func() error
	}{
		{name: "provider", want: "provider", run: func() error {
			return store.UpdateProvider(context.Background(), schemas.OpenAI, configstore.ProviderConfig{})
		}},
		{name: "create", want: "create", run: func() error {
			return store.CreateProviderKey(context.Background(), schemas.OpenAI, schemas.Key{})
		}},
		{name: "update", want: "update", run: func() error {
			return store.UpdateProviderKey(context.Background(), schemas.OpenAI, "key-1", schemas.Key{})
		}},
		{name: "delete", want: "delete", run: func() error {
			return store.DeleteProviderKey(context.Background(), schemas.OpenAI, "key-1")
		}},
		{name: "status", want: "status", run: func() error {
			return store.UpdateStatus(context.Background(), schemas.OpenAI, "key-1", "healthy", "")
		}},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.NoError(t, test.run())
			require.Equal(t, test.want, inner.operation)
			require.Same(t, platformTx, inner.tx)
			require.Equal(t, index+1, runs)
		})
	}
}

func TestPlatformProviderKeyStoreEnablesSuppliedTransaction(t *testing.T) {
	store, inner, _ := newTestStore(t)
	tx := &gorm.DB{}
	enabled := 0
	store.enablePlatformMode = func(got *gorm.DB) error {
		enabled++
		require.Same(t, tx, got)
		return nil
	}
	store.runAcrossTenants = func(context.Context, *gorm.DB, func(*gorm.DB) error) error {
		t.Fatal("must not open a second transaction")
		return nil
	}

	require.NoError(t, store.UpdateProvider(context.Background(), schemas.OpenAI, configstore.ProviderConfig{}, tx))
	require.Equal(t, 1, enabled)
	require.Equal(t, "provider", inner.operation)
	require.Same(t, tx, inner.tx)
}

func TestPlatformProviderKeyStoreRefusesTenantContext(t *testing.T) {
	store, inner, _ := newTestStore(t)
	store.runAcrossTenants = func(context.Context, *gorm.DB, func(*gorm.DB) error) error {
		t.Fatal("tenant-bound call must fail before opening a platform transaction")
		return nil
	}
	store.enablePlatformMode = func(*gorm.DB) error {
		t.Fatal("tenant-bound call must not enable platform mode")
		return nil
	}

	ctx := tenant.WithTenant(context.Background(), "tenant-a")
	err := store.UpdateProvider(ctx, schemas.OpenAI, configstore.ProviderConfig{})
	require.ErrorIs(t, err, tenant.ErrPlatformModeWithTenant)
	require.Empty(t, inner.operation)

	err = store.CreateProviderKey(ctx, schemas.OpenAI, schemas.Key{})
	require.ErrorIs(t, err, tenant.ErrPlatformModeWithTenant)
	require.Empty(t, inner.operation)

	err = store.DeleteProviderKey(ctx, schemas.OpenAI, "key-1", &gorm.DB{})
	require.ErrorIs(t, err, tenant.ErrPlatformModeWithTenant)
	require.Empty(t, inner.operation)

	err = store.UpdateStatus(ctx, schemas.OpenAI, "key-1", "healthy", "")
	require.ErrorIs(t, err, tenant.ErrPlatformModeWithTenant)
	require.Empty(t, inner.operation)
}

func TestPlatformProviderKeyStoreRejectsNilTransaction(t *testing.T) {
	store, inner, _ := newTestStore(t)
	err := store.UpdateProviderKey(context.Background(), schemas.OpenAI, "key-1", schemas.Key{}, nil)
	require.ErrorIs(t, err, errNilTransaction)
	require.Empty(t, inner.operation)
}

func TestPlatformProviderKeyStorePropagatesPlatformModeFailure(t *testing.T) {
	store, inner, _ := newTestStore(t)
	want := errors.New("cannot enable")
	store.enablePlatformMode = func(*gorm.DB) error { return want }
	err := store.DeleteProviderKey(context.Background(), schemas.OpenAI, "key-1", &gorm.DB{})
	require.ErrorIs(t, err, want)
	require.Empty(t, inner.operation)
}
