package tenant

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// SettingName is the Postgres session variable the RLS policies read.
//
// Exported so the migration that CREATEs the policies and the runtime that
// binds the value cannot drift apart: a policy reading app.tenant_id while the
// runtime sets app.tenant cannot fail loudly — the policy just matches nothing,
// every tenant read comes back empty, and the cause is invisible. One constant,
// referenced by both.
const SettingName = "app.tenant_id"

// PlatformModeSettingName is the Postgres session variable that opts a
// transaction into cross-tenant visibility (D15, design doc §7.4.5).
//
// Exported for the same reason as SettingName: the migration that writes the
// policies and the runtime that sets the value must agree on one name, because
// disagreeing produces no error — the policy's platform branch simply never
// matches, and every platform-level read comes back empty.
//
// Be clear about what this is and is not. It closes the gap where a strict policy
// makes an unbound read return zero rows, which would leave the data plane
// booting with no governance config at all. It is NOT a defence against a stolen
// credential: the application role can set this variable itself, so anyone
// holding that credential can already turn it on. That matches the boundary
// deploy/postgres/01_bootstrap.sql already documents — the table owner can also
// NO FORCE or DROP POLICY — so isolation here holds against missing predicates
// and injection, not against a compromised app credential. What this buys is
// that crossing tenants becomes something a caller states out loud and a reviewer
// can grep for, rather than something that happens by omitting a binding.
const PlatformModeSettingName = "app.platform_mode"

// PlatformModeOn is the only value the policies treat as enabled. Compared by
// exact string equality, so the empty string a committed transaction leaves
// behind on a pooled connection reads as disabled — see RunAcrossTenants.
const PlatformModeOn = "on"

// ErrTenantNotBound reports that the tenant binding did not take effect on the
// connection. Treated as fatal for the operation rather than ignored: an
// unbound connection reads as though no tenant were resolved, which for RLS
// means "no tenant rows" — a silent wrong answer, not an error.
var ErrTenantNotBound = errors.New("tenant: tenant binding did not take effect on this connection")

// ErrPlatformModeNotSet reports that platform mode did not take effect. Fatal for
// the operation for the same reason ErrTenantNotBound is: under a strict policy an
// unset flag reads as zero rows rather than as an error, so a reconciliation or
// backfill pass would conclude there is no work to do.
var ErrPlatformModeNotSet = errors.New("tenant: platform mode did not take effect on this connection")

// ErrPlatformModeWithTenant reports an attempt to enter platform mode on a context
// that already has a tenant resolved. Refused rather than resolved by precedence:
// a caller holding one tenant and asking for all of them has two contradictory
// intentions, and silently honouring either one widens or narrows a request in a
// way the call site did not ask for.
var ErrPlatformModeWithTenant = errors.New("tenant: refusing platform mode on a context that already has a tenant")

// RunInTenantTx runs fn inside a transaction with the context's tenant bound for
// the life of that transaction.
//
// This is the only supported way to read tenant-scoped tables. The shape is
// deliberate on three counts, each closing a failure mode that the spike in
// MAAS_TECH_DESIGN.md §9.2.3 demonstrated is silent:
//
// It owns the transaction rather than accepting one. The binding is
// transaction-scoped (set_config's local=true), and outside an explicit
// transaction every statement is its own implicit one — so the binding would be
// discarded the moment it was set, and every subsequent read would see no
// tenant. Detecting that afterwards is possible; making it unrepresentable is
// better, so this function begins the transaction itself.
//
// It never offers a session-scoped variant. Session-scoped binding
// (local=false) survives on the pooled connection after the request ends, so the
// next request served by that connection inherits the previous tenant's
// identity. That is a cross-tenant read, and it is not a mode worth exposing
// behind a flag.
//
// It fails closed when no tenant is resolved. An unresolved tenant is a bug in
// the resolution chain, and proceeding would run the query with no tenant bound
// — which reads as empty rather than erroring, hiding the bug.
func RunInTenantTx(ctx context.Context, db *gorm.DB, fn func(tx *gorm.DB) error) error {
	id := FromContext(ctx)
	if id == "" {
		return ErrMissingTenant
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := bind(tx, string(id)); err != nil {
			return err
		}
		return fn(tx)
	})
}

