package portal_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/portal"
	"github.com/luojinghua50/Joysteed-MaaS/internal/rbac"
	tenant "github.com/luojinghua50/Joysteed-MaaS/internal/tenantid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func testRBACStore(t *testing.T) *rbac.Store {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:portal_routes_"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	store := rbac.NewStore(db)
	require.NoError(t, store.Migrate(context.Background()))
	return store
}

func TestRouteGuardDefaultsDeny(t *testing.T) {
	claims, token, err := rbac.NewSessionClaims("s", rbac.Principal{Type: rbac.PrincipalTenantUser, ID: "u", TenantID: "t"}, time.Hour)
	require.NoError(t, err)
	require.Error(t, portal.AuthorizeRoute("GET", "/internal/debug", claims, token, nil, time.Now()))
	require.Error(t, portal.AuthorizeRoute("POST", "/api/portal/usage", claims, token, nil, time.Now()))
}

func TestRequestLogRoutesRequireDedicatedPermission(t *testing.T) {
	ctx := context.Background()
	store := testRBACStore(t)
	tenantID := tenant.ID("t")
	principal := rbac.Principal{Type: rbac.PrincipalTenantUser, ID: "u", TenantID: tenantID}
	claims, token, err := rbac.NewSessionClaims("s", principal, time.Hour)
	require.NoError(t, err)
	require.NoError(t, store.CreateRole(ctx, rbac.Role{ID: "logs", TenantID: &tenantID, Name: "logs"}, []rbac.Permission{rbac.PermissionRequestLogRead}))
	require.NoError(t, store.BindRole(ctx, "logs", principal))
	require.NoError(t, portal.AuthorizeRoute(http.MethodGet, "/api/portal/request-logs/request-1", claims, token, store, time.Now()))
	require.NoError(t, portal.AuthorizeRoute(http.MethodGet, "/api/portal/usage/usage-1/detail", claims, token, store, time.Now()))
	require.Error(t, portal.AuthorizeRoute(http.MethodGet, "/api/portal/usage", claims, token, store, time.Now()))
}
