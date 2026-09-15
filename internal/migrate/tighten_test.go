package migrate_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/migrate"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Phase-3 tests. The behaviour ones run against Postgres for the reason
// migrate_test.go's header gives: NOT NULL validation under a live RLS policy, and
// MATCH SIMPLE semantics, are both Postgres behaviours that SQLite would report
// success on while proving nothing (R34).

// createTablesWithEdges is createAllTenantTables plus the parent-pointer columns
// the composite FKs reference. Without them pass 3 would fail on a missing column,
// which is a fixture problem rather than anything under test.
func createTablesWithEdges(t *testing.T) *gorm.DB {
	t.Helper()
	owner := createAllTenantTables(t)
	for _, fk := range migrate.TenantFKs {
		require.NoError(t, owner.Exec(fmt.Sprintf(
			`ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s BIGINT`, fk.Child, fk.ChildColumn)).Error)
	}
	return owner
}

// tighteningByID finds one phase-3 migration so a test can drive it alone. Fails
// rather than returning nil, so a renamed ID surfaces here instead of as a nil
// dereference later.
func tighteningByID(t *testing.T, id string) func(*gorm.DB) error {
	t.Helper()
	for _, m := range migrate.TighteningMigrations() {
		if m.ID == id {
			require.NotNil(t, m.Migrate, "migration %s has no Migrate func", id)
			return m.Migrate
		}
	}
	t.Fatalf("no tightening migration with ID %q", id)
	return nil
}

// TestSetNotNull_ValidatesRowsThePolicyHides pins the claim notNullMigration's
// comment rests on: DDL constraint validation is not filtered by RLS.
//
// This matters because the rest of this package has to go out of its way to work
// around the opposite property. assertBackfilled enables platform mode before
// COUNTING unattributed rows, because a SELECT under a strict policy cannot see
// them and would report zero. If SET NOT NULL had that same blindness it would
// SUCCEED on a table full of invisible NULLs and leave a column marked NOT NULL
// that contains nulls.
//
// It does not, and the test demonstrates both halves on the same table: the row is
// genuinely invisible to a policy-filtered read, and the ALTER still fails on it.
func TestSetNotNull_ValidatesRowsThePolicyHides(t *testing.T) {
	owner := createAllTenantTables(t)
	ctx := context.Background()
	require.NoError(t, migrate.Apply(ctx, owner))

	insertBypassingPolicy(t, owner, "prompts", "orphan", nil)

	// Half one: the row is invisible through the live policy. Read as the owner
	// under FORCE, which is what the migration itself runs as.
	var visible int64
	require.NoError(t, owner.Raw(`SELECT count(*) FROM prompts`).Scan(&visible).Error)
	require.Zero(t, visible, "the premise: this row is hidden from an ordinary read")

	// Half two: the ALTER sees it anyway.
	err := migrate.SetTenantColumnNotNull(owner, "prompts")
	require.Error(t, err, "SET NOT NULL must validate rows the policy hides")
	require.Contains(t, strings.ToLower(err.Error()), "null")
}

// TestTighteningMigrations_PassesAreOrdered pins the ordering the whole set depends
// on and which is invisible at the call site, since TighteningMigrations() is just
// a slice.
//
// All three orderings are load-bearing. notnull before fk is the MATCH SIMPLE
// correction (see AddTenantFK). parentkey before fk is a hard Postgres requirement:
// a composite reference needs a unique constraint to point at. And the passes are
// global rather than per-table because an edge can point from a table late in the
// list to one early in it, so interleaving would reference a parent key that does
// not exist yet.
func TestTighteningMigrations_PassesAreOrdered(t *testing.T) {
	const (
		notNull   = "20260916-01-notnull-"
		parentKey = "20260916-02-parentkey-"
		fk        = "20260916-03-fk-"
	)
	lastNotNull, lastParentKey, firstParentKey, firstFK := -1, -1, -1, -1
	for i, m := range migrate.TighteningMigrations() {
		switch {
		case strings.HasPrefix(m.ID, notNull):
			lastNotNull = i
		case strings.HasPrefix(m.ID, parentKey):
			if firstParentKey < 0 {
				firstParentKey = i
			}
			lastParentKey = i
		case strings.HasPrefix(m.ID, fk):
			if firstFK < 0 {
				firstFK = i
			}
		default:
			t.Fatalf("migration %q belongs to no known pass", m.ID)
		}
	}
	require.Positive(t, lastNotNull)
	require.Positive(t, firstFK)
	require.Less(t, lastNotNull, firstParentKey, "every NOT NULL must precede every parent key")
	require.Less(t, lastParentKey, firstFK, "every parent key must precede every FK that references it")
}

