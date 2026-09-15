package migrate_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/pglock"
	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// These tests run against Postgres. Not a preference: they assert on RLS
// policies, FORCE, and CHECK constraints, none of which SQLite has. The same
// assertions would pass there while proving nothing (R34).
//
//	docker run -d --name maas-rls-spike \
//	  -e POSTGRES_PASSWORD=spike_password -e POSTGRES_USER=spike \
//	  -e POSTGRES_DB=spike -p 55432:5432 postgres:16-alpine

const (
	appRole = "migrate_app"
	appPass = "migrate_app_password"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func dsn(user, password string) string {
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		env("MAAS_SPIKE_PG_HOST", "localhost"),
		env("MAAS_SPIKE_PG_PORT", "55432"),
		user, password,
		env("MAAS_SPIKE_PG_DB", "spike"))
}

func open(t *testing.T, user, password string) *gorm.DB {
	return openWithConns(t, user, password, 4)
}

// openWithConns pins the pool size. Passing 1 makes connection REUSE
// deterministic, which is the only way to observe state a previous transaction
// left behind on a physical connection.
func openWithConns(t *testing.T, user, password string, maxConns int) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(postgres.Open(dsn(user, password)), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err, "start the spike Postgres first (see the file header)")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(maxConns)
	sqlDB.SetMaxIdleConns(maxConns)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// fixture creates a table in the shape of an upstream one (no tenant column) and
// returns an owner handle plus an app-role handle.
//
// The app role matters: it is NOT the table owner and not a superuser. Reading
// through the owner would prove nothing even with FORCE set, and reading through
// a superuser would prove nothing at all — superusers bypass RLS unconditionally.
// Production reads happen through exactly this kind of role.
func fixture(t *testing.T, table string, extraCols string) (owner, app *gorm.DB) {
	t.Helper()
	owner = open(t, env("MAAS_SPIKE_PG_USER", "spike"),
		env("MAAS_SPIKE_PG_PASSWORD", "spike_password"))

	stmts := []string{
		fmt.Sprintf(`DO $$ BEGIN
			IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%s') THEN
				CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOBYPASSRLS;
			END IF;
		END $$`, appRole, appRole, appPass),
		fmt.Sprintf(`DROP TABLE IF EXISTS %s`, table),
		fmt.Sprintf(`CREATE TABLE %s (id BIGSERIAL PRIMARY KEY, label TEXT NOT NULL%s)`,
			table, extraCols),
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON %s TO %s`, table, appRole),
		fmt.Sprintf(`GRANT USAGE, SELECT ON SEQUENCE %s_id_seq TO %s`, table, appRole),
	}
	// Under the shared-catalog lock: this GRANT updates the single pg_namespace row
	// for `public`, which other test packages' bootstraps also update. Postgres
	// does not do that under MVCC, so concurrent packages abort with "tuple
	// concurrently updated" (see internal/pglock).
	require.NoError(t, pglock.WithSharedCatalog(owner, func(tx *gorm.DB) error {
		return tx.Exec(fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, appRole)).Error
	}))
	for _, s := range stmts {
		require.NoError(t, owner.Exec(s).Error, s)
	}
	t.Cleanup(func() { _ = owner.Exec(fmt.Sprintf(`DROP TABLE IF EXISTS %s`, table)).Error })

	app = open(t, appRole, appPass)
	return owner, app
}

// seed inserts rows as the owner with RLS off, so the fixture does not depend on
// the very policy under test.
func seed(t *testing.T, owner *gorm.DB, table string, rows []struct {
	tenantID *string
	label    string
}) {
	t.Helper()
	require.NoError(t, owner.Exec(fmt.Sprintf(
		`ALTER TABLE %s DISABLE ROW LEVEL SECURITY`, table)).Error)
	for _, r := range rows {
		require.NoError(t, owner.Exec(fmt.Sprintf(
			`INSERT INTO %s (tenant_id, label) VALUES (?, ?)`, table),
			r.tenantID, r.label).Error)
	}
	require.NoError(t, owner.Exec(fmt.Sprintf(
		`ALTER TABLE %s ENABLE ROW LEVEL SECURITY`, table)).Error)
}

// labelsVisibleTo reads through RunInTenantTx, i.e. the real binding path rather
// than a hand-written SET in the test.
func labelsVisibleTo(t *testing.T, app *gorm.DB, table, tenantID string) []string {
	t.Helper()
	ctx := tenant.WithTenant(context.Background(), tenant.ID(tenantID))
	var labels []string
	require.NoError(t, tenant.RunInTenantTx(ctx, app, func(tx *gorm.DB) error {
		return tx.Raw(fmt.Sprintf(
			`SELECT label FROM %s ORDER BY label`, table)).Scan(&labels).Error
	}))
	return labels
}

// labelsVisibleToPlatform reads with no tenant bound.
func labelsVisibleToPlatform(t *testing.T, app *gorm.DB, table string) []string {
	t.Helper()
	var labels []string
	require.NoError(t, tenant.RunAsPlatform(context.Background(), app, func(tx *gorm.DB) error {
		return tx.Raw(fmt.Sprintf(
			`SELECT label FROM %s ORDER BY label`, table)).Scan(&labels).Error
	}))
	return labels
}
