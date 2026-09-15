package configbus_test

import (
	"context"
	"errors"
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/configbus"
	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"github.com/stretchr/testify/require"
)

type source struct{ rows []configbus.Generation }

func (s source) ListGenerations(context.Context) ([]configbus.Generation, error) { return s.rows, nil }

func TestReconcilerFencesDuplicateAndStaleNotifications(t *testing.T) {
	var got []uint64
	r, err := configbus.NewReconciler(source{rows: []configbus.Generation{{TenantID: "tenant-a", Generation: 4}}}, func(_ context.Context, id tenant.ID, generation uint64) error {
		require.Equal(t, tenant.ID("tenant-a"), id)
		got = append(got, generation)
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, r.Observe(context.Background(), "tenant-a", 4))
	require.NoError(t, r.Observe(context.Background(), "tenant-a", 4))
	require.NoError(t, r.Observe(context.Background(), "tenant-a", 3))
	require.Equal(t, []uint64{4}, got)
	require.Equal(t, uint64(4), r.Generation("tenant-a"))
}

func TestReconcilerDoesNotAdvanceFenceAfterReloadFailure(t *testing.T) {
	wantErr := errors.New("snapshot unavailable")
	r, err := configbus.NewReconciler(source{}, func(context.Context, tenant.ID, uint64) error { return wantErr })
	require.NoError(t, err)
	require.ErrorIs(t, r.Observe(context.Background(), "tenant-a", 2), wantErr)
	require.Zero(t, r.Generation("tenant-a"))
}
