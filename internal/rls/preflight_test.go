package rls_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/pglock"
	"github.com/luojinghua50/Joysteed-MaaS/internal/rls"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Every test here asserts the preflight CATCHES a specific silent-failure mode.
// That direction matters: the thing being guarded against raises no error at
// query time, so a preflight that returned "OK" for a broken deployment would
// look exactly like a preflight that worked.
//
// Requires the spike Postgres:
//
//	docker run -d --name maas-rls-spike \
//	  -e POSTGRES_PASSWORD=spike_password -e POSTGRES_USER=spike \
//	  -e POSTGRES_DB=spike -p 55432:5432 postgres:16-alpine

const (
	tableName  = "preflight_keys"
	appRole    = "preflight_app"
	appPass    = "preflight_app_password"
	superRole  = "preflight_super"
	superPass  = "preflight_super_password"
	bypassRole = "preflight_bypass"
	bypassPass = "preflight_bypass_password"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func dsnFor(user, password string) string {
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		env("MAAS_SPIKE_PG_HOST", "localhost"),
		env("MAAS_SPIKE_PG_PORT", "55432"),
		user, password,
		env("MAAS_SPIKE_PG_DB", "spike"))
}

func open(t *testing.T, user, password string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(postgres.Open(dsnFor(user, password)), &gorm.Config{
		Logger: logger.Discard,
	})
	require.NoError(t, err, "start the spike Postgres first (see the file header)")
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// bootstrap connects as the container superuser to create roles and the table.
func bootstrap(t *testing.T) *gorm.DB {
	t.Helper()
	return open(t, env("MAAS_SPIKE_PG_USER", "spike"), env("MAAS_SPIKE_PG_PASSWORD", "spike_password"))
}

// fixture builds a table in the state described by the options, as the app role
// (so the app role owns it — Bifrost's actual shape, where the runtime role is
// the table owner).
type fixture struct {
	enableRLS bool
	forceRLS  bool
	policy    bool
	dropTable bool
}

func setup(t *testing.T, f fixture) {
	t.Helper()
	root := bootstrap(t)

	stmts := []string{
		fmt.Sprintf(`DO $$ BEGIN
			IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%s') THEN
				CREATE ROLE %s LOGIN PASSWORD '%s';
			END IF;
		END $$`, appRole, appRole, appPass),
		fmt.Sprintf(`DO $$ BEGIN
			IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%s') THEN
				CREATE ROLE %s LOGIN SUPERUSER PASSWORD '%s';
			END IF;
		END $$`, superRole, superRole, superPass),
		fmt.Sprintf(`DO $$ BEGIN
			IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%s') THEN
				CREATE ROLE %s LOGIN BYPASSRLS PASSWORD '%s';
			END IF;
		END $$`, bypassRole, bypassRole, bypassPass),
		// Drop as bootstrap: a table left by a prior run may have a different owner.
		fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tableName),
		// Membership from a prior bypass-reachable test must not leak.
		fmt.Sprintf(`REVOKE %s FROM %s`, superRole, appRole),
	}
	for _, s := range stmts {
		require.NoError(t, root.Exec(s).Error, s)
	}
	// Under the shared-catalog lock: this GRANT updates the single pg_namespace row
	// for `public`, which other test packages' bootstraps also update. Postgres
	// does not do that under MVCC, so concurrent packages abort with "tuple
	// concurrently updated" (see internal/pglock).
	require.NoError(t, pglock.WithSharedCatalog(root, func(tx *gorm.DB) error {
		return tx.Exec(fmt.Sprintf(`GRANT USAGE, CREATE ON SCHEMA public TO %s`, appRole)).Error
	}))

	if f.dropTable {
		return
	}

	app := open(t, appRole, appPass)
	create := []string{
		fmt.Sprintf(`CREATE TABLE %s (
			id BIGSERIAL PRIMARY KEY,
			tenant_id TEXT,
			label TEXT NOT NULL
		)`, tableName),
	}
	if f.enableRLS {
		create = append(create, fmt.Sprintf(`ALTER TABLE %s ENABLE ROW LEVEL SECURITY`, tableName))
	}
	if f.policy {
		create = append(create, fmt.Sprintf(`CREATE POLICY tenant_isolation ON %s
			FOR ALL USING (tenant_id IS NULL
				OR tenant_id = current_setting('app.tenant_id', true))`, tableName))
	}
	if f.forceRLS {
		create = append(create, fmt.Sprintf(`ALTER TABLE %s FORCE ROW LEVEL SECURITY`, tableName))
	}
	for _, s := range create {
		require.NoError(t, app.Exec(s).Error, s)
	}
}

