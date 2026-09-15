package migrate

import (
	"fmt"

	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"gorm.io/gorm"
)

// PolicyName is the single policy installed per table. One name, so the installer
// is idempotent (DROP IF EXISTS then CREATE) and so rls.Enforce's pg_policies
// lookup has something predictable to find.
const PolicyName = "tenant_isolation"

// PolicyKind selects the predicate shape. There are exactly two, and which one a
// table gets is a data-model fact from MAAS_TABLE_AUDIT.md, not a tuning choice.
type PolicyKind int

const (
	// PolicyStrict is for B-class tables: the tenant's own rows and nothing else.
	// A NULL tenant_id row is visible to NO tenant under this policy — which is
	// why the NOT NULL step matters, and why an unattributed row is invisible
	// rather than dangerous.
	PolicyStrict PolicyKind = iota

	// PolicyPool is for config_keys alone (C-class, D1 dual mode): the tenant's
	// own BYOK keys plus the shared platform pool where tenant_id IS NULL.
	PolicyPool

	// PolicyExclusive is for tables holding BOTH tenant-owned and platform-owned
	// rows that must not see each other — `sessions` under shared deployment (D4)
	// being the case that forces it into existence.
	//
	// Neither other kind works there. Under PolicyStrict a platform-admin session
	// row (tenant_id IS NULL) is invisible to everyone including an unbound
	// transaction, because `tenant_id = NULL` evaluates to NULL and never to
	// true — so platform admins could not log in. Under PolicyPool every tenant
	// could read every platform-admin session, which is the auth boundary R16
	// asks for, inverted.
	//
	// This kind makes the two tiers mutually exclusive: bound transactions see
	// only that tenant's rows, unbound ones see only platform-owned rows.
	PolicyExclusive
)

func (k PolicyKind) String() string {
	switch k {
	case PolicyStrict:
		return "strict"
	case PolicyPool:
		return "pool"
	case PolicyExclusive:
		return "exclusive"
	}
	return fmt.Sprintf("PolicyKind(%d)", int(k))
}

// InstallPolicy enables, forces and defines RLS on one table.
//
// FORCE is not optional. The runtime role owns the tables it created (Bifrost
// runs its own migrations with the same credential it serves traffic with), and a
// table owner is exempt from its own table's RLS unless FORCE is set. Without it
// the policies exist, are visible in pg_policies, and filter nothing — which is
// precisely the silent failure rls.Enforce's table.not_forced check exists to
// catch.
//
// Idempotent by DROP-then-CREATE rather than CREATE ... IF NOT EXISTS, which
// Postgres does not offer for policies. That also means a changed predicate is
// actually applied on re-run instead of being skipped as "already present".
func InstallPolicy(tx *gorm.DB, table string, kind PolicyKind) error {
	if err := checkIdentifier(table); err != nil {
		return err
	}
	using, withCheck, err := policyClauses(kind)
	if err != nil {
		return err
	}

	stmts := []string{
		fmt.Sprintf(`ALTER TABLE %s ENABLE ROW LEVEL SECURITY`, table),
		fmt.Sprintf(`ALTER TABLE %s FORCE ROW LEVEL SECURITY`, table),
		fmt.Sprintf(`DROP POLICY IF EXISTS %s ON %s`, PolicyName, table),
		fmt.Sprintf(`CREATE POLICY %s ON %s FOR ALL USING (%s) WITH CHECK (%s)`,
			PolicyName, table, using, withCheck),
	}
	for _, s := range stmts {
		if err := tx.Exec(s).Error; err != nil {
			return fmt.Errorf("migrate: install %s policy on %s (%q): %w", kind, table, s, err)
		}
	}
	return nil
}

