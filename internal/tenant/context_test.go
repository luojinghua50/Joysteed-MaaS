package tenant_test

import (
	"context"
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

func TestSetResolvedTenantRejectsReplacement(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	require.NoError(t, tenant.SetResolvedTenant(ctx, "tenant-a"))
	require.ErrorIs(t, tenant.SetResolvedTenant(ctx, "tenant-b"), tenant.ErrTenantConflict)
	require.Equal(t, tenant.ID("tenant-a"), tenant.FromContext(ctx))
}
