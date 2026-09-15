package rlsspike_test

import (
	"context"
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/rlsspike"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// setTenant binds the tenant onto the connection/transaction.
//
// It must use set_config() rather than `SET LOCAL x = ?`: Postgres's SET is
// utility syntax and rejects bind parameters ("syntax error at or near $1",
// SQLSTATE 42601). That is load-bearing for the design — the only way to write
// it as SET would be to interpolate the tenant id into the statement string,
// which turns tenant context into a SQL-injection sink. set_config is an
// ordinary function call, so the value is a real parameter.
//
// local=true scopes the setting to the surrounding transaction (the SET LOCAL
// equivalent); local=false is session-scoped and survives on the pooled
// connection, which TestSetSession_LeaksAcrossPooledConnections exercises.
func setTenant(t *testing.T, db *gorm.DB, tenantID string, local bool) {
	t.Helper()
	require.NoError(t, db.Exec(
		`SELECT set_config('`+rlsspike.TenantIDSetting+`', ?, ?)`, tenantID, local).Error)
}

// labels runs the query pattern that matters: a SELECT with NO tenant predicate,
// i.e. exactly what Bifrost's ~387 raw s.DB() call sites already do today. If
// RLS works, isolation happens without this query knowing tenants exist.
func labels(t *testing.T, tx *gorm.DB) []string {
	t.Helper()
	var out []string
	require.NoError(t, tx.Model(&rlsspike.ProviderKey{}).Order("label").Pluck("label", &out).Error)
	return out
}

// fixture builds the table as the non-superuser owner role and grants the app
// role DML on it. force controls FORCE ROW LEVEL SECURITY.
func fixture(t *testing.T, force bool) *gorm.DB {
	t.Helper()
	bootstrap, err := rlsspike.OpenBootstrap()
	require.NoError(t, err, "start the spike Postgres first (see the package doc comment)")
	require.NoError(t, rlsspike.EnsureRoles(bootstrap))

	owner, err := rlsspike.OpenOwner()
	require.NoError(t, err)
	require.NoError(t, rlsspike.Setup(owner, force))
	require.NoError(t, rlsspike.GrantApp(owner))
	return owner
}

func appDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := rlsspike.OpenApp()
	require.NoError(t, err)
	return db
}

// TestRLS_BlocksRawUnscopedQuery is the decisive test for D11b: can the database
// constrain a query that has no idea tenants exist?
func TestRLS_BlocksRawUnscopedQuery(t *testing.T) {
	fixture(t, false)
	app := appDB(t)

	// The setting is transaction-scoped, so the request must own a transaction.
	err := app.Transaction(func(tx *gorm.DB) error {
		setTenant(t, tx, "tenant-a", true)
		got := labels(t, tx)
		// Tenant A sees its own BYOK key plus the platform pool, and NOT B's key.
		require.ElementsMatch(t, []string{"platform-pool-key", "tenant-a-byok"}, got)
		require.NotContains(t, got, "tenant-b-byok")
		return nil
	})
	require.NoError(t, err)
}

// TestRLS_OwnerBypassesWithoutForce records the deployment hazard that decides
// whether RLS is usable at all in Bifrost's current connection model: Postgres
// exempts table owners from RLS unless FORCE is set, and Bifrost's migration
// pool and runtime pool are built from the SAME config (framework/configstore/
// postgres.go), so the runtime role IS the table owner.
func TestRLS_OwnerBypassesWithoutForce(t *testing.T) {
	owner := fixture(t, false)

	err := owner.Transaction(func(tx *gorm.DB) error {
		setTenant(t, tx, "tenant-a", true)
		got := labels(t, tx)
		// The policy is enabled and the tenant variable is set, yet the owner
		// still sees tenant B's key. RLS is silently inert.
		require.Contains(t, got, "tenant-b-byok",
			"expected owner to bypass RLS without FORCE; if this fails Postgres changed its default")
		require.Len(t, got, 3)
		return nil
	})
	require.NoError(t, err)
}

