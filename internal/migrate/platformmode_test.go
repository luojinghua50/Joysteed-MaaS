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

// labelsAcrossTenants reads in platform mode.
func labelsAcrossTenants(t *testing.T, app *gorm.DB, table string) []string {
	t.Helper()
	var labels []string
	require.NoError(t, tenant.RunAcrossTenants(context.Background(), app, func(tx *gorm.DB) error {
		return tx.Raw(fmt.Sprintf(`SELECT label FROM %s ORDER BY label`, table)).Scan(&labels).Error
	}))
	return labels
}

// TestPlatformMode_StrictTableBecomesReadable is the D15 fix.
//
// Without it, an unbound read of a B-class table returns zero rows, so upstream's
// GetGovernanceConfig — bare s.DB() multi-table reads with no tenant bound — would
// load nothing and the data plane would boot with no virtual keys and no budgets.
func TestPlatformMode_StrictTableBecomesReadable(t *testing.T) {
	const table = "mig_pm_strict"
	owner, app := fixture(t, table, "")
	require.NoError(t, migrate.AddTenantColumn(owner, table))
	require.NoError(t, migrate.InstallPolicy(owner, table, migrate.PolicyStrict))
	seed(t, owner, table, []seedRow{
		{ptr("tenant-a"), "a-row"},
		{ptr("tenant-b"), "b-row"},
		{nil, "unattributed-row"},
	})

	// Plain unbound read still sees nothing — the default did not get wider.
	require.Empty(t, labelsVisibleToPlatform(t, app, table),
		"RunAsPlatform must NOT have been silently widened")

	// Platform mode reaches every row, including the unattributed one the backfill
	// has to find.
	require.Equal(t, []string{"a-row", "b-row", "unattributed-row"},
		labelsAcrossTenants(t, app, table))

	// And a tenant-bound read is unaffected.
	require.Equal(t, []string{"a-row"}, labelsVisibleTo(t, app, table, "tenant-a"))
}

// TestPlatformMode_CanBackfillTenantID is why WITH CHECK carries the platform
// disjunct: a read-only escape hatch could find unattributed rows but not fix
// them, and fixing them is the entire point of the backfill.
func TestPlatformMode_CanBackfillTenantID(t *testing.T) {
	const table = "mig_pm_backfill"
	owner, app := fixture(t, table, "")
	require.NoError(t, migrate.AddTenantColumn(owner, table))
	require.NoError(t, migrate.InstallPolicy(owner, table, migrate.PolicyStrict))
	seed(t, owner, table, []seedRow{{nil, "needs-attribution"}})

	n, err := migrate.CountUnattributed(app.Session(&gorm.Session{}), table)
	require.NoError(t, err, "counting must work through the app role")
	_ = n // the count itself is asserted below, through platform mode

	require.NoError(t, tenant.RunAcrossTenants(context.Background(), app, func(tx *gorm.DB) error {
		before, err := migrate.CountUnattributed(tx, table)
		if err != nil {
			return err
		}
		require.Equal(t, int64(1), before)
		return tx.Exec(fmt.Sprintf(
			`UPDATE %s SET tenant_id = ? WHERE tenant_id IS NULL`, table), "tenant-a").Error
	}))

	require.Equal(t, []string{"needs-attribution"}, labelsVisibleTo(t, app, table, "tenant-a"),
		"the backfilled row must now belong to tenant-a")

	require.NoError(t, tenant.RunAcrossTenants(context.Background(), app, func(tx *gorm.DB) error {
		after, err := migrate.CountUnattributed(tx, table)
		if err != nil {
			return err
		}
		require.Zero(t, after, "SET NOT NULL is only safe once this is zero")
		return nil
	}))
}

// TestPlatformMode_DoesNotLeakOntoAPooledConnection is the test that decides
// whether this escape hatch is acceptable at all.
//
// The flag is set transaction-locally, so a committed platform-mode transaction
// must leave the connection with cross-tenant visibility OFF. If it leaked, the
// next ordinary tenant request served by that physical connection would run with
// access to every tenant's rows — and unlike a leaked tenant binding, that would
// not even look wrong in a log.
//
// maxConns=1 forces the reuse rather than hoping for it.
func TestPlatformMode_DoesNotLeakOntoAPooledConnection(t *testing.T) {
	const table = "mig_pm_leak"
	owner, _ := fixture(t, table, "")
	require.NoError(t, migrate.AddTenantColumn(owner, table))
	require.NoError(t, migrate.InstallPolicy(owner, table, migrate.PolicyStrict))
	seed(t, owner, table, []seedRow{
		{ptr("tenant-a"), "a-row"},
		{ptr("tenant-b"), "b-row"},
	})

	app := openWithConns(t, appRole, appPass, 1)

	// Use platform mode, then commit.
	require.Equal(t, []string{"a-row", "b-row"}, labelsAcrossTenants(t, app, table))

	// The flag must be off on that same connection now.
	var stillOn bool
	require.NoError(t, tenant.RunAsPlatform(context.Background(), app, func(tx *gorm.DB) error {
		var err error
		stillOn, err = tenant.PlatformModeEnabled(tx)
		return err
	}))
	require.False(t, stillOn, "platform mode must not survive its transaction")

	// The observable consequence: an unbound read is back to seeing nothing.
	require.Empty(t, labelsVisibleToPlatform(t, app, table),
		"a connection that served a platform-mode transaction must not stay wide")

	// And a tenant read on that connection sees only its own rows.
	require.Equal(t, []string{"a-row"}, labelsVisibleTo(t, app, table, "tenant-a"))
}

