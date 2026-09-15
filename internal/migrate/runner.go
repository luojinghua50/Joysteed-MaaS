package migrate

import (
	"context"
	"errors"
	"fmt"

	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"github.com/maximhq/bifrost/framework/migrator"
	"gorm.io/gorm"
)

// migrationTable is our own registry, deliberately not upstream's "migrations".
//
// Sharing one table would put our IDs and upstream's in the same namespace, and
// upstream's Options.ValidateUnknownMigrations — if it is ever enabled there —
// fails on any recorded ID absent from the code that owns the table. Each side
// would then see the other's rows as unknown past migrations. A separate table
// costs nothing and makes that impossible.
const migrationTable = "maas_migrations"

// ErrBackfillRequired reports that a strict policy was about to be installed over
// a table that still holds rows with no tenant_id.
//
// This is refused rather than allowed, and the reason is what PolicyStrict does to
// such a row: it becomes invisible to every tenant AND to unbound platform reads,
// because the strict predicate has no NULL branch. Nothing errors — the row is
// simply gone from every query the application makes. Installing the policy first
// and backfilling afterwards would mean doing the backfill against rows the
// backfill itself can no longer see without platform mode.
var ErrBackfillRequired = errors.New("migrate: table has rows with no tenant_id; backfill before installing its policy")

// Store is the part of upstream's ConfigStore this package needs.
//
// Narrowed to two methods so tests can drive the runner against a plain *gorm.DB
// without constructing a full configstore. *configstore.RDBConfigStore satisfies
// it (asserted in the tests).
type Store interface {
	// RunMigration runs fn on a throwaway pool. Upstream's comment on it names
	// downstream consumers as the intended caller
	// (framework/configstore/rdb.go:370) — DDL on the runtime pool would leave
	// stale prepared-statement plans behind (SQLSTATE 0A000).
	RunMigration(ctx context.Context, fn func(context.Context, *gorm.DB) error) error

	// RefreshConnectionPool discards the runtime pool's cached plans. Required
	// after this package's migrations because they ALTER tables the runtime pool
	// has already queried.
	RefreshConnectionPool(ctx context.Context) error
}

// Migrations returns the ordered migration set.
//
// Two phases, and the split is not cosmetic:
//
// Phase 1 (`...-column-<table>`) adds a nullable tenant_id and its index. Safe on
// a populated table — it is additive, changes no query's results, and can be
// deployed ahead of any application change.
//
// Phase 2 (`...-policy-<table>`) installs the RLS policy. This one changes what
// every existing query returns, and for a B-class table it is only correct once
// that table's rows carry a tenant_id. Hence the gate in policyMigration.
//
// Every table gets its own ID in each phase rather than one migration widening all
// 29. Granularity buys resumability: a failure on table 14 leaves 1–13 recorded as
// applied, and the re-run resumes rather than re-issuing everything. IDs are keyed
// on the table NAME, not on position, so reordering BClassTables later cannot
// re-point an already-applied ID at a different table.
func Migrations() []*migrator.Migration {
	var out []*migrator.Migration

	// Phase 1: columns on every tenant-scoped table.
	for _, table := range BClassTables {
		out = append(out, columnMigration(table))
	}
	out = append(out, columnMigration(CClassTable))

	// sessions needs three columns and two CHECK constraints, not one column, so
	// it has its own primitive rather than being folded into the loop above.
	out = append(out, &migrator.Migration{
		ID:       "20260915-01-column-sessions",
		Migrate:  MigrateSessions,
		Rollback: RollbackSessions,
	})
	// Phase 2: policies, only after every column exists. A strict policy's
	// predicate references tenant_id, so installing one on a table still missing
	// the column fails outright — which is why all of phase 1 precedes all of
	// phase 2 rather than the two being interleaved per table.
	for _, table := range TenantScopedTables() {
		out = append(out, policyMigration(table))
	}
	return out
}

func columnMigration(table string) *migrator.Migration {
	return &migrator.Migration{
		ID: "20260915-01-column-" + table,
		Migrate: func(tx *gorm.DB) error {
			return AddTenantColumn(tx, table)
		},
		Rollback: func(tx *gorm.DB) error {
			return DropTenantColumn(tx, table)
		},
	}
}

