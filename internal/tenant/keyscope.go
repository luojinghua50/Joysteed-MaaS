// Package tenant carries the row-level isolation primitives for the MaaS
// control plane. It builds on framework/queryscope, which upstream documents as
// "the primitive that wrappers use to push a per-call SQL constraint onto the
// request context for inner stores to consume".
//
// The one rule that matters here: queryscope.FromContext returns nil when no
// scope is present, and a nil scope means NO WHERE CLAUSE — i.e. every row.
// That default is fail-open. Everything in this package exists to make the
// tenant predicate impossible to forget or hand-write.
package tenant

import (
	"context"
	"errors"
	"fmt"

	"github.com/maximhq/bifrost/framework/queryscope"
	"gorm.io/gorm"
)

// ErrMissingTenant is returned when a tenant-level scope is requested without a
// resolved tenant. Callers must treat it as a hard failure, never as "unscoped":
// turning a missing tenant into an unscoped query is exactly the fail-open
// behaviour this package prevents (MAAS_TECH_DESIGN.md R1/R9).
var ErrMissingTenant = errors.New("tenant: no tenant resolved on context; refusing to build an unscoped query")

// KeyColumn is the tenant discriminator added to config_keys by the M1
// migration. NULL means the row belongs to the platform pool.
const KeyColumn = "config_keys.tenant_id"

// KeyScopeFor returns the QueryScope for the config_keys table.
//
// config_keys is the ONLY table whose scope legitimately returns rows the tenant
// does not own: a tenant may use its own BYOK keys AND the platform pool
// (D1 = both modes, tenant_id NULL = platform pool). That makes the predicate
// asymmetric in its failure modes:
//
//	tenant_id = ?                  → tenant cannot reach the platform pool.
//	                                 Feature breaks loudly. Tests catch it.
//	(no predicate at all)          → tenant sees OTHER TENANTS' BYOK keys.
//	                                 Feature works perfectly. Nothing catches it.
//
// The second one is a silent cross-tenant leak, which is why hand-writing this
// predicate is banned: always route through this function.
func KeyScopeFor(tenantID string) (queryscope.QueryScope, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("building config_keys scope: %w", ErrMissingTenant)
	}
	return func(db *gorm.DB) *gorm.DB {
		return db.Where(
			fmt.Sprintf("%s = ? OR %s IS NULL", KeyColumn, KeyColumn),
			tenantID,
		)
	}, nil
}

// StrictScopeFor returns the QueryScope for an ordinary tenant-level table:
// the tenant's own rows and nothing else. Unlike KeyScopeFor there is no
// platform-owned tier, so a NULL tenant_id row is not visible to anyone.
//
// qualifiedColumn must be table-qualified ("governance_virtual_keys.tenant_id").
// Upstream query builders join and alias freely, and an unqualified tenant_id
// is ambiguous the moment a join appears; see the note at
// framework/configstore/prompts.go:283 for the same reasoning applied to ids.
func StrictScopeFor(qualifiedColumn, tenantID string) (queryscope.QueryScope, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("building %s scope: %w", qualifiedColumn, ErrMissingTenant)
	}
	if qualifiedColumn == "" {
		return nil, errors.New("tenant: qualifiedColumn is required")
	}
	return func(db *gorm.DB) *gorm.DB {
		return db.Where(qualifiedColumn+" = ?", tenantID)
	}, nil
}

// withQueryScope installs a scope on ctx. Thin wrapper over queryscope so
// ScopedContext has a single place to change if the upstream key mechanism moves.
func withQueryScope(ctx context.Context, scope queryscope.QueryScope) context.Context {
	return queryscope.WithQueryScope(ctx, scope)
}

// PlatformScope is the explicit opt-out used by background jobs, migrations and
// platform-admin reads that legitimately span tenants.
//
// It returns an identity scope rather than nil on purpose. A nil scope and a
// deliberate platform-wide read are indistinguishable downstream, so "unscoped"
// has to be something a caller states out loud and a reviewer can grep for,
// not something that happens by omission.
func PlatformScope() queryscope.QueryScope {
	return func(db *gorm.DB) *gorm.DB { return db }
}
