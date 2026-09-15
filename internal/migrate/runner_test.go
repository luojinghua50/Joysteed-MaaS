package migrate_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/migrate"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Upstream's store must satisfy the narrow interface the runner takes. Asserted at
// compile time rather than by a test body: if upstream changes either signature,
// this file stops compiling, which is louder than a test that silently stops
// covering the real type.
var _ migrate.Store = (*configstore.RDBConfigStore)(nil)

// The role these tests run migrations as.
//
// It must be a NON-SUPERUSER that OWNS the tables, because that is what production
// is: the design doc's §9.2.5 forbids the runtime role from being a superuser or
// holding BYPASSRLS, and upstream builds the migration pool and the runtime pool
// from the same config (framework/configstore/postgres.go:34-62) — so the role that
// runs migrations is the role that serves traffic, and it owns the tables it created.
//
// Driving these tests as the container's default `spike` role instead would prove
// nothing about the gate: `spike` is a superuser, and a superuser bypasses RLS
// unconditionally even with FORCE set. That is R31, the trap the whole preflight
// check exists to catch, and it would silently make the platform-mode logic in
// assertBackfilled look unnecessary.
const (
	migratorRole = "maas_migrator"
	migratorPass = "maas_migrator_password"
)

// runnerSchema isolates this file's tables from the other packages' tests.
//
// Unavoidable here, unlike the `tenants` collision that moving the suite back into
// the package directories resolved. These tests create the REAL upstream table
// names — Migrations() derives its IDs from them, so renaming the fixtures would
// stop the test from exercising the thing under test — and `config_keys` is also
// used by internal/tenant's keyscope_test.go. Those are two different packages, so
// `go test ./...` runs them concurrently no matter which directory they sit in.
//
// A dedicated schema rather than `-p 1`: a flag has to be remembered by every
// caller and every CI config, while this holds for anyone who just runs
// `go test ./...`.
const runnerSchema = "maas_runner_test"

