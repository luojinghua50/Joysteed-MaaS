package migrate_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/migrate"
	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type seedRow = struct {
	tenantID *string
	label    string
}

func ptr(s string) *string { return &s }

// TestAddTenantColumn_IsIdempotent covers the rolling-deploy case the raw DDL was
// written for: two runners can both see the column missing, and both ADDs must
// succeed rather than the loser aborting its transaction (SQLSTATE 42701).
func TestAddTenantColumn_IsIdempotent(t *testing.T) {
	owner, _ := fixture(t, "mig_idem", "")
	require.NoError(t, migrate.AddTenantColumn(owner, "mig_idem"))
	require.NoError(t, migrate.AddTenantColumn(owner, "mig_idem"))

	var n int64
	require.NoError(t, owner.Raw(`SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'mig_idem' AND column_name = 'tenant_id'`).Scan(&n).Error)
	require.Equal(t, int64(1), n)
}

func TestAddTenantColumn_RejectsUnsafeIdentifier(t *testing.T) {
	owner, _ := fixture(t, "mig_safe", "")
	for _, bad := range []string{
		`mig_safe; DROP TABLE mig_safe; --`,
		`"mig_safe"`,
		`mig safe`,
		"MigSafe",
		"",
	} {
		require.Error(t, migrate.AddTenantColumn(owner, bad), bad)
	}
	// The table is still there, and still has no tenant_id.
	var n int64
	require.NoError(t, owner.Raw(`SELECT count(*) FROM information_schema.tables
		WHERE table_name = 'mig_safe'`).Scan(&n).Error)
	require.Equal(t, int64(1), n)
}

// TestPolicyStrict_ThreePartyIsolation is the B-class acceptance test: tenant A
// sees its own rows and nothing else — not tenant B's, and not the unattributed
// row either.
func TestPolicyStrict_ThreePartyIsolation(t *testing.T) {
	const table = "mig_strict"
	owner, app := fixture(t, table, "")
	require.NoError(t, migrate.AddTenantColumn(owner, table))
	require.NoError(t, migrate.InstallPolicy(owner, table, migrate.PolicyStrict))
	seed(t, owner, table, []seedRow{
		{ptr("tenant-a"), "a-row"},
		{ptr("tenant-b"), "b-row"},
		{nil, "unattributed-row"},
	})

	require.Equal(t, []string{"a-row"}, labelsVisibleTo(t, app, table, "tenant-a"))
	require.Equal(t, []string{"b-row"}, labelsVisibleTo(t, app, table, "tenant-b"))

	// An unattributed row is visible to NO tenant, and not to the platform path
	// either. That is the fail-closed direction — invisible, not shared — and it
	// is why SetTenantColumnNotNull matters: invisible rows are still wrong.
	require.Empty(t, labelsVisibleToPlatform(t, app, table))
}

// TestPolicyStrict_CannotWriteAnotherTenantsRow: the WITH CHECK side. A tenant
// must not be able to plant a row attributed to someone else, which a
// USING-only policy would permit on INSERT.
func TestPolicyStrict_CannotWriteAnotherTenantsRow(t *testing.T) {
	const table = "mig_strict_write"
	owner, app := fixture(t, table, "")
	require.NoError(t, migrate.AddTenantColumn(owner, table))
	require.NoError(t, migrate.InstallPolicy(owner, table, migrate.PolicyStrict))

	ctx := tenant.WithTenant(context.Background(), "tenant-a")
	err := tenant.RunInTenantTx(ctx, app, func(tx *gorm.DB) error {
		return tx.Exec(fmt.Sprintf(
			`INSERT INTO %s (tenant_id, label) VALUES (?, ?)`, table),
			"tenant-b", "planted").Error
	})
	require.Error(t, err, "tenant-a must not be able to insert a tenant-b row")

	// Its own row is fine.
	require.NoError(t, tenant.RunInTenantTx(ctx, app, func(tx *gorm.DB) error {
		return tx.Exec(fmt.Sprintf(
			`INSERT INTO %s (tenant_id, label) VALUES (?, ?)`, table),
			"tenant-a", "mine").Error
	}))
	require.Equal(t, []string{"mine"}, labelsVisibleTo(t, app, table, "tenant-a"))
}