// TestTighteningMigrations_CoverEveryTableAndEdgeExactlyOnce.
//
// Exactly once, not at least once: a duplicated ID is applied on the first run and
// skipped on later ones, so a duplicate would make the set behave differently on a
// fresh database than on an existing one.
func TestTighteningMigrations_CoverEveryTableAndEdgeExactlyOnce(t *testing.T) {
	notNulls := map[string]int{}
	parentKeys := map[string]int{}
	fks := map[string]int{}
	for _, m := range migrate.TighteningMigrations() {
		if s, ok := cutPrefix(m.ID, "20260916-01-notnull-"); ok {
			notNulls[s]++
		}
		if s, ok := cutPrefix(m.ID, "20260916-02-parentkey-"); ok {
			parentKeys[s]++
		}
		if s, ok := cutPrefix(m.ID, "20260916-03-fk-"); ok {
			fks[s]++
		}
	}
	require.Len(t, notNulls, len(migrate.BClassTables))
	for _, table := range migrate.BClassTables {
		require.Equal(t, 1, notNulls[table], "%s must be tightened exactly once", table)
	}
	// config_keys is deliberately absent: NULL there means the platform pool (D1),
	// so tightening it would delete the shared-pool concept.
	require.NotContains(t, notNulls, migrate.CClassTable,
		"config_keys must never be NOT NULL — NULL is the platform pool")
	require.NotContains(t, notNulls, migrate.SessionsTable,
		"sessions holds platform-owned rows with a NULL tenant_id")

	require.Len(t, parentKeys, len(migrate.ParentTables()))
	require.Len(t, fks, len(migrate.TenantFKs))
	for _, fk := range migrate.TenantFKs {
		require.Equal(t, 1, fks[fk.Name])
	}
}

// TestTighteningMigrations_IDsDoNotCollideWithPhase12 matters because both sets are
// recorded in the same maas_migrations table. A shared ID would make one set's
// applied row satisfy the other's pending check, silently skipping a step.
func TestTighteningMigrations_IDsDoNotCollideWithPhase12(t *testing.T) {
	seen := map[string]bool{}
	for _, m := range migrate.Migrations() {
		require.False(t, seen[m.ID], "duplicate ID %q", m.ID)
		seen[m.ID] = true
	}
	for _, m := range migrate.TighteningMigrations() {
		require.False(t, seen[m.ID], "tightening ID %q collides with a phase-1/2 ID", m.ID)
		seen[m.ID] = true
	}
}

// TestTighteningMigrations_EveryStepIsReversible. A phase-3 step that cannot be
// rolled back would mean a failed tightening deploy has no way back to the
// previous schema.
func TestTighteningMigrations_EveryStepIsReversible(t *testing.T) {
	for _, m := range migrate.TighteningMigrations() {
		require.NotNil(t, m.Rollback, "migration %s has no Rollback", m.ID)
	}
}

// insertWithParent writes a row carrying a parent pointer, behind the live policy.
//
// NO FORCE rather than DISABLE, matching insertBypassingPolicy: the owner is exempt
// from its own table's RLS only when FORCE is off, so this is the narrowest window —
// policies stay defined and enabled throughout.
//
// Note what this does NOT bypass: the foreign keys. Postgres performs referential
// integrity checks with RLS bypassed regardless, so an FK still refuses a row here
// even though the policy would not. That is what makes the assertions below about
// the FK rather than about the policy.
func insertWithParent(t *testing.T, db *gorm.DB, table, column, tenantID string, parentID int64) error {
	t.Helper()
	require.NoError(t, db.Exec(fmt.Sprintf(`ALTER TABLE %s NO FORCE ROW LEVEL SECURITY`, table)).Error)
	defer func() {
		require.NoError(t, db.Exec(fmt.Sprintf(`ALTER TABLE %s FORCE ROW LEVEL SECURITY`, table)).Error)
	}()
	return db.Exec(fmt.Sprintf(
		`INSERT INTO %s (tenant_id, label, %s) VALUES (?, 'c', ?)`, table, column),
		tenantID, parentID).Error
}

// TestNotNullMigration_RefusesAnUnbackfilledTable pins the gate.
//
// The gate is for the error message rather than for correctness — Postgres would
// refuse this ALTER on its own — but the message is the difference between "column
// contains null values" and a named table with a count, on a step whose only
// remedy is a backfill nobody has written yet.
//
// Driven through owner.Transaction rather than on the bare handle, because that is
// how the migrator invokes it (Options.UseTransaction, inherited as true). The
// distinction is not cosmetic: assertBackfilled sets platform mode with
// set_config(..., local=true), and outside an explicit transaction every statement
// is its own implicit one, so the setting is discarded the moment it is made and
// the gate fails with ErrPlatformModeNotSet instead of doing its job.
func TestNotNullMigration_RefusesAnUnbackfilledTable(t *testing.T) {
	owner := createAllTenantTables(t)
	require.NoError(t, migrate.Apply(context.Background(), owner))
	insertBypassingPolicy(t, owner, "skills", "orphan", nil)

	migrateFn := tighteningByID(t, "20260916-01-notnull-skills")

	err := owner.Transaction(migrateFn)
	require.ErrorIs(t, err, migrate.ErrBackfillRequired)
	require.Contains(t, err.Error(), "skills")
	require.Contains(t, err.Error(), "1 such row")

	// The counterfactual: attribute the row and the identical call goes through.
	// Without this half the test would also pass against a gate that refused every
	// table unconditionally.
	require.NoError(t, owner.Exec(`ALTER TABLE skills NO FORCE ROW LEVEL SECURITY`).Error)
	require.NoError(t, owner.Exec(`UPDATE skills SET tenant_id = 't-b' WHERE tenant_id IS NULL`).Error)
	require.NoError(t, owner.Exec(`ALTER TABLE skills FORCE ROW LEVEL SECURITY`).Error)
	require.NoError(t, owner.Transaction(migrateFn))

	notNull, err := migrate.TenantColumnIsNotNull(owner, "skills")
	require.NoError(t, err)
	require.True(t, notNull, "the column must actually be tightened once the gate passes")
}