func checkAs(t *testing.T, user, password string) *rls.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := rls.Check(ctx, open(t, user, password), []string{tableName})
	require.NoError(t, err, "Check itself must not error on a reachable database")
	return res
}

// checks returns the set of check names in the result, for order-independent
// assertions.
func checks(res *rls.Result) map[string]bool {
	out := make(map[string]bool, len(res.Findings))
	for _, f := range res.Findings {
		out[f.Check] = true
	}
	return out
}

// TestPreflight_PassesWhenCorrectlyConfigured is the baseline. Without it, a
// preflight that failed everything unconditionally would pass every other test
// in this file.
func TestPreflight_PassesWhenCorrectlyConfigured(t *testing.T) {
	setup(t, fixture{enableRLS: true, forceRLS: true, policy: true})

	res := checkAs(t, appRole, appPass)
	require.True(t, res.OK(), "expected no findings, got: %v", res.Findings)
	require.NoError(t, res.Err())
	require.Equal(t, appRole, res.Role)
}

func TestPreflight_CatchesSuperuser(t *testing.T) {
	setup(t, fixture{enableRLS: true, forceRLS: true, policy: true})

	res := checkAs(t, superRole, superPass)
	require.True(t, checks(res)["role.superuser"],
		"a superuser connection must be refused; FORCE does not constrain it. got: %v", res.Findings)
	require.Error(t, res.Err())
}

func TestPreflight_CatchesBypassRLS(t *testing.T) {
	setup(t, fixture{enableRLS: true, forceRLS: true, policy: true})

	res := checkAs(t, bypassRole, bypassPass)
	require.True(t, checks(res)["role.bypassrls"],
		"a BYPASSRLS role must be refused. got: %v", res.Findings)
	require.Error(t, res.Err())
}

// TestPreflight_CatchesMissingForce covers Bifrost's default shape: the runtime
// role owns the table (one config builds both pools), so without FORCE the
// policy is installed and inert.
func TestPreflight_CatchesMissingForce(t *testing.T) {
	setup(t, fixture{enableRLS: true, forceRLS: false, policy: true})

	res := checkAs(t, appRole, appPass)
	require.True(t, checks(res)["table.not_forced"],
		"RLS enabled without FORCE must be refused when the role owns the table. got: %v", res.Findings)
	require.Error(t, res.Err())
}

func TestPreflight_CatchesRLSDisabled(t *testing.T) {
	setup(t, fixture{enableRLS: false, forceRLS: false, policy: false})

	res := checkAs(t, appRole, appPass)
	require.True(t, checks(res)["table.rls_disabled"],
		"a table with no RLS at all must be refused. got: %v", res.Findings)
}

// TestPreflight_CatchesMissingPolicy covers the inverse hazard: RLS on with no
// policy fails closed (table reads empty), so it leaks nothing — but the
// feature is broken and the cause is invisible.
func TestPreflight_CatchesMissingPolicy(t *testing.T) {
	setup(t, fixture{enableRLS: true, forceRLS: true, policy: false})

	res := checkAs(t, appRole, appPass)
	require.True(t, checks(res)["table.no_policy"],
		"RLS with zero policies must be reported. got: %v", res.Findings)
}

func TestPreflight_CatchesMissingTable(t *testing.T) {
	setup(t, fixture{dropTable: true})

	res := checkAs(t, appRole, appPass)
	require.True(t, checks(res)["table.missing"],
		"an absent table must be reported rather than silently passing. got: %v", res.Findings)
}

// TestPreflight_CatchesBypassReachableViaMembership covers the subtler case:
// the role's own attributes are clean, but it can SET ROLE to a superuser.
// Isolation then holds only until someone runs SET ROLE.
func TestPreflight_CatchesBypassReachableViaMembership(t *testing.T) {
	setup(t, fixture{enableRLS: true, forceRLS: true, policy: true})

	root := bootstrap(t)
	require.NoError(t, root.Exec(fmt.Sprintf(`GRANT %s TO %s`, superRole, appRole)).Error)
	t.Cleanup(func() {
		_ = root.Exec(fmt.Sprintf(`REVOKE %s FROM %s`, superRole, appRole)).Error
	})

	res := checkAs(t, appRole, appPass)
	require.True(t, checks(res)["role.bypass_reachable"],
		"membership in an RLS-exempt role must be surfaced. got: %v", res.Findings)
}

