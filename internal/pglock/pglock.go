// Package pglock serialises DDL that contends on a shared Postgres catalog row.
package pglock

import (
	"fmt"

	"gorm.io/gorm"
)

// sharedCatalogKey is the advisory lock every caller must agree on.
//
// The value is arbitrary; that all callers use the SAME value is the entire point,
// which is why it lives here rather than being written out at each call site. Two
// packages picking different numbers would take two different locks and serialise
// against nobody.
const sharedCatalogKey = 4242424242

// WithSharedCatalog runs fn holding a transaction-scoped advisory lock, for DDL that
// updates a catalog row shared across test packages.
//
// The problem it solves: Postgres updates catalog rows WITHOUT MVCC. `GRANT ... ON
// SCHEMA public` updates the single pg_namespace row for `public`, so two sessions
// doing it concurrently abort with "tuple concurrently updated" (SQLSTATE XX000).
// Our test packages each bootstrap their own roles and grants, and `go test ./...`
// runs different packages in PARALLEL — so they collide. Measured: 8 concurrent
// unguarded grants reproduce the error on every attempt; through this function, zero
// failures across the same load.
//
// Note this is NOT a row-lock substitute or an application-level primitive. It
// guards test/bootstrap DDL only. Ordinary data access needs none of it, because
// ordinary data access is MVCC.
//
// A lock rather than a retry loop: both work here since the guarded statements are
// idempotent, but a lock makes the outcome deterministic instead of probabilistic,
// and a flake that reappears under load is exactly what this is meant to remove.
//
// pg_advisory_xact_lock rather than the session-scoped pg_advisory_lock: the
// transaction variant is released on COMMIT or ROLLBACK, so a failing bootstrap
// cannot strand the lock and wedge every other package. It also has to run inside a
// transaction to hold the same pooled connection for lock and unlock, which is why
// fn is handed a tx.
func WithSharedCatalog(db *gorm.DB, fn func(tx *gorm.DB) error) error {
	if db == nil {
		return fmt.Errorf("pglock: db is nil")
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(?)`, sharedCatalogKey).Error; err != nil {
			return fmt.Errorf("pglock: acquire shared catalog lock: %w", err)
		}
		return fn(tx)
	})
}
