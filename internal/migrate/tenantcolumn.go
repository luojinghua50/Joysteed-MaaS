// Package migrate widens upstream Bifrost tables with the tenant discriminator
// and installs the RLS policies that enforce it.
//
// Nothing here modifies upstream source. The columns are added to the tables
// Bifrost's own migrations created, through the RunMigration hook upstream
// documents for exactly this ("downstream consumers (e.g. bifrost-enterprise)
// run their migrations via this hook", framework/configstore/postgres.go:81).
package migrate

import (
	"fmt"
	"regexp"

	"gorm.io/gorm"
)

// TenantColumn is the discriminator added to every tenant-scoped table.
const TenantColumn = "tenant_id"

// maxIdentifierLen is Postgres's NAMEDATALEN-1. Generated index names are
// checked against it rather than left to be truncated: truncation is silent, and
// two long table names can truncate to the SAME index name, at which point the
// second CREATE INDEX either fails or — with IF NOT EXISTS — succeeds as a no-op
// and leaves that table with no index at all.
const maxIdentifierLen = 63

// safeIdentifier matches the shape of every table name this package accepts.
//
// Table names here come from our own audit-derived list, not from user input, so
// this is not the primary defence. It is a guard on DDL construction: ALTER TABLE
// cannot take bind parameters, so these names are concatenated into statement
// text, and a name that ever came to contain a quote would make schema migration
// itself an injection sink.
var safeIdentifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

func checkIdentifier(name string) error {
	if !safeIdentifier.MatchString(name) {
		return fmt.Errorf("migrate: %q is not a safe SQL identifier", name)
	}
	if len(name) > maxIdentifierLen {
		return fmt.Errorf("migrate: identifier %q exceeds %d bytes", name, maxIdentifierLen)
	}
	return nil
}

// tenantIndexName is the single-column index backing per-tenant lookups.
//
// Note what this is NOT: the composite indexes R10 calls for, with tenant_id in
// first position ahead of each table's existing lookup columns. Those cannot be
// generated, because the columns to follow tenant_id differ per table and have
// to be read off that table's actual query patterns. This index makes tenant
// filtering indexed rather than sequential; the composite ones are per-table
// follow-up work.
func tenantIndexName(table string) string {
	return "idx_" + table + "_tenant_id"
}

// AddTenantColumn adds a nullable tenant_id plus its index to an existing table.
//
// Raw DDL rather than migrator.AddColumnIfNotExists, which MAAS_TECH_DESIGN.md
// §3.4 and MAAS_TABLE_AUDIT.md §8 both prescribe. That helper resolves the column
// definition from the Go struct field via stmt.Schema.LookUpField and returns
// "failed to look up field" when the field is absent. Every table widened here
// has no TenantID field on its upstream struct — that absence is the entire
// reason the migration exists — so the helper cannot apply. It fits the case it
// was written for (batch_jobs, where the struct already declared the columns and
// only the database lagged), which is the inverse of this one.
//
// The column is added NULLABLE even for tables that must end up NOT NULL. A NOT
// NULL column cannot be added to a populated table without a default, and a
// default here would mean attributing every historical row to one tenant — which
// for config_keys or credential tables is not untidy data but a cross-tenant
// leak. Nullable now, backfilled per-table, then SetTenantColumnNotNull.
func AddTenantColumn(tx *gorm.DB, table string) error {
	if err := checkIdentifier(table); err != nil {
		return err
	}
	index := tenantIndexName(table)
	if err := checkIdentifier(index); err != nil {
		return err
	}

	if err := tx.Exec(fmt.Sprintf(
		`ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s TEXT`, table, TenantColumn)).Error; err != nil {
		return fmt.Errorf("migrate: add %s to %s: %w", TenantColumn, table, err)
	}
	// IF NOT EXISTS on both statements rather than a HasColumn guard: the guard
	// is check-then-act, and two migration runners during a rolling deploy can
	// both observe the column missing and both issue the ADD, aborting the
	// loser's surrounding transaction (SQLSTATE 42701). Upstream's own helper
	// documents this same reasoning.
	if err := tx.Exec(fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS %s ON %s (%s)`, index, table, TenantColumn)).Error; err != nil {
		return fmt.Errorf("migrate: index %s on %s: %w", TenantColumn, table, err)
	}
	return nil
}

// DropTenantColumn reverses AddTenantColumn.
func DropTenantColumn(tx *gorm.DB, table string) error {
	if err := checkIdentifier(table); err != nil {
		return err
	}
	index := tenantIndexName(table)
	if err := checkIdentifier(index); err != nil {
		return err
	}
	if err := tx.Exec(fmt.Sprintf(`DROP INDEX IF EXISTS %s`, index)).Error; err != nil {
		return fmt.Errorf("migrate: drop index on %s: %w", table, err)
	}
	if err := tx.Exec(fmt.Sprintf(
		`ALTER TABLE %s DROP COLUMN IF EXISTS %s`, table, TenantColumn)).Error; err != nil {
		return fmt.Errorf("migrate: drop %s from %s: %w", TenantColumn, table, err)
	}
	return nil
}

// SetTenantColumnNotNull tightens tenant_id after that table's backfill.
//
// Runs as its own step, after backfill and never in the same migration as the ADD.
// Postgres validates the constraint against existing rows, so on a table with any
// unattributed row this fails — which is the intended behaviour: an unattributed
// row means the backfill rules did not cover it, and the alternative to failing
// here is a row nobody can see and nobody knows about.
//
// Not for config_keys, whose NULL is meaningful (D1: NULL = platform pool).
func SetTenantColumnNotNull(tx *gorm.DB, table string) error {
	if err := checkIdentifier(table); err != nil {
		return err
	}
	if err := tx.Exec(fmt.Sprintf(
		`ALTER TABLE %s ALTER COLUMN %s SET NOT NULL`, table, TenantColumn)).Error; err != nil {
		return fmt.Errorf("migrate: set %s NOT NULL on %s (unattributed rows block this): %w",
			table, TenantColumn, err)
	}
	return nil
}

// DropTenantColumnNotNull reverses SetTenantColumnNotNull.
//
// Loosening a constraint always succeeds regardless of the data, which makes this
// the safe half of the pair — but note what it does not undo: any composite
// foreign key created while the column was NOT NULL stays in place and silently
// stops covering unattributed rows, because MATCH SIMPLE skips a row with a NULL
// in the referencing columns. So a rollback that stops here leaves constraints
// that look present and check less than they did. The phase-3 set orders its
// rollbacks accordingly (see TighteningMigrations).
func DropTenantColumnNotNull(tx *gorm.DB, table string) error {
	if err := checkIdentifier(table); err != nil {
		return err
	}
	if err := tx.Exec(fmt.Sprintf(
		`ALTER TABLE %s ALTER COLUMN %s DROP NOT NULL`, table, TenantColumn)).Error; err != nil {
		return fmt.Errorf("migrate: drop NOT NULL on %s.%s: %w", table, TenantColumn, err)
	}
	return nil
}

// CountUnattributed reports how many rows have no tenant_id, so a backfill can be
// verified before the NOT NULL step rather than discovered by its failure.
func CountUnattributed(tx *gorm.DB, table string) (int64, error) {
	if err := checkIdentifier(table); err != nil {
		return 0, err
	}
	var n int64
	if err := tx.Raw(fmt.Sprintf(
		`SELECT count(*) FROM %s WHERE %s IS NULL`, table, TenantColumn)).Scan(&n).Error; err != nil {
		return 0, fmt.Errorf("migrate: count unattributed rows in %s: %w", table, err)
	}
	return n, nil
}
