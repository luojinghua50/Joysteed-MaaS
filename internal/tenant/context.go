package tenant

import (
	"context"
	"errors"

	"github.com/luojinghua50/Joysteed-MaaS/internal/tenantid"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/queryscope"
)

// ctxKey is unexported so nothing outside this package can plant a tenant on a
// context. Tenant identity must come from the resolution chains in
// MAAS_TECH_DESIGN.md §3.6 — a VK credential on the data plane, a login session
// on the control plane — and never from a request parameter (§5.3 rule 2).
type ctxKey struct{}

// ID identifies a tenant. A distinct type rather than a bare string so a tenant
// id cannot be passed where a team/customer/vk id is expected.
type ID = tenantid.ID

var ErrTenantConflict = errors.New("tenant: refusing to replace an already resolved tenant")

type platformScopeKey struct{}

// SetResolvedTenant installs a verified identity on the mutable transport context.
// Using the same private key as WithTenant lets it survive plugin scopes and the
// transport's copy into fasthttp user values without accepting a public header.
func SetResolvedTenant(ctx *schemas.BifrostContext, id ID) error {
	if ctx == nil || id == "" {
		return ErrMissingTenant
	}
	if previous := FromContext(ctx); previous != "" && previous != id {
		return ErrTenantConflict
	}
	ctx.SetValue(ctxKey{}, id)
	return nil
}

// WithTenant returns ctx carrying the resolved tenant. An empty id is not
// planted: a caller that failed to resolve a tenant must produce a context that
// still looks unresolved, so downstream fail-closed checks trip.
func WithTenant(ctx context.Context, id ID) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the resolved tenant, or "" when none is present.
//
// Callers must not treat "" as "all tenants". Mirrors queryscope.FromContext's
// shape deliberately, but the interpretation is the opposite: there, absent
// means unrestricted; here, absent means refuse.
func FromContext(ctx context.Context) ID {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(ctxKey{}).(ID); ok {
		return v
	}
	return ""
}

// WithPlatformScope explicitly opts a call into platform-wide query behavior.
// It is intentionally not represented by a nil QueryScope: nil is the upstream
// fail-open default, while this marker is auditable and survives wrapper hops.
func WithPlatformScope(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return queryscope.WithQueryScope(
		context.WithValue(ctx, platformScopeKey{}, true),
		PlatformScope(),
	)
}

func IsPlatformScope(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	ok, _ := ctx.Value(platformScopeKey{}).(bool)
	return ok
}

// ScopedContext resolves the tenant and installs the matching config_keys
// QueryScope in one step, so a caller cannot set one without the other.
func ScopedContext(ctx context.Context, id ID) (context.Context, error) {
	scope, err := KeyScopeFor(string(id))
	if err != nil {
		return nil, err
	}
	return withQueryScope(WithTenant(ctx, id), scope), nil
}