// TestPolicyPool_TenantReadsPlatformPool is config_keys' reason for existing
// (D1): a tenant uses its own BYOK keys AND the shared platform pool.
func TestPolicyPool_TenantReadsPlatformPool(t *testing.T) {
	const table = "mig_pool"
	owner, app := fixture(t, table, "")
	require.NoError(t, migrate.AddTenantColumn(owner, table))
	require.NoError(t, migrate.InstallPolicy(owner, table, migrate.PolicyPool))
	seed(t, owner, table, []seedRow{
		{nil, "platform-pool-key"},
		{ptr("tenant-a"), "a-byok"},
		{ptr("tenant-b"), "b-byok"},
	})

	require.Equal(t, []string{"a-byok", "platform-pool-key"},
		labelsVisibleTo(t, app, table, "tenant-a"))
	require.Equal(t, []string{"b-byok", "platform-pool-key"},
		labelsVisibleTo(t, app, table, "tenant-b"))
}

// TestPolicyPool_TenantCannotWriteIntoPlatformPool is the fix this package makes
// to the spike's policy.
//
// The spike wrote `FOR ALL USING (tenant_id IS NULL OR tenant_id = ...)` with no
// WITH CHECK clause. Postgres defaults WITH CHECK to the USING expression, so
// that policy permits INSERTing a tenant_id NULL row — a tenant writing its own
// key into the shared pool, where every other tenant then reads it. The spike
// only exercised reads, so its nine tests could not observe this.
//
// The second half of the test demonstrates the counterfactual directly: the same
// insert IS accepted under the spike's exact predicate.
func TestPolicyPool_TenantCannotWriteIntoPlatformPool(t *testing.T) {
	const table = "mig_pool_write"
	owner, app := fixture(t, table, "")
	require.NoError(t, migrate.AddTenantColumn(owner, table))
	require.NoError(t, migrate.InstallPolicy(owner, table, migrate.PolicyPool))

	ctx := tenant.WithTenant(context.Background(), "tenant-a")
	insertIntoPool := func() error {
		return tenant.RunInTenantTx(ctx, app, func(tx *gorm.DB) error {
			return tx.Exec(fmt.Sprintf(
				`INSERT INTO %s (tenant_id, label) VALUES (NULL, ?)`, table),
				"smuggled-into-pool").Error
		})
	}
	require.Error(t, insertIntoPool(),
		"a tenant must not be able to write into the shared platform pool")

	// Counterfactual: reinstall the spike's predicate (USING only, WITH CHECK
	// defaulted) and the same statement succeeds. This is what pins the fix as
	// load-bearing rather than decorative.
	require.NoError(t, owner.Exec(fmt.Sprintf(
		`DROP POLICY IF EXISTS %s ON %s`, migrate.PolicyName, table)).Error)
	require.NoError(t, owner.Exec(fmt.Sprintf(
		`CREATE POLICY %s ON %s FOR ALL USING (
			tenant_id IS NULL OR tenant_id = current_setting('%s', true))`,
		migrate.PolicyName, table, tenant.SettingName)).Error)

	require.NoError(t, insertIntoPool(),
		"the spike's USING-only predicate accepts the smuggled row — this is the hole")
	require.Contains(t, labelsVisibleTo(t, app, table, "tenant-b"), "smuggled-into-pool",
		"and every other tenant can then read it")
}

// TestPolicyExclusive_PartitionsPlatformAndTenantRows covers the sessions shape.
// The two tiers must be mutually invisible in BOTH directions, which neither
// other policy kind achieves.
func TestPolicyExclusive_PartitionsPlatformAndTenantRows(t *testing.T) {
	const table = "mig_exclusive"
	owner, app := fixture(t, table, "")
	require.NoError(t, migrate.AddTenantColumn(owner, table))
	require.NoError(t, migrate.InstallPolicy(owner, table, migrate.PolicyExclusive))
	seed(t, owner, table, []seedRow{
		{nil, "platform-admin-session"},
		{ptr("tenant-a"), "a-user-session"},
		{ptr("tenant-b"), "b-user-session"},
	})

	// A tenant sees only its own sessions — crucially NOT the admin session,
	// which PolicyPool would have exposed.
	require.Equal(t, []string{"a-user-session"}, labelsVisibleTo(t, app, table, "tenant-a"))

	// The platform sees only platform sessions. Under PolicyStrict this would be
	// empty and admin login would be impossible.
	require.Equal(t, []string{"platform-admin-session"}, labelsVisibleToPlatform(t, app, table))
}