// RunAsPlatform runs fn inside a transaction with NO tenant bound, for
// operations that legitimately span tenants: platform-table reads, the
// reconciliation loop, migrations, billing rollups.
//
// It is a separate named function rather than a flag on RunInTenantTx so that
// crossing the isolation boundary is visible at the call site and greppable in
// review. This is not a master key: it is the absence of a tenant filter, and
// under RLS what that yields depends on the table's policy kind (see
// internal/migrate):
//
//	PolicyStrict (B-class)    → NO rows at all. `tenant_id = ''` matches nothing,
//	                            so an unbound read of a tenant table is empty.
//	PolicyPool (config_keys)  → the platform pool only (tenant_id IS NULL).
//	PolicyExclusive (sessions)→ platform-owned rows only (tenant_id IS NULL).
//
// The first line is the one to keep in mind, and it corrects what this comment
// used to claim. "An unbound transaction still sees tenant_id IS NULL rows" holds
// only where the policy names that tier explicitly; on a strict table an unbound
// read returns empty rather than platform-wide. That is fail-closed, but it also
// means this function is NOT a way to read across tenants on B-class tables —
// there is currently no such way, which is an open item (see §7.4.5 of the design
// doc, the GetGovernanceConfig problem).
func RunAsPlatform(ctx context.Context, db *gorm.DB, fn func(tx *gorm.DB) error) error {
	return db.WithContext(ctx).Transaction(fn)
}

// RunAcrossTenants runs fn in a transaction that can see and write EVERY tenant's
// rows, by setting the platform-mode variable the policies check.
//
// This is the widest thing in this package, and it is a third named function
// rather than a flag on RunAsPlatform for the reason RunAsPlatform is itself not a
// flag on RunInTenantTx: the call sites that legitimately cross tenants should be
// greppable, and folding this into RunAsPlatform would silently widen every
// existing unbound call — the reconciliation loop, migrations, platform reads —
// from "sees platform-owned rows" to "sees everything".
//
// Use it for: loading governance config at boot (upstream's GetGovernanceConfig
// does bare multi-table reads with no tenant bound), the reconciliation loop,
// billing rollups, and the backfill migrations, which UPDATE tenant_id and so need
// write access too.
//
// Do not use it to serve a request. If a tenant is resolved on ctx this returns
// ErrPlatformModeWithTenant rather than quietly widening that request's
// visibility: a caller holding a tenant and asking for every tenant is a bug at
// best, and the two intentions cannot be reconciled by picking one.
func RunAcrossTenants(ctx context.Context, db *gorm.DB, fn func(tx *gorm.DB) error) error {
	if id := FromContext(ctx); id != "" {
		return fmt.Errorf("%w: tenant %q is resolved on this context", ErrPlatformModeWithTenant, id)
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := enablePlatformMode(tx); err != nil {
			return err
		}
		return fn(tx)
	})
}

// EnablePlatformMode turns on cross-tenant visibility for a transaction the caller
// already owns.
//
// This exists for one caller shape: code running inside a transaction it did not
// open, which cannot use RunAcrossTenants without nesting. The migration runner is
// that caller — upstream's migrator wraps each migration in its own transaction
// and hands it in (Options.UseTransaction), and the runner has to read across
// tenants to check whether a table's backfill is complete before installing a
// policy over it.
//
// Request paths must use RunAcrossTenants instead. That function's refusal when a
// tenant is already resolved (ErrPlatformModeWithTenant) is the guard against
// widening a request that holds one tenant, and this entry point has no context to
// make that check against. The narrowness is the point: a migration never holds a
// tenant, so there is nothing to contradict.
func EnablePlatformMode(tx *gorm.DB) error {
	return enablePlatformMode(tx)
}