// TestPlatformMode_BoundTenantWinsOverTheFlag: the contradictory combination
// resolves to the NARROWER reading. The policy requires the tenant variable to be
// empty for the platform branch to apply, so a bound transaction stays confined
// even if the flag is somehow on.
func TestPlatformMode_BoundTenantWinsOverTheFlag(t *testing.T) {
	const table = "mig_pm_precedence"
	owner, app := fixture(t, table, "")
	require.NoError(t, migrate.AddTenantColumn(owner, table))
	require.NoError(t, migrate.InstallPolicy(owner, table, migrate.PolicyStrict))
	seed(t, owner, table, []seedRow{
		{ptr("tenant-a"), "a-row"},
		{ptr("tenant-b"), "b-row"},
	})

	// Set both by hand — RunAcrossTenants refuses this combination, so the only
	// way to reach it is to construct it, which is exactly what a future bug would
	// do.
	ctx := tenant.WithTenant(context.Background(), "tenant-a")
	var labels []string
	require.NoError(t, tenant.RunInTenantTx(ctx, app, func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT set_config(?, ?, true)`,
			tenant.PlatformModeSettingName, tenant.PlatformModeOn).Error; err != nil {
			return err
		}
		return tx.Raw(fmt.Sprintf(`SELECT label FROM %s ORDER BY label`, table)).Scan(&labels).Error
	}))
	require.Equal(t, []string{"a-row"}, labels,
		"a bound tenant must stay confined even with the platform flag on")
}

// TestRunAcrossTenants_RefusesWhenATenantIsResolved: the guard in Go, ahead of the
// policy. A request context carrying a tenant must not be able to ask for every
// tenant — the two intentions cannot both be honoured, so neither is guessed.
func TestRunAcrossTenants_RefusesWhenATenantIsResolved(t *testing.T) {
	const table = "mig_pm_refuse"
	owner, app := fixture(t, table, "")
	require.NoError(t, migrate.AddTenantColumn(owner, table))
	require.NoError(t, migrate.InstallPolicy(owner, table, migrate.PolicyStrict))

	ctx := tenant.WithTenant(context.Background(), "tenant-a")
	ran := false
	err := tenant.RunAcrossTenants(ctx, app, func(tx *gorm.DB) error {
		ran = true
		return nil
	})
	require.ErrorIs(t, err, tenant.ErrPlatformModeWithTenant)
	require.False(t, ran, "fn must not execute")
}

// TestPlatformMode_ExclusiveTableReachesEveryTenantsSessions covers the session
// reaper. Under PolicyExclusive an unbound transaction sees only platform-owned
// rows, which is right for admin login but wrong for the job that has to expire
// every tenant's sessions.
func TestPlatformMode_ExclusiveTableReachesEverySession(t *testing.T) {
	const table = "mig_pm_exclusive"
	owner, app := fixture(t, table, "")
	require.NoError(t, migrate.AddTenantColumn(owner, table))
	require.NoError(t, migrate.InstallPolicy(owner, table, migrate.PolicyExclusive))
	seed(t, owner, table, []seedRow{
		{nil, "admin-session"},
		{ptr("tenant-a"), "a-session"},
		{ptr("tenant-b"), "b-session"},
	})

	// Login path: platform rows only.
	require.Equal(t, []string{"admin-session"}, labelsVisibleToPlatform(t, app, table))

	// Reaper: everything.
	require.Equal(t, []string{"a-session", "admin-session", "b-session"},
		labelsAcrossTenants(t, app, table))
}

// TestPlatformMode_PoolTableStillRefusesTenantWritesIntoThePool checks the D15
// disjunct did not reopen §7.4.3's hole. Platform mode may write to the pool;
// a bound tenant still may not.
func TestPlatformMode_PoolTableStillRefusesTenantWritesIntoThePool(t *testing.T) {
	const table = "mig_pm_pool"
	owner, app := fixture(t, table, "")
	require.NoError(t, migrate.AddTenantColumn(owner, table))
	require.NoError(t, migrate.InstallPolicy(owner, table, migrate.PolicyPool))

	ctx := tenant.WithTenant(context.Background(), "tenant-a")
	require.Error(t, tenant.RunInTenantTx(ctx, app, func(tx *gorm.DB) error {
		return tx.Exec(fmt.Sprintf(
			`INSERT INTO %s (tenant_id, label) VALUES (NULL, ?)`, table), "smuggled").Error
	}), "a tenant still must not write into the shared pool")

	// The platform legitimately manages pool keys.
	require.NoError(t, tenant.RunAcrossTenants(context.Background(), app, func(tx *gorm.DB) error {
		return tx.Exec(fmt.Sprintf(
			`INSERT INTO %s (tenant_id, label) VALUES (NULL, ?)`, table), "platform-key").Error
	}))
	require.Contains(t, labelsVisibleTo(t, app, table, "tenant-a"), "platform-key")
}
