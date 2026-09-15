package configbus_test

import (
	"context"
	"errors"
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/configbus"
	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func testStore(t *testing.T) *configbus.Store {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:configbus-test?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	s := configbus.NewStore(db)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { sqlDB, _ := db.DB(); _ = sqlDB.Close() })
	return s
}

func TestPublishAllocatesMonotonicGenerationAndDurableChange(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	first, err := s.Publish(ctx, tenant.ID("tenant-a"), "governance")
	require.NoError(t, err)
	second, err := s.Publish(ctx, tenant.ID("tenant-a"), "routing")
	require.NoError(t, err)
	other, err := s.Publish(ctx, tenant.ID("tenant-b"), "governance")
	require.NoError(t, err)
	require.Equal(t, uint64(1), first.Generation)
	require.Equal(t, uint64(2), second.Generation)
	require.Equal(t, uint64(1), other.Generation)
	require.Equal(t, uint64(2), mustCurrent(t, s, "tenant-a"))
	changes, err := s.ListChanges(ctx, 0, 10)
	require.NoError(t, err)
	require.Len(t, changes, 3)
}

func TestPublishTxRollsBackWithOwningTransaction(t *testing.T) {
	s := testStore(t)
	db := s.DB()
	err := db.Transaction(func(tx *gorm.DB) error {
		_, err := s.PublishTx(tx, tenant.ID("tenant-a"), "governance")
		return errors.Join(err, errors.New("force rollback"))
	})
	require.Error(t, err)
	current, err := s.Current(context.Background(), tenant.ID("tenant-a"))
	require.NoError(t, err)
	require.Zero(t, current)
	changes, err := s.ListChanges(context.Background(), 0, 10)
	require.NoError(t, err)
	require.Empty(t, changes)
}

type notifier struct {
	change configbus.Change
	err    error
}

func (n *notifier) Publish(_ context.Context, change configbus.Change) error {
	n.change = change
	return n.err
}

func TestPublishAndNotifyKeepsDurableChangeWhenAcceleratorFails(t *testing.T) {
	s := testStore(t)
	n := &notifier{err: errors.New("redis unavailable")}
	change, err := s.PublishAndNotify(context.Background(), "tenant-a", "governance", n)
	require.Error(t, err)
	require.NotNil(t, change)
	require.Equal(t, change.ID, n.change.ID)
	require.Equal(t, uint64(1), mustCurrent(t, s, "tenant-a"))
	changes, listErr := s.ListChanges(context.Background(), 0, 10)
	require.NoError(t, listErr)
	require.Len(t, changes, 1)
}

func mustCurrent(t *testing.T, s *configbus.Store, id string) uint64 {
	t.Helper()
	v, err := s.Current(context.Background(), tenant.ID(id))
	require.NoError(t, err)
	return v
}