// TestRLS_ForceConstrainsOwner shows the fix for the above.
func TestRLS_ForceConstrainsOwner(t *testing.T) {
	owner := fixture(t, true)

	err := owner.Transaction(func(tx *gorm.DB) error {
		setTenant(t, tx, "tenant-a", true)
		got := labels(t, tx)
		require.ElementsMatch(t, []string{"platform-pool-key", "tenant-a-byok"}, got)
		return nil
	})
	require.NoError(t, err)
}

// TestRLS_SuperuserBypassesEvenWithForce is the sharpest operational hazard the
// spike found: FORCE ROW LEVEL SECURITY does NOT constrain a superuser, nor any
// role holding BYPASSRLS. Isolation silently evaporates with no error raised
// anywhere — the policy is present, enabled and forced, and still returns every
// tenant's rows.
//
// This is not a theoretical concern. A container Postgres hands out a superuser
// by default (the role this spike bootstraps with is one), so a deployment that
// points Bifrost's DSN at it gets zero isolation while every policy looks
// correctly installed.
func TestRLS_SuperuserBypassesEvenWithForce(t *testing.T) {
	fixture(t, true)

	bootstrap, err := rlsspike.OpenBootstrap()
	require.NoError(t, err)

	var isSuper, bypass bool
	require.NoError(t, bootstrap.Raw(
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Row().Scan(&isSuper, &bypass))
	require.True(t, isSuper || bypass, "premise: bootstrap role must be exempt from RLS")

	err = bootstrap.Transaction(func(tx *gorm.DB) error {
		setTenant(t, tx, "tenant-a", true)
		got := labels(t, tx)
		require.Contains(t, got, "tenant-b-byok",
			"superuser/BYPASSRLS ignores FORCE ROW LEVEL SECURITY")
		require.Len(t, got, 3)
		return nil
	})
	require.NoError(t, err)
}

// TestRLS_FailsClosedWhenTenantUnset checks the failure direction. queryscope's
// documented default is fail-OPEN (nil scope = no restriction). RLS inverts
// that for tenant rows, which is the safer direction — but note the platform
// pool stays visible, because tenant_id IS NULL is unconditionally true.
func TestRLS_FailsClosedWhenTenantUnset(t *testing.T) {
	fixture(t, true)

	app := appDB(t)
	err := app.Transaction(func(tx *gorm.DB) error {
		// No tenant set at all — simulates a code path that forgot to set it.
		got := labels(t, tx)
		require.Equal(t, []string{"platform-pool-key"}, got,
			"unset tenant must not expose any tenant-owned row")
		return nil
	})
	require.NoError(t, err)
}

// TestSetSession_LeaksAcrossPooledConnections proves why SET SESSION is not an
// option: with a reused pool the setting outlives the request that set it.
func TestSetSession_LeaksAcrossPooledConnections(t *testing.T) {
	fixture(t, true)

	app := appDB(t)
	sqlDB, err := app.DB()
	require.NoError(t, err)
	// Single connection makes reuse deterministic; a real pool reuses
	// non-deterministically, which is worse (intermittent cross-tenant reads).
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	ctx := context.Background()
	// local=false: session-scoped, i.e. the SET SESSION equivalent.
	setTenant(t, app.WithContext(ctx), "tenant-a", false)

	var leaked string
	require.NoError(t, app.WithContext(ctx).Raw(
		`SELECT coalesce(current_setting('`+rlsspike.TenantIDSetting+`', true), '')`).Scan(&leaked).Error)
	require.Equal(t, "tenant-a", leaked,
		"SET SESSION persisted onto the pooled connection: the next request inherits tenant-a")
}

// TestSetLocal_RequiresTransaction verifies the constraint that forces the
// per-request-transaction design: outside an explicit transaction each Exec is
// its own implicit transaction, so SET LOCAL is discarded immediately.
func TestSetLocal_RequiresTransaction(t *testing.T) {
	fixture(t, true)

	app := appDB(t)
	sqlDB, err := app.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	// local=true outside an explicit transaction: each Exec is its own implicit
	// transaction, so the setting is discarded the moment it returns.
	setTenant(t, app, "tenant-a", true)

	var after string
	require.NoError(t, app.Raw(
		`SELECT coalesce(current_setting('`+rlsspike.TenantIDSetting+`', true), '')`).Scan(&after).Error)
	require.Empty(t, after,
		"SET LOCAL outside an explicit transaction does not survive; RLS would fail closed")
}