// TestPolicyExclusive_PlatformCanInsertItsOwnSession: admin login inserts a
// session on an unbound transaction, so WITH CHECK must permit it.
func TestPolicyExclusive_PlatformCanInsertItsOwnSession(t *testing.T) {
	const table = "mig_exclusive_write"
	owner, app := fixture(t, table, "")
	require.NoError(t, migrate.AddTenantColumn(owner, table))
	require.NoError(t, migrate.InstallPolicy(owner, table, migrate.PolicyExclusive))

	require.NoError(t, tenant.RunAsPlatform(context.Background(), app, func(tx *gorm.DB) error {
		return tx.Exec(fmt.Sprintf(
			`INSERT INTO %s (tenant_id, label) VALUES (NULL, ?)`, table),
			"admin-login").Error
	}))
	require.Equal(t, []string{"admin-login"}, labelsVisibleToPlatform(t, app, table))

	// And a bound tenant still cannot forge a platform row.
	ctx := tenant.WithTenant(context.Background(), "tenant-a")
	require.Error(t, tenant.RunInTenantTx(ctx, app, func(tx *gorm.DB) error {
		return tx.Exec(fmt.Sprintf(
			`INSERT INTO %s (tenant_id, label) VALUES (NULL, ?)`, table),
			"forged-admin").Error
	}))
}

// TestPolicyExclusive_PlatformReadSurvivesAPooledConnectionThatServedATenant is
// the regression test for a bug this package shipped and then fixed.
//
// A transaction-local set_config does NOT restore the variable to NULL on commit
// — it leaves the empty string on the connection, which then goes back to the
// pool. Measured on PG16:
//
//	BEGIN; SELECT set_config('app.tenant_id','t-a',true); COMMIT;
//	SELECT current_setting('app.tenant_id', true) IS NULL;  -- f, the value is ''
//
// The first PolicyExclusive predicate tested `current_setting(...) IS NULL` for
// "no tenant bound". That reads correctly only on a connection that has never
// served a tenant; on a reused one it takes the bound branch and filters on
// tenant_id = ”, which matches nothing. The effect: platform-admin login works
// on a cold node and silently returns zero sessions once that connection has
// served one tenant request.
//
// maxConns=1 forces the reuse, so this does not depend on which connection the
// pool happens to hand out — the original failure was found only by accident of
// test ordering, which is not a property to rely on.
func TestPolicyExclusive_PlatformReadSurvivesAPooledConnectionThatServedATenant(t *testing.T) {
	const table = "mig_exclusive_pooled"
	owner, _ := fixture(t, table, "")
	require.NoError(t, migrate.AddTenantColumn(owner, table))
	require.NoError(t, migrate.InstallPolicy(owner, table, migrate.PolicyExclusive))
	seed(t, owner, table, []seedRow{
		{nil, "platform-admin-session"},
		{ptr("tenant-a"), "a-user-session"},
	})

	app := openWithConns(t, appRole, appPass, 1)

	// Dirty the one connection in the pool: a committed tenant transaction leaves
	// '' behind on it.
	ctx := tenant.WithTenant(context.Background(), "tenant-a")
	var tenantLabels []string
	require.NoError(t, tenant.RunInTenantTx(ctx, app, func(tx *gorm.DB) error {
		return tx.Raw(fmt.Sprintf(`SELECT label FROM %s`, table)).Scan(&tenantLabels).Error
	}))
	require.Equal(t, []string{"a-user-session"}, tenantLabels)

	// Confirm the residue is there, so this test cannot pass by the residue
	// silently ceasing to exist (e.g. a driver that resets the session): if that
	// day comes, this assertion fails and says so, rather than the test quietly
	// no longer covering anything.
	var isNull bool
	require.NoError(t, tenant.RunAsPlatform(context.Background(), app, func(tx *gorm.DB) error {
		return tx.Raw(fmt.Sprintf(
			`SELECT current_setting('%s', true) IS NULL`, tenant.SettingName)).Scan(&isNull).Error
	}))
	require.False(t, isNull,
		"precondition: the committed tenant tx must have left '' on this connection")

	// The platform read must still work on that same dirty connection.
	require.Equal(t, []string{"platform-admin-session"},
		labelsVisibleToPlatform(t, app, table),
		"platform-admin login must not depend on getting a connection that never served a tenant")
}

