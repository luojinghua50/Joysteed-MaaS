package rbac_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/rbac"
	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func rbacStore(t *testing.T) *rbac.Store {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:rbac_"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	s := rbac.NewStore(db)
	require.NoError(t, s.Migrate(context.Background()))
	return s
}

func TestTenantRolesCannotCrossScopes(t *testing.T) {
	s := rbacStore(t)
	t1, t2 := tenant.ID("tenant-a"), tenant.ID("tenant-b")
	require.NoError(t, s.CreateRole(context.Background(), rbac.Role{ID: "platform", Name: "platform"}, []rbac.Permission{rbac.PermissionTenantManage}))
	require.NoError(t, s.CreateRole(context.Background(), rbac.Role{ID: "role-a", TenantID: &t1, Name: "a"}, []rbac.Permission{rbac.PermissionKeyManage}))
	require.NoError(t, s.CreateRole(context.Background(), rbac.Role{ID: "role-b", TenantID: &t2, Name: "b"}, []rbac.Permission{rbac.PermissionBillingRead}))
	a := rbac.Principal{Type: rbac.PrincipalTenantUser, ID: "same-user", TenantID: t1}
	b := rbac.Principal{Type: rbac.PrincipalTenantUser, ID: "same-user", TenantID: t2}
	platform := rbac.Principal{Type: rbac.PrincipalPlatformAdmin, ID: "admin"}
	require.ErrorIs(t, s.BindRole(context.Background(), "platform", a), rbac.ErrTenantMismatch)
	require.ErrorIs(t, s.BindRole(context.Background(), "role-b", a), rbac.ErrTenantMismatch)
	require.NoError(t, s.BindRole(context.Background(), "role-a", a))
	require.NoError(t, s.BindRole(context.Background(), "platform", platform))
	require.NoError(t, s.Authorize(context.Background(), a, rbac.PermissionKeyManage))
	require.ErrorIs(t, s.Authorize(context.Background(), b, rbac.PermissionKeyManage), rbac.ErrForbidden)
	require.ErrorIs(t, s.Authorize(context.Background(), a, rbac.PermissionTenantManage), rbac.ErrForbidden)
	require.NoError(t, s.Authorize(context.Background(), platform, rbac.PermissionTenantManage))
}

func TestRBACContextAndSessionFailClosed(t *testing.T) {
	s := rbacStore(t)
	require.ErrorIs(t, s.Require(context.Background(), rbac.PermissionAuditRead), rbac.ErrUnauthenticated)
	p := rbac.Principal{Type: rbac.PrincipalTenantUser, ID: "u", TenantID: "t"}
	ctx, err := rbac.WithPrincipal(context.Background(), p)
	require.NoError(t, err)
	got, ok := rbac.PrincipalFromContext(ctx)
	require.True(t, ok)
	require.Equal(t, p, got)
	claims, token, err := rbac.NewSessionClaims("session", p, time.Minute)
	require.NoError(t, err)
	require.True(t, claims.VerifyCSRF(token))
	require.False(t, claims.VerifyCSRF(token+"x"))
	require.False(t, claims.Valid(time.Now().Add(2*time.Minute)))
	_, _, err = rbac.NewSessionClaims("", p, time.Minute)
	require.ErrorIs(t, err, rbac.ErrInvalidPrincipal)
	require.True(t, errors.Is(rbac.ErrForbidden, rbac.ErrForbidden))
}
