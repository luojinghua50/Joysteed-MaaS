package governance_test

import (
	"context"
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/governance"
	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	configstoreTables "github.com/maximhq/bifrost/framework/configstore/tables"
	"github.com/maximhq/bifrost/framework/grant"
	upstream "github.com/maximhq/bifrost/plugins/governance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	vkID       = "vk-0001"
	vkBudgetID = "budget-vk-0001"
	tenantID   = tenant.ID("tenant-aaaa")
)

// vkPermit is the minimal schemas.Permit that resolves to a virtual key.
// virtualKeyOf accepts a permit whose Type() is "vk" and whose ID() names a
// known key; nothing else on the interface is consulted by HolderLimits.
type vkPermit struct{ id string }

func (p vkPermit) Type() string                              { return string(grant.PermitVirtualKey) }
func (p vkPermit) ID() string                                { return p.id }
func (p vkPermit) Name() string                              { return "test-vk" }
func (p vkPermit) IsActive() bool                            { return true }
func (p vkPermit) IsExpired() bool                           { return false }
func (p vkPermit) ProviderPermits() []schemas.ProviderPermit { return nil }
func (p vkPermit) MCPPermits() []schemas.MCPPermit           { return nil }
func (p vkPermit) AllowsAllProviders() bool                  { return true }

// stubTenants is the Phase 0 stand-in for the M1 tenant store.
type stubTenants struct {
	budgets    map[tenant.ID][]string
	rateLimits map[tenant.ID][]string
}

func (s stubTenants) BudgetIDs(_ context.Context, id tenant.ID) []string    { return s.budgets[id] }
func (s stubTenants) RateLimitIDs(_ context.Context, id tenant.ID) []string { return s.rateLimits[id] }
func (s stubTenants) DisplayName(_ context.Context, id tenant.ID) string    { return "Acme Corp" }

// newStore builds a real LocalGovernanceStore from in-memory config (configStore
// nil → loadFromConfigMemory), seeded with one VK that funds itself.
func newStore(t *testing.T, tenants governance.TenantLimits) *governance.TenantGovernanceStore {
	t.Helper()
	rateLimitID := "ratelimit-vk-0001"
	cfg := &configstore.GovernanceConfig{
		VirtualKeys: []configstoreTables.TableVirtualKey{{
			ID:          vkID,
			Name:        "test-vk",
			Value:       schemas.SecretVar{Val: "vk-secret-value"},
			RateLimitID: &rateLimitID,
			Budgets:     []configstoreTables.TableBudget{{ID: vkBudgetID, MaxLimit: 100, ResetDuration: "1M"}},
		}},
		RateLimits: []configstoreTables.TableRateLimit{{ID: rateLimitID}},
		Budgets:    []configstoreTables.TableBudget{{ID: vkBudgetID, MaxLimit: 100, ResetDuration: "1M"}},
	}

	inner, err := upstream.NewLocalGovernanceStore(
		context.Background(),
		bifrost.NewDefaultLogger(schemas.LogLevelError),
		nil, // no configStore → in-memory config path
		cfg,
		nil, // no model catalog
		nil, // no InMemoryStore
	)
	require.NoError(t, err)
	return governance.NewTenantGovernanceStore(inner, tenants)
}

func holderKinds(limits []schemas.Limit) []string {
	out := make([]string, 0, len(limits))
	for _, l := range limits {
		out = append(out, l.HolderKind)
	}
	return out
}

// The baseline: with no tenant on the context the wrapper must be
// indistinguishable from the embedded store. This is what makes the wrapper safe
// to deploy before tenant resolution is wired up.
func TestHolderLimits_NoTenant_MatchesEmbedded(t *testing.T) {
	store := newStore(t, stubTenants{
		budgets: map[tenant.ID][]string{tenantID: {"budget-tenant-1"}},
	})
	permit := vkPermit{id: vkID}

	wrapped, wrappedRL := store.HolderLimits(context.Background(), permit)
	inner, innerRL := store.LocalGovernanceStore.HolderLimits(context.Background(), permit)

	assert.Equal(t, inner, wrapped, "no tenant on ctx must yield exactly the embedded store's budgets")
	assert.Equal(t, innerRL, wrappedRL, "same for rate limits")
	assert.NotContains(t, holderKinds(wrapped), string(governance.LimitHolderTenant))
}

