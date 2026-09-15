package portal

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/rbac"
)

var ErrRouteDenied = errors.New("portal: route denied")

// AuthorizeRoute is a framework-neutral route guard. Unknown paths and all
// routes outside /api/admin and /api/portal are denied by default. Mutations
// require a valid server-side session and constant-time CSRF verification.
func AuthorizeRoute(method, path string, claims rbac.SessionClaims, csrfToken string, store *rbac.Store, now time.Time) error {
	if store == nil || !claims.Valid(now) || strings.TrimSpace(path) == "" {
		return ErrRouteDenied
	}
	var permission rbac.Permission
	switch {
	case strings.HasPrefix(path, "/api/admin/") && claims.Principal.Type == rbac.PrincipalPlatformAdmin:
		permission = adminPermission(method, path)
	case strings.HasPrefix(path, "/api/portal/") && claims.Principal.Type == rbac.PrincipalTenantUser:
		permission = portalPermission(method, path)
	default:
		return ErrRouteDenied
	}
	if permission == "" {
		return ErrRouteDenied
	}
	ctx, err := rbac.WithPrincipal(nil, claims.Principal)
	if err != nil {
		return ErrRouteDenied
	}
	if err := store.Require(ctx, permission); err != nil {
		return err
	}
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions && !claims.VerifyCSRF(csrfToken) {
		return rbac.ErrForbidden
	}
	return nil
}

func adminPermission(method, path string) rbac.Permission {
	if strings.Contains(path, "/audit") {
		if method == http.MethodGet {
			return rbac.PermissionAuditRead
		}
		return rbac.PermissionAuditExport
	}
	if strings.Contains(path, "/tenants") {
		if method == http.MethodGet {
			return rbac.PermissionTenantRead
		}
		return rbac.PermissionTenantManage
	}
	if strings.Contains(path, "/billing") {
		if method == http.MethodGet {
			return rbac.PermissionBillingRead
		}
		return rbac.PermissionBillingManage
	}
	if strings.Contains(path, "/policy") {
		return rbac.PermissionPolicyManage
	}
	return ""
}
func portalPermission(method, path string) rbac.Permission {
	if strings.Contains(path, "/audit") {
		return rbac.PermissionAuditRead
	}
	if strings.Contains(path, "/usage") || strings.Contains(path, "/billing") {
		if method == http.MethodGet {
			return rbac.PermissionBillingRead
		}
		return rbac.PermissionBillingManage
	}
	if strings.Contains(path, "/budget") || strings.Contains(path, "/rate-limit") {
		return rbac.PermissionBudgetManage
	}
	if strings.Contains(path, "/members") {
		return rbac.PermissionMemberManage
	}
	if strings.Contains(path, "/keys") {
		return rbac.PermissionKeyManage
	}
	return ""
}