// TestPolicyStrict_StaleEmptySettingMatchesNoRows is the same residue seen from
// the strict side. An empty tenant must not match any row — including, in
// particular, a row whose tenant_id is itself the empty string, which is the one
// value a stale connection would otherwise authorise.
func TestPolicyStrict_StaleEmptySettingMatchesNoRows(t *testing.T) {
	const table = "mig_strict_stale"
	owner, _ := fixture(t, table, "")
	require.NoError(t, migrate.AddTenantColumn(owner, table))
	require.NoError(t, migrate.InstallPolicy(owner, table, migrate.PolicyStrict))
	seed(t, owner, table, []seedRow{
		{ptr("tenant-a"), "a-row"},
		{ptr(""), "empty-tenant-row"},
	})

	app := openWithConns(t, appRole, appPass, 1)
	ctx := tenant.WithTenant(context.Background(), "tenant-a")
	require.NoError(t, tenant.RunInTenantTx(ctx, app, func(tx *gorm.DB) error { return nil }))

	require.Empty(t, labelsVisibleToPlatform(t, app, table),
		"a stale empty setting must not authorise the tenant_id = '' row")
}

// TestInstallPolicy_IsIdempotentAndAppliesChanges: policies have no CREATE ... IF
// NOT EXISTS, so the installer drops and recreates. That must be re-runnable, and
// a changed predicate must actually take effect rather than be skipped.
func TestInstallPolicy_IsIdempotentAndAppliesChanges(t *testing.T) {
	const table = "mig_reinstall"
	owner, app := fixture(t, table, "")
	require.NoError(t, migrate.AddTenantColumn(owner, table))
	require.NoError(t, migrate.InstallPolicy(owner, table, migrate.PolicyStrict))
	require.NoError(t, migrate.InstallPolicy(owner, table, migrate.PolicyStrict))
	seed(t, owner, table, []seedRow{{nil, "platform-row"}, {ptr("tenant-a"), "a-row"}})

	require.Empty(t, labelsVisibleToPlatform(t, app, table), "strict: platform sees nothing")

	// Switch kinds; the new predicate must win.
	require.NoError(t, migrate.InstallPolicy(owner, table, migrate.PolicyPool))
	require.Equal(t, []string{"a-row", "platform-row"},
		labelsVisibleTo(t, app, table, "tenant-a"), "pool: tenant now also sees the pool")

	var n int64
	require.NoError(t, owner.Raw(
		`SELECT count(*) FROM pg_policies WHERE tablename = ? AND policyname = ?`,
		table, migrate.PolicyName).Scan(&n).Error)
	require.Equal(t, int64(1), n, "exactly one policy, not one per install")
}

// TestInstallPolicy_SetsForce — without FORCE the policies exist, show up in
// pg_policies, and filter nothing for the table owner, which is the runtime role
// in Bifrost's single-credential deployment. rls.Enforce's table.not_forced check
// exists for this failure; the installer must not be what causes it.
//
// The assertion is on pg_class rather than on the owner's own reads. This fixture
// is owned by the bootstrap superuser, and a superuser bypasses RLS
// unconditionally — FORCE does not constrain it (R31, and the spike's
// TestRLS_SuperuserBypassesEvenWithForce). Asserting "the owner sees nothing"
// here would be asserting a false proposition. FORCE's effect on a non-superuser
// owner is what the app-role tests above cover.
func TestInstallPolicy_SetsForce(t *testing.T) {
	const table = "mig_force"
	owner, _ := fixture(t, table, "")
	require.NoError(t, migrate.AddTenantColumn(owner, table))
	require.NoError(t, migrate.InstallPolicy(owner, table, migrate.PolicyStrict))

	var enabled, forced bool
	require.NoError(t, owner.Raw(
		`SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = ?`,
		table).Row().Scan(&enabled, &forced))
	require.True(t, enabled, "RLS enabled")
	require.True(t, forced, "FORCE set — rls.Enforce refuses to boot without it")

	var policies int64
	require.NoError(t, owner.Raw(
		`SELECT count(*) FROM pg_policies WHERE tablename = ? AND policyname = ?`,
		table, migrate.PolicyName).Scan(&policies).Error)
	require.Equal(t, int64(1), policies)
}

func TestRemovePolicy_RestoresUnfilteredAccess(t *testing.T) {
	const table = "mig_remove"
	owner, app := fixture(t, table, "")
	require.NoError(t, migrate.AddTenantColumn(owner, table))
	require.NoError(t, migrate.InstallPolicy(owner, table, migrate.PolicyStrict))
	seed(t, owner, table, []seedRow{{ptr("tenant-a"), "a-row"}, {ptr("tenant-b"), "b-row"}})

	require.NoError(t, migrate.RemovePolicy(owner, table))
	// Both DISABLE and NO FORCE are needed: dropping only the policy would leave
	// RLS on with no policy, which shows zero rows — indistinguishable from data
	// loss for the application.
	require.Len(t, labelsVisibleToPlatform(t, app, table), 2)
}
