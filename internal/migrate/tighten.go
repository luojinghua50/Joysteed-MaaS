package migrate

// Phase 3: the constraints that can only be added once a table's rows carry a
// tenant_id.
//
// Kept as a SEPARATE migration set from Migrations() rather than appended to it,
// because the two have opposite deployment properties. Phase 1 and 2 are
// deployable against a populated database on any schedule: adding a nullable
// column changes no query's result, and the policy gate refuses rather than
// corrupts. Everything here requires the backfill to be finished, and the
// backfill needs real data and per-table product rules (§3.4's quarantine rule:
// a row whose owner cannot be determined must not be guessed at).
//
// Folding both into one set would mean the additive phase could not ship until
// the backfill was complete, which inverts the dependency — the backfill wants
// the column to exist first.
//
// Sharing one migration table across two sets is safe: migrator.Options
// defaults ValidateUnknownMigrations to false, and Migrate applies only the
// pending IDs from the list it is handed, so neither set sees the other's rows
// as unknown.

import (
	"context"
	"errors"
	"fmt"

	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"github.com/maximhq/bifrost/framework/migrator"
	"gorm.io/gorm"
)

// TighteningMigrations returns the ordered phase-3 set.
//
// Three passes, and the order between them is forced rather than chosen:
//
// Pass 1 (`...-notnull-<table>`) removes the NULL from tenant_id on all 27
// B-class tables. It comes first because of MATCH SIMPLE — see AddTenantFK. This
// is the pass that contradicts the order §3.4 and §7.4.6 both record.
//
// Pass 2 (`...-parentkey-<parent>`) adds UNIQUE (tenant_id, id) to the 13 tables
// an edge points at. Postgres will not accept a two-column reference without it.
//
// Pass 3 (`...-fk-<name>`) adds the 23 composite foreign keys.
//
// The passes are global rather than per-table: an edge can point from a table
// late in the list to one early in it (prompt_session_messages → prompt_sessions),
// so per-table interleaving would try to reference a parent key that does not
// exist yet.
//
// Consequence worth knowing before running it: the set halts at the first table
// whose backfill is incomplete, and no later constraint is created — including
// ones whose own tables are ready. That is the migrator's in-order semantics, and
// it is the safe reading, but it means Unattributed should be consulted first so
// the whole remaining backfill is known in one pass rather than one deploy at a
// time.
func TighteningMigrations() []*migrator.Migration {
	var out []*migrator.Migration

	for _, table := range BClassTables {
		out = append(out, notNullMigration(table))
	}
	for _, parent := range ParentTables() {
		out = append(out, parentKeyMigration(parent))
	}
	for _, fk := range TenantFKs {
		out = append(out, fkMigration(fk))
	}
	return out
}

// notNullMigration tightens one table, gated on its backfill.
//
// The gate is for the error message, not for correctness: Postgres validates SET
// NOT NULL against every row itself, and — unlike a SELECT — that validation is
// not filtered by the table's RLS policy, so it cannot be fooled into passing by
// rows the migration cannot see. What it produces is a bare "column contains null
// values" with no count and no hint that a backfill is the missing step.
// TestSetNotNull_FailsThroughPolicyEvenWhenRowsAreInvisible pins the underlying
// behaviour so this comment cannot quietly become false.
func notNullMigration(table string) *migrator.Migration {
	return &migrator.Migration{
		ID: "20260916-01-notnull-" + table,
		Migrate: func(tx *gorm.DB) error {
			if err := assertBackfilled(tx, table, PolicyStrict); err != nil {
				return err
			}
			return SetTenantColumnNotNull(tx, table)
		},
		Rollback: func(tx *gorm.DB) error {
			return DropTenantColumnNotNull(tx, table)
		},
	}
}

func parentKeyMigration(parent string) *migrator.Migration {
	return &migrator.Migration{
		ID: "20260916-02-parentkey-" + parent,
		Migrate: func(tx *gorm.DB) error {
			return AddParentTenantKey(tx, parent)
		},
		Rollback: func(tx *gorm.DB) error {
			return DropParentTenantKey(tx, parent)
		},
	}
}

// fkMigration adds one composite FK.
//
// Note what this does NOT do: validate existing rows against the new constraint
// separately. ADD CONSTRAINT ... FOREIGN KEY validates immediately by default, so
// a pre-existing cross-tenant pair fails the migration. That is the intended
// outcome — such a pair is the data path §3.4 describes, and creating the
// constraint NOT VALID would record it as present while leaving exactly the rows
// that motivated it unchecked.
func fkMigration(fk TenantFK) *migrator.Migration {
	return &migrator.Migration{
		ID: "20260916-03-fk-" + fk.Name,
		Migrate: func(tx *gorm.DB) error {
			return AddTenantFK(tx, fk)
		},
		Rollback: func(tx *gorm.DB) error {
			return DropTenantFK(tx, fk)
		},
	}
}

// RunTightening applies the phase-3 set through upstream's RunMigration hook,
// then refreshes the runtime pool. Mirrors Run.
func RunTightening(ctx context.Context, store Store) error {
	if store == nil {
		return errors.New("migrate: store is nil")
	}
	if err := store.RunMigration(ctx, func(ctx context.Context, db *gorm.DB) error {
		return ApplyTightening(ctx, db)
	}); err != nil {
		return err
	}
	if err := store.RefreshConnectionPool(ctx); err != nil {
		return fmt.Errorf("migrate: refresh connection pool after tightening: %w", err)
	}
	return nil
}

// ApplyTightening runs the phase-3 set against a *gorm.DB directly. Mirrors Apply.
func ApplyTightening(ctx context.Context, db *gorm.DB) error {
	if db == nil {
		return errors.New("migrate: db is nil")
	}
	opts := *migrator.DefaultOptions
	opts.TableName = migrationTable
	m := migrator.New(db.WithContext(ctx), &opts, TighteningMigrations())
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("migrate: apply tenant constraint tightening: %w", err)
	}
	return nil
}

// CrossTenantPairs counts child rows whose parent belongs to a different tenant,
// for one edge.
//
// This is the pre-flight for pass 3, and it exists because the alternative to
// knowing the number is learning it from a failed ALTER TABLE that names one
// violating row. Runs in platform mode: the join reads two tenant tables at once,
// and under strict policies an unbound read of either returns nothing — which
// would report zero violations because nothing was visible, not because nothing
// is wrong.
//
// A NULL child pointer is excluded, matching what MATCH SIMPLE will and will not
// check.
func CrossTenantPairs(ctx context.Context, db *gorm.DB, fk TenantFK) (int64, error) {
	for _, id := range []string{fk.Child, fk.ChildColumn, fk.Parent} {
		if err := checkIdentifier(id); err != nil {
			return 0, err
		}
	}
	var n int64
	err := tenant.RunAcrossTenants(ctx, db, func(tx *gorm.DB) error {
		return tx.Raw(fmt.Sprintf(
			`SELECT count(*) FROM %s c JOIN %s p ON p.id = c.%s
			 WHERE c.%s IS NOT NULL AND p.%s IS DISTINCT FROM c.%s`,
			fk.Child, fk.Parent, fk.ChildColumn,
			fk.ChildColumn, TenantColumn, TenantColumn)).Scan(&n).Error
	})
	if err != nil {
		return 0, fmt.Errorf("migrate: count cross-tenant pairs for %s: %w", fk.Name, err)
	}
	return n, nil
}