// enablePlatformMode sets the variable transaction-locally and verifies it landed.
//
// local=true is what keeps this from becoming a real master key. A session-scoped
// setting would survive on the pooled connection after the transaction ends, so
// the next ordinary request served by that connection would run with cross-tenant
// visibility — and, unlike a leaked tenant binding, it would not even look wrong
// in a log. Transaction-locally, the residue a committed transaction leaves is the
// empty string, which is not PlatformModeOn, so the flag fails closed on reuse.
func enablePlatformMode(tx *gorm.DB) error {
	if err := tx.Exec(
		`SELECT set_config(?, ?, true)`, PlatformModeSettingName, PlatformModeOn).Error; err != nil {
		return fmt.Errorf("tenant: enable platform mode: %w", err)
	}
	// Read back for the same reason bind does. If this setting is silently absent
	// — wrong name, a pooler that resets state, a caller outside a real
	// transaction — a strict-policy read returns zero rows instead of erroring,
	// and the reconciliation loop concludes there is nothing to do.
	var got string
	if err := tx.Raw(
		`SELECT coalesce(current_setting(?, true), '')`, PlatformModeSettingName).Scan(&got).Error; err != nil {
		return fmt.Errorf("tenant: verify platform mode: %w", err)
	}
	if got != PlatformModeOn {
		return fmt.Errorf("%w: expected %q, connection reports %q",
			ErrPlatformModeNotSet, PlatformModeOn, got)
	}
	return nil
}

// PlatformModeEnabled reports whether tx is in platform mode, for assertions and
// diagnostics.
func PlatformModeEnabled(tx *gorm.DB) (bool, error) {
	var got string
	if err := tx.Raw(
		`SELECT coalesce(current_setting(?, true), '')`, PlatformModeSettingName).Scan(&got).Error; err != nil {
		return false, fmt.Errorf("tenant: read platform mode: %w", err)
	}
	return got == PlatformModeOn, nil
}

// bind sets the tenant for the current transaction and verifies it landed.
//
// set_config is used rather than SET LOCAL because Postgres's SET is utility
// syntax that rejects bind parameters — "syntax error at or near $1",
// SQLSTATE 42601. The only way to write it as SET is to interpolate the tenant
// id into the statement text, which would make tenant binding itself a SQL
// injection sink. set_config is an ordinary function call, so the value travels
// as a real parameter.
func bind(tx *gorm.DB, id string) error {
	if id == "" {
		return ErrMissingTenant
	}
	if err := tx.Exec(
		`SELECT set_config(?, ?, true)`, SettingName, id).Error; err != nil {
		return fmt.Errorf("tenant: bind %q: %w", id, err)
	}

	// Read back, rather than trusting the write. This costs one round trip on a
	// path that already owns a transaction, and it converts the entire class of
	// "binding silently absent" faults — wrong setting name, a pooler that
	// resets state, a caller that reached here outside a real transaction — from
	// a wrong answer into an error. Given that the wrong answer is a
	// cross-tenant or empty read, that trade is worth one round trip.
	var got string
	if err := tx.Raw(
		`SELECT coalesce(current_setting(?, true), '')`, SettingName).Scan(&got).Error; err != nil {
		return fmt.Errorf("tenant: verify binding for %q: %w", id, err)
	}
	if got != id {
		return fmt.Errorf("%w: expected %q, connection reports %q",
			ErrTenantNotBound, id, got)
	}
	return nil
}

// BoundTenant reports the tenant currently bound on tx, for assertions and
// diagnostics. Returns "" when nothing is bound.
func BoundTenant(tx *gorm.DB) (string, error) {
	var got string
	if err := tx.Raw(
		`SELECT coalesce(current_setting(?, true), '')`, SettingName).Scan(&got).Error; err != nil {
		return "", fmt.Errorf("tenant: read bound tenant: %w", err)
	}
	return got, nil
}
