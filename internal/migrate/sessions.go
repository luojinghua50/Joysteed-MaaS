package migrate

import (
	"fmt"

	"gorm.io/gorm"
)

// The sessions columns. Upstream's table carries ID / Token / TokenHash /
// ExpiresAt / timestamps / EncryptionStatus and nothing else — it answers "is
// this token valid", never "whose token is this" (audit §6.1).
//
// Under shared deployment (D4) tenant-portal sessions and platform-admin
// sessions land in that same undifferentiated table, which is why the audit ranks
// this the highest-priority migration and refuses to defer it: without these
// columns there is no auth boundary to enforce, only a valid-token check.
const (
	// SessionPrincipalTypeColumn distinguishes the two kinds of logged-in
	// subject. Not inferable from tenant_id being NULL: that is the encoding, and
	// an explicit column is what lets the database reject the combinations where
	// the two disagree (see the CHECK constraint below).
	SessionPrincipalTypeColumn = "principal_type"

	// SessionUserIDColumn identifies the human. Upstream has no user identity on
	// sessions at all, so this is new rather than widened.
	SessionUserIDColumn = "user_id"

	// PrincipalPlatformAdmin is the console operator. tenant_id IS NULL.
	PrincipalPlatformAdmin = "platform_admin"
	// PrincipalTenantUser is a tenant-portal user. tenant_id IS NOT NULL.
	PrincipalTenantUser = "tenant_user"

	sessionPrincipalCheck = "sessions_principal_type_check"
	sessionOwnershipCheck = "sessions_principal_tenant_consistency_check"
	sessionPrincipalIndex = "idx_sessions_principal"
)

// MigrateSessions adds the three ownership columns and the two constraints that
// keep them coherent.
//
// All columns are nullable: the table has live rows, and there is no correct
// default. A pre-migration session cannot be attributed — the information was
// never recorded — so the honest treatment is to leave it unattributed and let
// the deploy expire or purge those sessions. Backfilling them to platform_admin
// would hand console access to whoever held a tenant session; backfilling to any
// tenant would do the reverse.
func MigrateSessions(tx *gorm.DB) error {
	for _, stmt := range []string{
		fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s TEXT`,
			SessionsTable, TenantColumn),
		fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s TEXT`,
			SessionsTable, SessionPrincipalTypeColumn),
		fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s TEXT`,
			SessionsTable, SessionUserIDColumn),
	} {
		if err := tx.Exec(stmt).Error; err != nil {
			return fmt.Errorf("migrate: sessions columns (%q): %w", stmt, err)
		}
	}

	// Composite index, not the single-column one AddTenantColumn installs: every
	// session lookup that matters filters on both the principal type and the
	// tenant, and the platform-admin case filters on principal type with
	// tenant_id NULL, which a tenant_id-only index cannot serve.
	if err := tx.Exec(fmt.Sprintf(
		`CREATE INDEX IF NOT EXISTS %s ON %s (%s, %s)`,
		sessionPrincipalIndex, SessionsTable, SessionPrincipalTypeColumn, TenantColumn)).Error; err != nil {
		return fmt.Errorf("migrate: sessions principal index: %w", err)
	}

	return installSessionConstraints(tx)
}

// installSessionConstraints installs both CHECK constraints, dropped-then-added
// so a re-run applies a changed definition instead of skipping it.
func installSessionConstraints(tx *gorm.DB) error {
	stmts := []string{
		fmt.Sprintf(`ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s`,
			SessionsTable, sessionPrincipalCheck),
		// NULL is permitted: pre-migration rows have no principal type, and a
		// NOT VALID-style constraint that rejected them would make this migration
		// unrunnable on a live table.
		fmt.Sprintf(
			`ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s IS NULL OR %s IN ('%s', '%s'))`,
			SessionsTable, sessionPrincipalCheck,
			SessionPrincipalTypeColumn, SessionPrincipalTypeColumn,
			PrincipalPlatformAdmin, PrincipalTenantUser),

		fmt.Sprintf(`ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s`,
			SessionsTable, sessionOwnershipCheck),
		// This is the constraint that carries the auth boundary, and it is worth
		// more than the enum check above.
		//
		// Under PolicyExclusive, visibility is decided by tenant_id: bound
		// transactions see their own rows, unbound ones see tenant_id IS NULL. A
		// row whose principal_type and tenant_id disagree is therefore not merely
		// inconsistent — a tenant_user row with tenant_id NULL is filed as a
		// PLATFORM session, visible to the unbound console path and invisible to
		// the tenant it belongs to. That is a privilege escalation reachable by
		// writing one NULL, so the database refuses the combination outright.
		fmt.Sprintf(
			`ALTER TABLE %s ADD CONSTRAINT %s CHECK (
				%s IS NULL
				OR (%s = '%s' AND %s IS NULL)
				OR (%s = '%s' AND %s IS NOT NULL)
			)`,
			SessionsTable, sessionOwnershipCheck,
			SessionPrincipalTypeColumn,
			SessionPrincipalTypeColumn, PrincipalPlatformAdmin, TenantColumn,
			SessionPrincipalTypeColumn, PrincipalTenantUser, TenantColumn),
	}
	for _, s := range stmts {
		if err := tx.Exec(s).Error; err != nil {
			return fmt.Errorf("migrate: sessions constraints (%q): %w", s, err)
		}
	}
	return nil
}

// RollbackSessions reverses MigrateSessions.
func RollbackSessions(tx *gorm.DB) error {
	for _, s := range []string{
		fmt.Sprintf(`ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s`, SessionsTable, sessionOwnershipCheck),
		fmt.Sprintf(`ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s`, SessionsTable, sessionPrincipalCheck),
		fmt.Sprintf(`DROP INDEX IF EXISTS %s`, sessionPrincipalIndex),
		fmt.Sprintf(`ALTER TABLE %s DROP COLUMN IF EXISTS %s`, SessionsTable, SessionUserIDColumn),
		fmt.Sprintf(`ALTER TABLE %s DROP COLUMN IF EXISTS %s`, SessionsTable, SessionPrincipalTypeColumn),
		fmt.Sprintf(`ALTER TABLE %s DROP COLUMN IF EXISTS %s`, SessionsTable, TenantColumn),
	} {
		if err := tx.Exec(s).Error; err != nil {
			return fmt.Errorf("migrate: rollback sessions (%q): %w", s, err)
		}
	}
	return nil
}
