package migrate_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/migrate"
	"github.com/luojinghua50/Joysteed-MaaS/internal/pglock"
	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// sessionsFixture builds a table in upstream's exact sessions shape — no
// ownership column of any kind — and seeds one pre-existing row.
//
// The pre-existing row is the point: this migration has to run against a
// populated live table, which is why every column it adds is nullable. A test on
// an empty table would not exercise that constraint at all.
func sessionsFixture(t *testing.T) (owner, app *gorm.DB) {
	t.Helper()
	owner = open(t, env("MAAS_SPIKE_PG_USER", "spike"),
		env("MAAS_SPIKE_PG_PASSWORD", "spike_password"))

	for _, s := range []string{
		fmt.Sprintf(`DO $$ BEGIN
			IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%s') THEN
				CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOBYPASSRLS;
			END IF;
		END $$`, appRole, appRole, appPass),
		`DROP TABLE IF EXISTS sessions`,
		`CREATE TABLE sessions (
			id BIGSERIAL PRIMARY KEY,
			token TEXT NOT NULL UNIQUE,
			token_hash VARCHAR(64),
			expires_at TIMESTAMPTZ NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			encryption_status VARCHAR(20) DEFAULT 'plain_text'
		)`,
		`INSERT INTO sessions (token, expires_at) VALUES ('pre-migration-token', now() + interval '1 day')`,
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON sessions TO %s`, appRole),
		fmt.Sprintf(`GRANT USAGE, SELECT ON SEQUENCE sessions_id_seq TO %s`, appRole),
	} {
		require.NoError(t, owner.Exec(s).Error, s)
	}
	// Under the shared-catalog lock: this GRANT updates the single pg_namespace row
	// for `public`, which other test packages' bootstraps also update. Postgres
	// does not do that under MVCC, so concurrent packages abort with "tuple
	// concurrently updated" (see internal/pglock).
	require.NoError(t, pglock.WithSharedCatalog(owner, func(tx *gorm.DB) error {
		return tx.Exec(fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, appRole)).Error
	}))
	t.Cleanup(func() { _ = owner.Exec(`DROP TABLE IF EXISTS sessions`).Error })

	app = open(t, appRole, appPass)
	return owner, app
}

func TestMigrateSessions_AddsOwnershipColumnsIdempotently(t *testing.T) {
	owner, _ := sessionsFixture(t)
	require.NoError(t, migrate.MigrateSessions(owner))
	require.NoError(t, migrate.MigrateSessions(owner), "must be re-runnable")

	for _, col := range []string{
		migrate.TenantColumn,
		migrate.SessionPrincipalTypeColumn,
		migrate.SessionUserIDColumn,
	} {
		var n int64
		require.NoError(t, owner.Raw(`SELECT count(*) FROM information_schema.columns
			WHERE table_name = 'sessions' AND column_name = ?`, col).Scan(&n).Error)
		require.Equal(t, int64(1), n, col)
	}

	// The pre-migration row survives, unattributed. Nothing guessed an owner for
	// it: attributing it to platform_admin would hand console access to whoever
	// held a tenant session, and attributing it to any tenant does the reverse.
	var unattributed int64
	require.NoError(t, owner.Raw(`SELECT count(*) FROM sessions
		WHERE principal_type IS NULL AND tenant_id IS NULL`).Scan(&unattributed).Error)
	require.Equal(t, int64(1), unattributed)
}

// TestMigrateSessions_OwnershipCheckRejectsPrivilegeEscalation is the highest-value
// test for this migration.
//
// Under PolicyExclusive, visibility is decided by tenant_id alone: unbound reads
// see tenant_id IS NULL. So a tenant_user row written with tenant_id NULL is filed
// as a PLATFORM session — visible to the unbound console path, invisible to the
// tenant it belongs to. That is privilege escalation reachable by writing one
// NULL, and no Go-side validation can prevent it for writers that do not go
// through our code. The database has to refuse the combination.
func TestMigrateSessions_OwnershipCheckRejectsPrivilegeEscalation(t *testing.T) {
	owner, _ := sessionsFixture(t)
	require.NoError(t, migrate.MigrateSessions(owner))

	insert := func(token, principal string, tenantID *string) error {
		return owner.Exec(
			`INSERT INTO sessions (token, expires_at, principal_type, tenant_id, user_id)
			 VALUES (?, now() + interval '1 day', ?, ?, 'u-1')`,
			token, principal, tenantID).Error
	}

	// The escalation: a tenant user filed as a platform session.
	require.Error(t,
		insert("escalation", migrate.PrincipalTenantUser, nil),
		"a tenant_user with NULL tenant_id would be filed as a platform session")

	// The inverse: a platform admin carrying a tenant, which would make console
	// sessions visible to that tenant instead of to the platform.
	require.Error(t,
		insert("misfiled-admin", migrate.PrincipalPlatformAdmin, ptr("tenant-a")),
		"a platform_admin must not carry a tenant")

	// Both coherent combinations are accepted.
	require.NoError(t, insert("good-tenant", migrate.PrincipalTenantUser, ptr("tenant-a")))
	require.NoError(t, insert("good-admin", migrate.PrincipalPlatformAdmin, nil))
}

func TestMigrateSessions_PrincipalTypeCheckRejectsUnknownValues(t *testing.T) {
	owner, _ := sessionsFixture(t)
	require.NoError(t, migrate.MigrateSessions(owner))

	for _, bad := range []string{"admin", "PLATFORM_ADMIN", "tenant", "root", ""} {
		err := owner.Exec(
			`INSERT INTO sessions (token, expires_at, principal_type, tenant_id)
			 VALUES (?, now() + interval '1 day', ?, NULL)`,
			"bad-"+bad, bad).Error
		require.Error(t, err, "principal_type %q must be rejected", bad)
	}
}

// TestMigrateSessions_PolicyPartitionsAdminAndTenantSessions runs the real
// sessions table through the policy the audit's §6.1 requirement reduces to:
// under shared deployment (D4), tenant-portal and platform-admin sessions live in
// one table and must not see each other.
func TestMigrateSessions_PolicyPartitionsAdminAndTenantSessions(t *testing.T) {
	owner, app := sessionsFixture(t)
	require.NoError(t, migrate.MigrateSessions(owner))

	kind, ok := migrate.PolicyKindFor(migrate.SessionsTable)
	require.True(t, ok, "sessions must be classified")
	require.Equal(t, migrate.PolicyExclusive, kind)
	require.NoError(t, migrate.InstallPolicy(owner, migrate.SessionsTable, kind))

	require.NoError(t, owner.Exec(`ALTER TABLE sessions DISABLE ROW LEVEL SECURITY`).Error)
	for _, r := range []struct {
		token     string
		principal string
		tenantID  *string
	}{
		{"admin-session", migrate.PrincipalPlatformAdmin, nil},
		{"a-session", migrate.PrincipalTenantUser, ptr("tenant-a")},
		{"b-session", migrate.PrincipalTenantUser, ptr("tenant-b")},
	} {
		require.NoError(t, owner.Exec(
			`INSERT INTO sessions (token, expires_at, principal_type, tenant_id)
			 VALUES (?, now() + interval '1 day', ?, ?)`,
			r.token, r.principal, r.tenantID).Error)
	}
	require.NoError(t, owner.Exec(`ALTER TABLE sessions ENABLE ROW LEVEL SECURITY`).Error)

	tokens := func(db *gorm.DB, tenantID string) []string {
		var out []string
		read := func(tx *gorm.DB) error {
			return tx.Raw(`SELECT token FROM sessions ORDER BY token`).Scan(&out).Error
		}
		if tenantID == "" {
			require.NoError(t, tenant.RunAsPlatform(context.Background(), db, read))
		} else {
			ctx := tenant.WithTenant(context.Background(), tenant.ID(tenantID))
			require.NoError(t, tenant.RunInTenantTx(ctx, db, read))
		}
		return out
	}

	require.Equal(t, []string{"a-session"}, tokens(app, "tenant-a"),
		"a tenant sees only its own sessions, never the admin's")
	require.Equal(t, []string{"b-session"}, tokens(app, "tenant-b"))

	// The platform sees admin sessions plus the unattributed pre-migration row,
	// and no tenant session.
	platform := tokens(app, "")
	require.Contains(t, platform, "admin-session")
	require.Contains(t, platform, "pre-migration-token")
	require.NotContains(t, platform, "a-session")
	require.NotContains(t, platform, "b-session")
}

func TestRollbackSessions_RemovesColumnsAndConstraints(t *testing.T) {
	owner, _ := sessionsFixture(t)
	require.NoError(t, migrate.MigrateSessions(owner))
	require.NoError(t, migrate.RollbackSessions(owner))

	for _, col := range []string{
		migrate.TenantColumn,
		migrate.SessionPrincipalTypeColumn,
		migrate.SessionUserIDColumn,
	} {
		var n int64
		require.NoError(t, owner.Raw(`SELECT count(*) FROM information_schema.columns
			WHERE table_name = 'sessions' AND column_name = ?`, col).Scan(&n).Error)
		require.Zero(t, n, col)
	}
	var constraints int64
	require.NoError(t, owner.Raw(`SELECT count(*) FROM pg_constraint
		WHERE conname LIKE 'sessions_principal%'`).Scan(&constraints).Error)
	require.Zero(t, constraints)
}