// policyMigration installs one table's policy, gated on that table's backfill
// being complete.
func policyMigration(table string) *migrator.Migration {
	return &migrator.Migration{
		ID: "20260915-02-policy-" + table,
		Migrate: func(tx *gorm.DB) error {
			kind, ok := PolicyKindFor(table)
			if !ok {
				// Unreachable via Migrations(), which only iterates classified
				// tables. Kept because the alternative to erroring is defaulting to
				// PolicyStrict, and on an A-class platform table that makes every
				// row invisible to everyone — a total outage traced back to a
				// missing list entry.
				return fmt.Errorf("migrate: %s has no policy classification", table)
			}
			if err := assertBackfilled(tx, table, kind); err != nil {
				return err
			}
			return InstallPolicy(tx, table, kind)
		},
		Rollback: func(tx *gorm.DB) error {
			return RemovePolicy(tx, table)
		},
	}
}

// assertBackfilled refuses a strict policy over unattributed rows.
//
// Only PolicyStrict is gated, and the two exemptions are both cases where a NULL
// tenant_id is meaningful rather than missing:
//
//	PolicyPool (config_keys) — NULL IS the platform pool (D1). Gating it would
//	refuse the normal state.
//	PolicyExclusive (sessions) — NULL means a platform-owned session, and
//	pre-migration rows are expected to be unattributed; MigrateSessions documents
//	that they are left to expire rather than guessed at.
func assertBackfilled(tx *gorm.DB, table string, kind PolicyKind) error {
	if kind != PolicyStrict {
		return nil
	}
	// Platform mode first. On a re-run the policy may already be installed, and
	// the runtime role is the table owner under FORCE — so an ungated count would
	// read through the very policy it is checking and report zero unattributed
	// rows because it cannot see them. That reads as "backfill complete" when it
	// means "cannot see anything".
	if err := tenant.EnablePlatformMode(tx); err != nil {
		return fmt.Errorf("migrate: platform mode for %s backfill check: %w", table, err)
	}
	n, err := CountUnattributed(tx, table)
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: %s has %d such row(s)", ErrBackfillRequired, table, n)
	}
	return nil
}

// Run applies every pending migration through upstream's RunMigration hook, then
// refreshes the runtime pool.
//
// The refresh is not optional bookkeeping. Upstream's RunMigration comment states
// that callers should refresh when the migration altered tables the runtime pool
// has already queried, and this package's migrations do exactly that: the pool has
// cached plans for tables that now have an extra column and an RLS policy.
func Run(ctx context.Context, store Store) error {
	if store == nil {
		return errors.New("migrate: store is nil")
	}
	if err := store.RunMigration(ctx, func(ctx context.Context, db *gorm.DB) error {
		return Apply(ctx, db)
	}); err != nil {
		return err
	}
	if err := store.RefreshConnectionPool(ctx); err != nil {
		return fmt.Errorf("migrate: refresh connection pool after migration: %w", err)
	}
	return nil
}

// Apply runs the migrations against a *gorm.DB directly.
//
// Separate from Run so tests can drive it against a bare connection, and so a
// caller holding its own migration pool is not forced through the store hook.
func Apply(ctx context.Context, db *gorm.DB) error {
	if db == nil {
		return errors.New("migrate: db is nil")
	}
	opts := *migrator.DefaultOptions
	opts.TableName = migrationTable
	// UseTransaction stays true (inherited): each migration runs in its own
	// transaction, so a failure leaves that table untouched rather than
	// half-widened. Postgres does DDL transactionally, which is what makes this
	// hold — the same code on a backend without transactional DDL would not have
	// this property, and rls.Enforce refuses to start on those backends anyway.
	m := migrator.New(db.WithContext(ctx), &opts, Migrations())
	if err := m.Migrate(); err != nil {
		return fmt.Errorf("migrate: apply tenant migrations: %w", err)
	}
	return nil
}

// Unattributed counts rows with no tenant_id for every strictly-owned table.
//
// This is the operator's view of what the phase-2 gate will do, available before
// running it. Returned as a map rather than a first error so one pass reports every
// table needing work — the alternative is learning about them one deploy at a time.
//
// Runs in platform mode for the reason assertBackfilled does: once policies are
// installed, a count taken without it reads zero because the rows are invisible,
// not because they are attributed.
func Unattributed(ctx context.Context, db *gorm.DB) (map[string]int64, error) {
	out := make(map[string]int64)
	err := tenant.RunAcrossTenants(ctx, db, func(tx *gorm.DB) error {
		for _, table := range BClassTables {
			n, err := CountUnattributed(tx, table)
			if err != nil {
				return err
			}
			if n > 0 {
				out[table] = n
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
