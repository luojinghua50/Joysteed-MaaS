// Package governance wraps Bifrost's governance store so requests can be funded
// by a tenant instead of (only) a virtual key.
//
// The wrapper embeds *governance.LocalGovernanceStore rather than reimplementing
// governance.GovernanceStore. That interface carries ~40 methods; reimplementing
// it means recompiling against every upstream addition. Embedding inherits all
// of them and overrides the few that must change — which is what upstream asks
// for at plugins/governance/store.go:186:
//
//	"This is the seam a deployment reimplements to fund requests from something
//	 other than a key. Nothing downstream asks what kind of holder answered."
package governance

import (
	"context"

	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/grant"
	upstream "github.com/maximhq/bifrost/plugins/governance"
)

// LimitHolderTenant is the holder kind for a tenant-funded limit.
//
// grant.LimitHolderKind is an open string type and upstream states nothing
// registers or enumerates the kinds, so a deployment can introduce its own
// without patching framework/grant. Refusals name the holder that ran out, so
// this string surfaces in operator-facing errors.
const LimitHolderTenant grant.LimitHolderKind = "tenant"

// TenantLimits is what the control plane knows about a tenant's funding. The
// wrapper deliberately depends on this narrow interface rather than on a
// concrete store, so the Phase 0 spike and the real M1 store are
// interchangeable.
type TenantLimits interface {
	// BudgetIDs returns the budget rows funding this tenant, or nil if the
	// tenant funds nothing itself.
	BudgetIDs(ctx context.Context, id tenant.ID) []string
	// RateLimitIDs returns the rate limit rows applying to this tenant.
	RateLimitIDs(ctx context.Context, id tenant.ID) []string
	// DisplayName is used only to describe a limit in refusals.
	DisplayName(ctx context.Context, id tenant.ID) string
}

// TenantGovernanceStore funds requests from the tenant above the caller, in
// addition to whatever the embedded store already charges (vk → team → customer).
type TenantGovernanceStore struct {
	*upstream.LocalGovernanceStore
	tenants TenantLimits
}

// Compile-time proof that embedding satisfies the whole upstream interface.
// This single line is the real deliverable of the Phase 0 wrapper spike: if a
// future upstream bump adds a method, this fails to build instead of silently
// falling back to a partial implementation.
var _ upstream.GovernanceStore = (*TenantGovernanceStore)(nil)

// NewTenantGovernanceStore wraps an already-constructed local store.
func NewTenantGovernanceStore(inner *upstream.LocalGovernanceStore, tenants TenantLimits) *TenantGovernanceStore {
	return &TenantGovernanceStore{LocalGovernanceStore: inner, tenants: tenants}
}

// HolderLimits reports what funds a permit's holder, with the tenant's own
// limits appended to the inherited vk/team/customer ones.
//
// Additive rather than replacing: a deployment still wants the key's own budget
// enforced. The tenant is an *additional* payer, so an exhausted tenant budget
// refuses the request even when the key has headroom, and vice versa.
//
// Upstream contract kept intact: this returns limits only, decides nothing about
// exhaustion, and never reports an error — a tenant that funds nothing simply
// adds no limits.
func (s *TenantGovernanceStore) HolderLimits(ctx context.Context, permit schemas.Permit) (budgets []schemas.Limit, rateLimits []schemas.Limit) {
	budgets, rateLimits = s.LocalGovernanceStore.HolderLimits(ctx, permit)

	id := tenant.FromContext(ctx)
	if id == "" || s.tenants == nil {
		// No tenant resolved: behave exactly like the embedded store. Governance
		// is not the place to refuse an unresolved tenant — the transport's
		// pre-auth hook and the scoped config store already do that, and failing
		// here would turn a missing tenant into a confusing budget error.
		return budgets, rateLimits
	}

	name := s.tenants.DisplayName(ctx, id)

	if ids := s.tenants.BudgetIDs(ctx, id); len(ids) > 0 {
		budgets = append(budgets,
			grant.LimitsHeldBy(LimitHolderTenant, string(id), name, "", "", ids...)...)
	}
	if ids := s.tenants.RateLimitIDs(ctx, id); len(ids) > 0 {
		rateLimits = append(rateLimits,
			grant.LimitsHeldBy(LimitHolderTenant, string(id), name, "", "", ids...)...)
	}

	return budgets, rateLimits
}