// TestPreflight_ReportsAllFindingsAtOnce checks the operator-facing property:
// one restart should reveal every problem, not the first one.
func TestPreflight_ReportsAllFindingsAtOnce(t *testing.T) {
	setup(t, fixture{enableRLS: true, forceRLS: false, policy: false})

	// Superuser connection AND a table that is neither forced nor has a policy.
	res := checkAs(t, superRole, superPass)

	got := checks(res)
	require.True(t, got["role.superuser"], "got: %v", res.Findings)
	require.True(t, got["table.not_forced"], "got: %v", res.Findings)
	require.True(t, got["table.no_policy"], "got: %v", res.Findings)
	require.GreaterOrEqual(t, len(res.Findings), 3,
		"all problems must be reported together, got: %v", res.Findings)

	msg := res.Err().Error()
	require.Contains(t, msg, "role.superuser")
	require.Contains(t, msg, "table.not_forced")
	require.Contains(t, msg, "would NOT be enforced")
}

// TestPreflight_EmptyTableListIsItselfAFinding guards against the failure where
// the preflight is wired up but handed nothing, and reports OK by vacuous truth.
func TestPreflight_EmptyTableListIsItselfAFinding(t *testing.T) {
	setup(t, fixture{enableRLS: true, forceRLS: true, policy: true})

	ctx := context.Background()
	res, err := rls.Check(ctx, open(t, appRole, appPass), nil)
	require.NoError(t, err)
	require.True(t, checks(res)["tables.empty"],
		"verifying nothing must not report success. got: %v", res.Findings)
	require.Error(t, res.Err())
}

// TestEnforce_ReturnsErrorOnBrokenSetup covers the wrapper startup code calls.
func TestEnforce_ReturnsErrorOnBrokenSetup(t *testing.T) {
	setup(t, fixture{enableRLS: true, forceRLS: false, policy: true})

	err := rls.Enforce(context.Background(), open(t, appRole, appPass), []string{tableName})
	require.Error(t, err)
	require.Contains(t, err.Error(), "table.not_forced")
}

func TestEnforce_PassesOnCorrectSetup(t *testing.T) {
	setup(t, fixture{enableRLS: true, forceRLS: true, policy: true})

	require.NoError(t, rls.Enforce(context.Background(),
		open(t, appRole, appPass), []string{tableName}))
}

// The dialect tests below are the only ones in this file that do NOT need the
// spike Postgres — being on the wrong backend is the thing under test.

func openSQLite(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// TestPreflight_CatchesNonPostgresDialect covers R36. The hazard is not only
// that RLS is absent: Bifrost's dbForUpdate silently drops FOR UPDATE off
// Postgres, so budget read-modify-write takes no row lock and reports no error.
func TestPreflight_CatchesNonPostgresDialect(t *testing.T) {
	res, err := rls.Check(context.Background(), openSQLite(t), []string{tableName})
	require.NoError(t, err, "the wrong backend is a finding, not a Check failure")

	require.True(t, checks(res)["dialect.not_postgres"], "got: %v", res.Findings)
	require.Equal(t, "sqlite", res.Dialect)
	require.Error(t, res.Err())
}

// TestPreflight_DialectCheckShortCircuits pins the ordering. Every other check
// reads a pg_* catalog, so running them on SQLite would surface "no such table:
// pg_roles" — an error about the preflight rather than a verdict about the
// deployment. Exactly one finding is the evidence it stopped.
func TestPreflight_DialectCheckShortCircuits(t *testing.T) {
	res, err := rls.Check(context.Background(), openSQLite(t), []string{tableName})
	require.NoError(t, err)

	require.Len(t, res.Findings, 1, "must not attempt pg_* checks off Postgres, got: %v", res.Findings)
	require.Equal(t, "dialect.not_postgres", res.Findings[0].Check)
	require.Empty(t, res.Role, "reading current_user is itself Postgres-specific")

	// With no role read, the failure has to name the dialect — otherwise it
	// reports a problem with role "".
	msg := res.Err().Error()
	require.Contains(t, msg, `dialect "sqlite"`)
	require.NotContains(t, msg, `role ""`)
}

// TestEnforce_RefusesNonPostgres is the startup path: a SQLite-backed deployment
// must not boot, and the message must say why it matters beyond isolation.
func TestEnforce_RefusesNonPostgres(t *testing.T) {
	err := rls.Enforce(context.Background(), openSQLite(t), []string{tableName})
	require.Error(t, err)
	require.Contains(t, err.Error(), "dialect.not_postgres")
	require.Contains(t, err.Error(), "FOR UPDATE")
}

// TestPreflight_RecordsPostgresDialect guards the field itself: a Dialect that
// were never populated would leave every assertion above passing on "".
func TestPreflight_RecordsPostgresDialect(t *testing.T) {
	setup(t, fixture{enableRLS: true, forceRLS: true, policy: true})

	res, err := rls.Check(context.Background(), open(t, appRole, appPass), []string{tableName})
	require.NoError(t, err)
	require.Equal(t, rls.RequiredDialect, res.Dialect)
	require.True(t, res.OK(), "got: %v", res.Findings)
}