// TestApplyTightening_EndToEnd runs the whole phase-3 set and then checks the one
// thing it exists to produce: a cross-tenant child is refused.
//
// The refusal is asserted on a row that BOTH per-table policies accept. Each row's
// own tenant_id is correct; it is the pair that crosses. Before this constraint
// exists the same insert succeeds, which is the state §3.4 describes as a
// cross-tenant data path.
func TestApplyTightening_EndToEnd(t *testing.T) {
	owner := createTablesWithEdges(t)
	ctx := context.Background()
	require.NoError(t, migrate.Apply(ctx, owner))
	require.NoError(t, migrate.ApplyTightening(ctx, owner))

	var applied int64
	require.NoError(t, owner.Raw(`SELECT count(*) FROM maas_migrations`).Scan(&applied).Error)
	require.Equal(t, int64(len(migrate.Migrations())+len(migrate.TighteningMigrations())), applied,
		"both sets must be recorded in the one table without either skipping the other's IDs")

	// A parent owned by t-b.
	require.NoError(t, owner.Exec(`ALTER TABLE prompts NO FORCE ROW LEVEL SECURITY`).Error)
	var parentOfB int64
	require.NoError(t, owner.Raw(
		`INSERT INTO prompts (tenant_id, label) VALUES ('t-b', 'p') RETURNING id`).Scan(&parentOfB).Error)
	require.NoError(t, owner.Exec(`ALTER TABLE prompts FORCE ROW LEVEL SECURITY`).Error)

	require.NoError(t, insertWithParent(t, owner, "prompt_versions", "prompt_id", "t-b", parentOfB),
		"the same-tenant pair must still be writable")
	err := insertWithParent(t, owner, "prompt_versions", "prompt_id", "t-a", parentOfB)
	require.Error(t, err, "t-a must not reference t-b's prompt")
	require.Contains(t, err.Error(), "fk_pv_tenant_prompt")
}

// TestApplyTightening_IsIdempotent. The constraint DDL has no IF NOT EXISTS form, so
// each step guards on the catalog; a second run must be a no-op rather than a
// duplicate-object failure.
func TestApplyTightening_IsIdempotent(t *testing.T) {
	owner := createTablesWithEdges(t)
	ctx := context.Background()
	require.NoError(t, migrate.Apply(ctx, owner))
	require.NoError(t, migrate.ApplyTightening(ctx, owner))
	require.NoError(t, migrate.ApplyTightening(ctx, owner), "second run must be a no-op")
}

// TestCrossTenantPairs_CountsViolationsBeforeTheConstraintExists.
//
// This is the preflight for pass 3, and it runs in platform mode for the same
// reason assertBackfilled does: the count joins two tenant tables, and under strict
// policies an unbound read of either returns nothing — reporting zero violations
// because nothing was visible rather than because nothing is wrong. The test proves
// that by planting a violation the policies hide and asserting it is still counted.
func TestCrossTenantPairs_CountsViolationsBeforeTheConstraintExists(t *testing.T) {
	owner := createTablesWithEdges(t)
	ctx := context.Background()
	require.NoError(t, migrate.Apply(ctx, owner))
	fk := edge(t, "fk_pv_tenant_prompt")

	n, err := migrate.CrossTenantPairs(ctx, owner, fk)
	require.NoError(t, err)
	require.Zero(t, n, "empty tables have no violating pairs")

	require.NoError(t, owner.Exec(`ALTER TABLE prompts NO FORCE ROW LEVEL SECURITY`).Error)
	var parentOfB int64
	require.NoError(t, owner.Raw(
		`INSERT INTO prompts (tenant_id, label) VALUES ('t-b', 'p') RETURNING id`).Scan(&parentOfB).Error)
	require.NoError(t, owner.Exec(`ALTER TABLE prompts FORCE ROW LEVEL SECURITY`).Error)

	// The violating pair, and a legitimate one, and one with a NULL pointer that
	// MATCH SIMPLE will never check.
	require.NoError(t, insertWithParent(t, owner, "prompt_versions", "prompt_id", "t-a", parentOfB))
	require.NoError(t, insertWithParent(t, owner, "prompt_versions", "prompt_id", "t-b", parentOfB))
	insertBypassingPolicy(t, owner, "prompt_versions", "no-parent", ptr("t-a"))

	n, err = migrate.CrossTenantPairs(ctx, owner, fk)
	require.NoError(t, err)
	require.Equal(t, int64(1), n,
		"exactly the cross-tenant pair counts: not the matching pair, not the NULL pointer")
}