// The actual Phase 0 goal: a tenant on the context funds the request in addition
// to the key.
func TestHolderLimits_TenantAppendsItsOwnLimits(t *testing.T) {
	store := newStore(t, stubTenants{
		budgets:    map[tenant.ID][]string{tenantID: {"budget-tenant-1", "budget-tenant-2"}},
		rateLimits: map[tenant.ID][]string{tenantID: {"ratelimit-tenant-1"}},
	})
	permit := vkPermit{id: vkID}
	ctx := tenant.WithTenant(context.Background(), tenantID)

	budgets, rateLimits := store.HolderLimits(ctx, permit)

	// The key's own budget survives: the tenant is an additional payer, not a
	// replacement. An exhausted tenant budget refuses even when the key has
	// headroom, and vice versa.
	assert.Contains(t, holderKinds(budgets), string(grant.LimitHolderVirtualKey),
		"the key's own limits must still be charged")

	var tenantBudgets []schemas.Limit
	for _, l := range budgets {
		if l.HolderKind == string(governance.LimitHolderTenant) {
			tenantBudgets = append(tenantBudgets, l)
		}
	}
	require.Len(t, tenantBudgets, 2, "both tenant budgets must be gathered")
	assert.Equal(t, string(tenantID), tenantBudgets[0].HolderID)
	assert.Equal(t, "Acme Corp", tenantBudgets[0].HolderName,
		"HolderName is what a refusal shows an operator")
	assert.ElementsMatch(t,
		[]string{"budget-tenant-1", "budget-tenant-2"},
		[]string{tenantBudgets[0].ID, tenantBudgets[1].ID})

	assert.Contains(t, holderKinds(rateLimits), string(governance.LimitHolderTenant))
}

// A tenant that funds nothing itself adds nothing. Covers the platform-pool-only
// tenant whose spending is governed entirely by its keys.
func TestHolderLimits_TenantFundingNothing_AddsNothing(t *testing.T) {
	store := newStore(t, stubTenants{})
	permit := vkPermit{id: vkID}

	withTenant, _ := store.HolderLimits(tenant.WithTenant(context.Background(), tenantID), permit)
	without, _ := store.HolderLimits(context.Background(), permit)

	assert.Equal(t, without, withTenant)
}

// A nil TenantLimits must degrade to the embedded behaviour rather than panic:
// the wrapper gets constructed before the tenant store exists during rollout.
func TestHolderLimits_NilTenantProvider_DoesNotPanic(t *testing.T) {
	store := newStore(t, nil)
	permit := vkPermit{id: vkID}
	ctx := tenant.WithTenant(context.Background(), tenantID)

	assert.NotPanics(t, func() {
		budgets, _ := store.HolderLimits(ctx, permit)
		assert.NotContains(t, holderKinds(budgets), string(governance.LimitHolderTenant))
	})
}

// An unknown permit resolves to no holder in the embedded store, yet the tenant
// still funds the request.
//
// This is behaviour by construction, not a bug: tenant funding is keyed off the
// context, not off the permit, so it survives a permit nothing can resolve. The
// consequence is that a request presenting an unresolvable key is still billed to
// its tenant. Whether that is what a deployment wants is an M1 policy question
// (see D9 in MAAS_TECH_DESIGN.md) — pinned here so any future change to it is
// deliberate rather than incidental.
func TestHolderLimits_UnknownPermit_TenantStillFunds(t *testing.T) {
	store := newStore(t, stubTenants{
		budgets:    map[tenant.ID][]string{tenantID: {"budget-tenant-1"}},
		rateLimits: map[tenant.ID][]string{tenantID: {"ratelimit-tenant-1"}},
	})
	ctx := tenant.WithTenant(context.Background(), tenantID)

	// Establish the premise: the embedded store finds no holder whatsoever.
	innerBudgets, innerRL := store.LocalGovernanceStore.HolderLimits(ctx, vkPermit{id: "vk-does-not-exist"})
	require.Empty(t, innerBudgets, "embedded store must resolve no holder for an unknown key")
	require.Empty(t, innerRL)

	budgets, rateLimits := store.HolderLimits(ctx, vkPermit{id: "vk-does-not-exist"})

	assert.Equal(t, []string{string(governance.LimitHolderTenant)}, holderKinds(budgets),
		"tenant funding is ctx-keyed, so it survives an unresolvable permit")
	assert.Equal(t, []string{string(governance.LimitHolderTenant)}, holderKinds(rateLimits))
}

// Belt-and-braces companion to the compile-time assertion in tenantstore.go:
// confirms the wrapper is usable through the upstream interface, so a caller
// holding a governance.GovernanceStore gets the overridden HolderLimits.
func TestWrapper_SatisfiesUpstreamInterfaceAtRuntime(t *testing.T) {
	store := newStore(t, stubTenants{
		budgets: map[tenant.ID][]string{tenantID: {"budget-tenant-1"}},
	})

	var iface upstream.GovernanceStore = store
	budgets, _ := iface.HolderLimits(tenant.WithTenant(context.Background(), tenantID), vkPermit{id: vkID})

	assert.Contains(t, holderKinds(budgets), string(governance.LimitHolderTenant),
		"the override must be reached through the interface, not shadowed by the embed")
}