// newMigratorDB returns a handle for a non-superuser role that owns the tables it
// creates, in this file's own schema. The role is provisioned by the superuser, then
// connected to directly so that every CREATE TABLE below is owned by it.
func newMigratorDB(t *testing.T) *gorm.DB {
	t.Helper()
	super := open(t, env("MAAS_SPIKE_PG_USER", "spike"),
		env("MAAS_SPIKE_PG_PASSWORD", "spike_password"))
	for _, s := range []string{
		fmt.Sprintf(`DO $$ BEGIN
			IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%s') THEN
				CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOBYPASSRLS;
			END IF;
		END $$`, migratorRole, migratorRole, migratorPass),
		`CREATE SCHEMA IF NOT EXISTS ` + runnerSchema,
		// CREATE is needed for the tables and for our own migration registry.
		fmt.Sprintf(`GRANT USAGE, CREATE ON SCHEMA %s TO %s`, runnerSchema, migratorRole),
	} {
		require.NoError(t, super.Exec(s).Error, s)
	}

	// Guard the premise rather than assume it. If this role ever gained superuser
	// or BYPASSRLS, every isolation assertion below would pass while testing
	// nothing, and the failure would be invisible.
	var super_, bypass bool
	require.NoError(t, super.Raw(
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = ?`,
		migratorRole).Row().Scan(&super_, &bypass))
	require.False(t, super_, "the migration role must not be a superuser (R31)")
	require.False(t, bypass, "the migration role must not hold BYPASSRLS (R31)")

	// search_path in the DSN rather than a `SET search_path` statement: the setting
	// has to hold for every connection in the pool, and these tests use several.
	db, err := gorm.Open(
		postgres.Open(dsn(migratorRole, migratorPass)+" search_path="+runnerSchema),
		&gorm.Config{Logger: logger.Discard})
	require.NoError(t, err, "start the spike Postgres first (see migrate_test.go header)")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	sqlDB.SetMaxIdleConns(4)
	t.Cleanup(func() { _ = sqlDB.Close() })

	// Assert the isolation took effect. A silent fallback to `public` would put
	// these fixtures back in the path of internal/tenant's config_keys, and the
	// resulting failure would be intermittent — the hardest kind to trace back to a
	// connection string.
	var current string
	require.NoError(t, db.Raw(`SELECT current_schema()`).Scan(&current).Error)
	require.Equal(t, runnerSchema, current,
		"fixtures must not land in the schema other packages use")
	return db
}

// ownedTable creates one table owned by the migration role, in the shape upstream
// leaves it: no tenant_id, because adding it is what the migration does.
func ownedTable(t *testing.T, db *gorm.DB, table string) {
	t.Helper()
	require.NoError(t, db.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, table)).Error)
	require.NoError(t, db.Exec(fmt.Sprintf(
		`CREATE TABLE %s (id BIGSERIAL PRIMARY KEY, tenant_id TEXT, label TEXT)`, table)).Error)
	t.Cleanup(func() { _ = db.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, table)).Error })
}

// insertBypassingPolicy writes a row the live policy would reject.
//
// NO FORCE rather than DISABLE: the owner is exempt from its own table's RLS unless
// FORCE is set, so this is the narrowest possible window — policies stay defined and
// enabled throughout, and only the owner's exemption is restored.
func insertBypassingPolicy(t *testing.T, db *gorm.DB, table, label string, tenantID *string) {
	t.Helper()
	require.NoError(t, db.Exec(fmt.Sprintf(
		`ALTER TABLE %s NO FORCE ROW LEVEL SECURITY`, table)).Error)
	require.NoError(t, db.Exec(fmt.Sprintf(
		`INSERT INTO %s (tenant_id, label) VALUES (?, ?)`, table), tenantID, label).Error)
	require.NoError(t, db.Exec(fmt.Sprintf(
		`ALTER TABLE %s FORCE ROW LEVEL SECURITY`, table)).Error)
}

// migrationByID finds one migration so a test can drive it in isolation. Fails
// rather than returning nil, because a renamed ID would otherwise surface as a nil
// dereference several lines later.
func migrationByID(t *testing.T, id string) func(*gorm.DB) error {
	t.Helper()
	for _, m := range migrate.Migrations() {
		if m.ID == id {
			require.NotNil(t, m.Migrate, "migration %s has no Migrate func", id)
			return m.Migrate
		}
	}
	t.Fatalf("no migration with ID %q", id)
	return nil
}

// TestMigrations_AllColumnsPrecedeAllPolicies pins the phase split.
//
// This is the ordering the whole runner depends on and it is invisible at the call
// site — Migrations() is just a slice. A strict policy's predicate references
// tenant_id, so a policy migration ordered before that table's column migration
// fails at deploy time. Worse, an interleaved order would appear to work on a fresh
// database (where every table is empty) and fail only on a populated one.
func TestMigrations_AllColumnsPrecedeAllPolicies(t *testing.T) {
	lastColumn, firstPolicy := -1, -1
	for i, m := range migrate.Migrations() {
		switch {
		case strings.Contains(m.ID, "-column-"):
			lastColumn = i
			require.Equal(t, -1, firstPolicy,
				"column migration %s is ordered after policy migration at %d", m.ID, firstPolicy)
		case strings.Contains(m.ID, "-policy-"):
			if firstPolicy == -1 {
				firstPolicy = i
			}
		default:
			t.Fatalf("migration %s is neither a column nor a policy step", m.ID)
		}
	}
	require.NotEqual(t, -1, lastColumn)
	require.NotEqual(t, -1, firstPolicy)
	require.Less(t, lastColumn, firstPolicy)
}

// TestMigrations_CoverEveryTenantScopedTableExactlyOnce: a table missing from the
// policy phase keeps its tenant_id column and no policy, which is the failure mode
// with no symptom — the column is there, queries succeed, and nothing is isolated.
func TestMigrations_CoverEveryTenantScopedTableExactlyOnce(t *testing.T) {
	columns := map[string]int{}
	policies := map[string]int{}
	for _, m := range migrate.Migrations() {
		if table, ok := cutPrefix(m.ID, "20260915-01-column-"); ok {
			columns[table]++
		}
		if table, ok := cutPrefix(m.ID, "20260915-02-policy-"); ok {
			policies[table]++
		}
	}
	for _, table := range migrate.TenantScopedTables() {
		require.Equal(t, 1, columns[table], "column migrations for %s", table)
		require.Equal(t, 1, policies[table], "policy migrations for %s", table)
	}
	require.Len(t, columns, len(migrate.TenantScopedTables()))
	require.Len(t, policies, len(migrate.TenantScopedTables()))
}

// TestMigrations_IDsAreUnique. Upstream rejects duplicate IDs at Migrate() time
// (DuplicatedIDError), so a collision is caught — but only on a database. Catching
// it here means a bad table list fails in unit tests instead of at deploy.
func TestMigrations_IDsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range migrate.Migrations() {
		require.False(t, seen[m.ID], "duplicate migration ID %q", m.ID)
		seen[m.ID] = true
	}
}

// TestMigrations_EveryStepIsReversible: a policy migration that fails mid-deploy
// leaves RLS enabled on a table whose policy did not get created, and under FORCE
// that means zero rows visible to the application. Being able to roll back is the
// difference between a failed deploy and an outage.
func TestMigrations_EveryStepIsReversible(t *testing.T) {
	for _, m := range migrate.Migrations() {
		require.NotNil(t, m.Rollback, "migration %s has no Rollback", m.ID)
	}
}

// TestPolicyGate_RefusesUnattributedRows is the gate's reason for existing.
//
// PolicyStrict has no NULL branch, so a row with no tenant_id becomes invisible to
// every tenant AND to unbound platform reads the moment the policy lands. Nothing
// errors — the row is simply absent from every query the application makes. So the
// migration refuses, and the second half proves the refusal is not unconditional:
// once the row is attributed, the same migration succeeds.
func TestPolicyGate_RefusesUnattributedRows(t *testing.T) {
	const table = "governance_customers"
	owner := newMigratorDB(t)
	ownedTable(t, owner, table)
	require.NoError(t, owner.Exec(fmt.Sprintf(
		`INSERT INTO %s (tenant_id, label) VALUES ('t-a', 'attributed'), (NULL, 'orphan')`,
		table)).Error)

	migrateFn := migrationByID(t, "20260915-02-policy-"+table)

	err := owner.Transaction(migrateFn)
	require.ErrorIs(t, err, migrate.ErrBackfillRequired)
	require.Contains(t, err.Error(), table)
	require.Contains(t, err.Error(), "1 such row")

	// The counterfactual: attribute the row and the identical call goes through.
	// Without this half the test would also pass against a gate that refused
	// every table unconditionally.
	require.NoError(t, owner.Exec(fmt.Sprintf(
		`UPDATE %s SET tenant_id = 't-b' WHERE tenant_id IS NULL`, table)).Error)
	require.NoError(t, owner.Transaction(migrateFn))

	// Qualified by schema: another package's fixtures may carry the same table name
	// in `public`, and an unqualified count would tally theirs alongside ours.
	var policies int64
	require.NoError(t, owner.Raw(
		`SELECT count(*) FROM pg_policies
		 WHERE schemaname = ? AND tablename = ? AND policyname = ?`,
		runnerSchema, table, migrate.PolicyName).Scan(&policies).Error)
	require.Equal(t, int64(1), policies, "the policy must exist once the gate passes")
}

// TestPolicyGate_StillSeesOrphansAfterThePolicyIsInstalled is the subtle one, and
// it is why assertBackfilled enables platform mode instead of just counting.
//
// On a re-run the policy is already installed, and the migration runs as the table
// OWNER under FORCE ROW LEVEL SECURITY — so an ordinary count reads through the very
// policy it is trying to validate and sees zero unattributed rows, because a strict
// policy hides them. That reads as "backfill complete" when it actually means
// "cannot see anything". Platform mode is what makes the count able to observe the
// rows it is counting.
//
// Verified falsifiable: dropping the EnablePlatformMode call from assertBackfilled
// makes this test fail — the gate returns nil and the orphan stays invisible.
func TestPolicyGate_StillSeesOrphansAfterThePolicyIsInstalled(t *testing.T) {
	const table = "governance_teams"
	owner := newMigratorDB(t)
	ownedTable(t, owner, table)
	require.NoError(t, owner.Exec(fmt.Sprintf(
		`INSERT INTO %s (tenant_id, label) VALUES ('t-a', 'attributed')`, table)).Error)

	migrateFn := migrationByID(t, "20260915-02-policy-"+table)

	// First pass: clean table, policy installs.
	require.NoError(t, owner.Transaction(migrateFn))

	// An orphan appears behind the now-live policy.
	insertBypassingPolicy(t, owner, table, "orphan", nil)

	// Establish that the orphan really is invisible to an ordinary read through the
	// policy. This is the premise the platform-mode count has to defeat; if a future
	// Postgres or driver change made the row visible anyway, this assertion fails
	// first and says so, rather than the test quietly covering nothing.
	var ordinary int64
	require.NoError(t, owner.Transaction(func(tx *gorm.DB) error {
		return tx.Raw(fmt.Sprintf(
			`SELECT count(*) FROM %s WHERE tenant_id IS NULL`, table)).Scan(&ordinary).Error
	}))
	require.Equal(t, int64(0), ordinary,
		"precondition: a strict policy hides the orphan from an ordinary count")

	err := owner.Transaction(migrateFn)
	require.ErrorIs(t, err, migrate.ErrBackfillRequired,
		"the gate must see through the installed policy, or a re-run reports success on an unbackfilled table")
}

// TestPolicyGate_ExemptsPoolAndExclusiveTables: for config_keys a NULL tenant_id IS
// the platform pool (D1), and for sessions it marks a platform-owned row. Gating
// those would refuse their normal state and block the migration permanently.
func TestPolicyGate_ExemptsPoolAndExclusiveTables(t *testing.T) {
	for _, table := range []string{migrate.CClassTable, migrate.SessionsTable} {
		t.Run(table, func(t *testing.T) {
			owner := newMigratorDB(t)
			ownedTable(t, owner, table)
			require.NoError(t, owner.Exec(fmt.Sprintf(
				`INSERT INTO %s (tenant_id, label) VALUES (NULL, 'platform-owned'), ('t-a', 'tenant-owned')`,
				table)).Error)
			require.NoError(t, owner.Transaction(migrationByID(t, "20260915-02-policy-"+table)),
				"a NULL tenant_id is meaningful here, not missing")
		})
	}
}

// TestApply_IsIdempotent. Every deployed node runs this on boot, so the second run
// is the common case, not the edge case. A non-idempotent step would fail every
// boot after the first.
func TestApply_IsIdempotent(t *testing.T) {
	owner := createAllTenantTables(t)
	ctx := context.Background()

	require.NoError(t, migrate.Apply(ctx, owner))
	require.NoError(t, migrate.Apply(ctx, owner), "second Apply must be a no-op")

	// Spot-check that it actually did the work rather than skipping everything.
	for _, table := range []string{"governance_virtual_keys", migrate.CClassTable, migrate.SessionsTable} {
		var forced bool
		require.NoError(t, owner.Raw(
			`SELECT c.relforcerowsecurity FROM pg_class c
			 JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = ? AND c.relname = ?`,
			runnerSchema, table).Scan(&forced).Error)
		require.True(t, forced, "%s must end up FORCE ROW LEVEL SECURITY", table)
	}
}

// TestApply_RecordsMigrationsInOurOwnTable, not upstream's "migrations". Sharing one
// table would put our IDs in upstream's namespace, where each side can see the
// other's rows as unknown past migrations.
func TestApply_RecordsMigrationsInOurOwnTable(t *testing.T) {
	owner := createAllTenantTables(t)
	require.NoError(t, migrate.Apply(context.Background(), owner))

	var applied int64
	require.NoError(t, owner.Raw(`SELECT count(*) FROM maas_migrations`).Scan(&applied).Error)
	require.Equal(t, int64(len(migrate.Migrations())), applied)

	require.False(t, owner.Migrator().HasTable("migrations"),
		"upstream's migration table must be untouched by us")
}

// TestUnattributed_ReportsEveryTableNeedingBackfill. Returned as a map rather than a
// first error so an operator sees the whole list in one pass instead of learning
// about each table one deploy at a time.
func TestUnattributed_ReportsEveryTableNeedingBackfill(t *testing.T) {
	owner := createAllTenantTables(t)
	ctx := context.Background()

	// Apply first: before it runs there is no tenant_id column to count, so calling
	// Unattributed earlier would fail on a missing column rather than report zero.
	// Applying on empty tables also means the phase-2 gate passes trivially, which
	// is what leaves the policies live for the rest of this test.
	require.NoError(t, migrate.Apply(ctx, owner))

	got, err := migrate.Unattributed(ctx, owner)
	require.NoError(t, err)
	require.Empty(t, got, "empty tables have nothing unattributed")

	// Orphans written behind the live policies, which is the state an operator is
	// actually diagnosing: the policy is already installed and these rows are
	// invisible to every ordinary read.
	for _, table := range []string{"prompts", "skills"} {
		insertBypassingPolicy(t, owner, table, "orphan", nil)
	}
	attributed := "t-a"
	insertBypassingPolicy(t, owner, "prompts", "fine", &attributed)

	got, err = migrate.Unattributed(ctx, owner)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"prompts": 1, "skills": 1}, got,
		"only unattributed rows count, and only on strictly-owned tables")
}

// createAllTenantTables builds a stub for every tenant-scoped table, in the shape
// upstream leaves them: WITHOUT tenant_id, because adding it is what the migration
// under test does. Owned by the non-superuser migration role, for the reason
// newMigratorDB documents.
func createAllTenantTables(t *testing.T) *gorm.DB {
	t.Helper()
	owner := newMigratorDB(t)

	require.NoError(t, owner.Exec(`DROP TABLE IF EXISTS maas_migrations`).Error)
	t.Cleanup(func() { _ = owner.Exec(`DROP TABLE IF EXISTS maas_migrations`).Error })

	for _, table := range migrate.TenantScopedTables() {
		require.NoError(t, owner.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, table)).Error)
		require.NoError(t, owner.Exec(fmt.Sprintf(
			`CREATE TABLE %s (id BIGSERIAL PRIMARY KEY, label TEXT)`, table)).Error)
		tbl := table
		t.Cleanup(func() { _ = owner.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, tbl)).Error })
	}
	return owner
}

func cutPrefix(s, prefix string) (string, bool) {
	if strings.HasPrefix(s, prefix) {
		return s[len(prefix):], true
	}
	return "", false
}