// policyClauses returns the USING and WITH CHECK predicates.
//
// The two are returned separately, and for PolicyPool they deliberately DIFFER.
// This corrects the spike's policy (internal/rlsspike/spike.go), which wrote
// `FOR ALL USING (tenant_id IS NULL OR tenant_id = current_setting(...))` with no
// WITH CHECK. Postgres defaults WITH CHECK to the USING expression, so that
// policy lets a tenant INSERT a row with tenant_id NULL — writing its own key
// into the shared platform pool, where every other tenant can then read it. The
// spike only exercised reads, so its tests could not see this.
//
// Reading the platform pool is the feature (D1). Writing into it is not.
func policyClauses(kind PolicyKind) (using, withCheck string, err error) {
	// current is the bound tenant, normalised so that "unset" has exactly one
	// representation.
	//
	// The coalesce is load-bearing, and NOT defensive dressing. A
	// transaction-local set_config does not restore the variable to NULL when the
	// transaction commits — it leaves the EMPTY STRING on the connection, which
	// database/sql then returns to the pool. Measured on PG16:
	//
	//	BEGIN; SELECT set_config('app.probe','t-a',true); COMMIT;
	//	SELECT current_setting('app.probe', true) IS NULL;  -- f, value is ''
	//
	// So a predicate testing `current_setting(...) IS NULL` reads "no tenant
	// bound" correctly only on a connection that has never served a tenant. On a
	// reused one it reads false, and the failure is invisible: it depends on which
	// pooled connection the request happened to get. Everything below therefore
	// compares against '' rather than NULL.
	current := fmt.Sprintf("coalesce(current_setting('%s', true), '')", tenant.SettingName)

	// bound requires a non-empty tenant, so a stale-empty connection can never
	// match a row — not even one whose tenant_id is itself the empty string.
	bound := fmt.Sprintf("(%s <> '' AND %s = %s)", current, TenantColumn, current)

	// unbound is the complement, on the same normalised value.
	unbound := fmt.Sprintf("%s = ''", current)

	// platform is the D15 escape hatch (design doc §7.4.5): cross-tenant
	// visibility for the reads that legitimately span tenants — governance config
	// at boot, reconciliation, billing rollups, and the backfill migrations.
	//
	// Two properties are deliberate. It requires `unbound`, so a transaction that
	// has a tenant bound is governed by that tenant's predicate no matter what the
	// flag says — the contradictory combination resolves to the NARROWER reading,
	// not the wider one. And it compares against an exact value rather than testing
	// for non-emptiness, so the empty string a committed transaction leaves on a
	// pooled connection reads as disabled.
	platform := fmt.Sprintf("(%s AND coalesce(current_setting('%s', true), '') = '%s')",
		unbound, tenant.PlatformModeSettingName, tenant.PlatformModeOn)

	switch kind {
	case PolicyStrict:
		// WITH CHECK carries the platform disjunct too, because backfill is an
		// UPDATE of tenant_id: a read-only escape hatch could find the
		// unattributed rows but not fix them.
		return fmt.Sprintf("%s OR %s", platform, bound),
			fmt.Sprintf("%s OR %s", platform, bound), nil
	case PolicyPool:
		return fmt.Sprintf("%s OR %s IS NULL OR %s", platform, TenantColumn, bound),
			fmt.Sprintf("%s OR %s", platform, bound), nil
	case PolicyExclusive:
		// The two tiers partition the table. A bound transaction matches only its
		// own rows; an unbound one matches only platform-owned rows. Note this is
		// NOT PolicyPool with extra steps: there, platform rows are visible to
		// everyone; here, they are visible only when no tenant is bound.
		//
		// WITH CHECK is the same expression rather than the stricter `bound`, and
		// that is deliberate: the platform genuinely has to INSERT its own
		// sessions on admin login, which happens on an unbound transaction. The
		// asymmetry that PolicyPool needs (read the pool, never write to it) does
		// not apply, because here writing a platform row requires already being
		// unbound — i.e. already being the platform.
		// The platform disjunct comes FIRST, and it has to, because the CASE below
		// is exhaustive: without it, an unbound platform-mode transaction would
		// take the `unbound` branch and see only tenant_id IS NULL rows. That is
		// right for the login path but wrong for the session reaper, which has to
		// reach every tenant's expired sessions.
		expr := fmt.Sprintf("%s OR CASE WHEN %s THEN %s IS NULL ELSE %s END",
			platform, unbound, TenantColumn, bound)
		return expr, expr, nil
	}
	return "", "", fmt.Errorf("migrate: unknown policy kind %d", int(kind))
}

// RemovePolicy reverses InstallPolicy.
//
// NO FORCE and DISABLE are both issued: leaving RLS enabled on a table whose
// policy has been dropped means zero rows are visible to non-owners, so a partial
// rollback would look like total data loss to the application.
func RemovePolicy(tx *gorm.DB, table string) error {
	if err := checkIdentifier(table); err != nil {
		return err
	}
	for _, s := range []string{
		fmt.Sprintf(`DROP POLICY IF EXISTS %s ON %s`, PolicyName, table),
		fmt.Sprintf(`ALTER TABLE %s NO FORCE ROW LEVEL SECURITY`, table),
		fmt.Sprintf(`ALTER TABLE %s DISABLE ROW LEVEL SECURITY`, table),
	} {
		if err := tx.Exec(s).Error; err != nil {
			return fmt.Errorf("migrate: remove policy from %s (%q): %w", table, s, err)
		}
	}
	return nil
}
