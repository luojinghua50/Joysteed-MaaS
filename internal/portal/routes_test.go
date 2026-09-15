package portal_test

import (
	"testing"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/portal"
	"github.com/luojinghua50/Joysteed-MaaS/internal/rbac"
	"github.com/stretchr/testify/require"
)

func TestRouteGuardDefaultsDeny(t *testing.T) {
	claims, token, err := rbac.NewSessionClaims("s", rbac.Principal{Type: rbac.PrincipalTenantUser, ID: "u", TenantID: "t"}, time.Hour)
	require.NoError(t, err)
	require.Error(t, portal.AuthorizeRoute("GET", "/internal/debug", claims, token, nil, time.Now()))
	require.Error(t, portal.AuthorizeRoute("POST", "/api/portal/usage", claims, token, nil, time.Now()))
}
