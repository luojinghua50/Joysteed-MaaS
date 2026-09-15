package tenant_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/pglock"
	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// These tests run against Postgres with real RLS, not SQLite. That is a
// requirement, not a preference: SQLite has no row-level security, so the same
// assertions would pass there while proving nothing about isolation
// (MAAS_TECH_DESIGN.md §9.2.5, risk R34).
//
//	docker run -d --name maas-rls-spike \
//	  -e POSTGRES_PASSWORD=spike_password -e POSTGRES_USER=spike \
//	  -e POSTGRES_DB=spike -p 55432:5432 postgres:16-alpine

const (
	bindTable   = "binding_keys"
	bindAppRole = "binding_app"
	bindAppPass = "binding_app_password"
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

// openDB returns a pool of exactly maxConns connections. Pinning it small is
// what makes connection REUSE deterministic, which is the precondition for
// observing a leaked session variable at all.
func openDB(t *testing.T, user, password string, maxConns int) *gorm.DB {
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

// fixture builds a tenant table under RLS + FORCE, owned by a non-superuser
// role, seeded with one platform row and one row for each of two tenants.
func fixture(t *testing.T, maxConns int) *gorm.DB {
	t.Helper()

	root := openDB(t, env("MAAS_SPIKE_PG_USER", "spike"),
		env("MAAS_SPIKE_PG_PASSWORD", "spike_password"), 2)
	// Under the shared-catalog lock: the GRANT below updates the single
	// pg_namespace row for `public`, which other test packages' bootstraps also
	// update. Postgres does not do that under MVCC, so concurrent packages abort
	// with "tuple concurrently updated" (see internal/pglock).
	require.NoError(t, pglock.WithSharedCatalog(root, func(tx *gorm.DB) error {
		for _, s := range []string{
			fmt.Sprintf(`DO $$ BEGIN
				IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%s') THEN
					CREATE ROLE %s LOGIN PASSWORD '%s';
				END IF;
			END $$`, bindAppRole, bindAppRole, bindAppPass),
			fmt.Sprintf(`GRANT USAGE, CREATE ON SCHEMA public TO %s`, bindAppRole),
			fmt.Sprintf(`DROP TABLE IF EXISTS %s`, bindTable),
		} {
			if err := tx.Exec(s).Error; err != nil {
				return fmt.Errorf("%s: %w", s, err)
			}
		}
		return nil
	}))

	app := openDB(t, bindAppRole, bindAppPass, maxConns)
	for _, s := range []string{
		fmt.Sprintf(`CREATE TABLE %s (
			id BIGSERIAL PRIMARY KEY,
			tenant_id TEXT,
			label TEXT NOT NULL
		)`, bindTable),
		fmt.Sprintf(`INSERT INTO %s (tenant_id, label) VALUES
			(NULL, 'platform-pool'),
			('tenant-a', 'a-byok'),
			('tenant-b', 'b-byok')`, bindTable),
		fmt.Sprintf(`ALTER TABLE %s ENABLE ROW LEVEL SECURITY`, bindTable),
		// Uses tenant.SettingName, so a drift between policy and runtime would
		// break these tests rather than silently returning empty results.
		fmt.Sprintf(`CREATE POLICY tenant_isolation ON %s FOR ALL
			USING (tenant_id IS NULL OR tenant_id = current_setting('%s', true))`,
			bindTable, tenant.SettingName),
		fmt.Sprintf(`ALTER TABLE %s FORCE ROW LEVEL SECURITY`, bindTable),
	} {
		require.NoError(t, app.Exec(s).Error, s)
	}
	return app
}

func labels(t *testing.T, tx *gorm.DB) []string {
	t.Helper()
	var out []string
	require.NoError(t, tx.Table(bindTable).Order("label").Pluck("label", &out).Error)
	return out
}

func TestRunInTenantTx_ThreePartyIsolation(t *testing.T) {
	db := fixture(t, 4)

	ctx := tenant.WithTenant(context.Background(), "tenant-a")
	require.NoError(t, tenant.RunInTenantTx(ctx, db, func(tx *gorm.DB) error {
		got := labels(t, tx)
		require.ElementsMatch(t, []string{"platform-pool", "a-byok"}, got)
		require.NotContains(t, got, "b-byok")
		return nil
	}))
}

// TestRunInTenantTx_FailsClosedWithoutTenant covers the resolution-chain bug:
// proceeding unbound would read as empty rather than erroring, hiding the bug.
func TestRunInTenantTx_FailsClosedWithoutTenant(t *testing.T) {
	db := fixture(t, 2)

	called := false
	err := tenant.RunInTenantTx(context.Background(), db, func(tx *gorm.DB) error {
		called = true
		return nil
	})
	require.ErrorIs(t, err, tenant.ErrMissingTenant)
	require.False(t, called, "fn must not run when no tenant is resolved")
}

// TestRunInTenantTx_BindingDoesNotLeakOntoPooledConnection is the most
// important test in this file. It pins the leak the spike demonstrated: a
// session-scoped binding survives on the pooled connection, so the NEXT request
// served by it inherits the previous tenant's identity — a cross-tenant read
// with no error anywhere.
//
// maxConns=1 forces every operation onto the same physical connection, making
// reuse deterministic. A real pool reuses non-deterministically, which is worse:
// the leak would be intermittent.
func TestRunInTenantTx_BindingDoesNotLeakOntoPooledConnection(t *testing.T) {
	db := fixture(t, 1)

	ctxA := tenant.WithTenant(context.Background(), "tenant-a")
	require.NoError(t, tenant.RunInTenantTx(ctxA, db, func(tx *gorm.DB) error {
		bound, err := tenant.BoundTenant(tx)
		require.NoError(t, err)
		require.Equal(t, "tenant-a", bound, "premise: tenant must be bound inside the tx")
		return nil
	}))

	// Same connection, after the transaction committed.
	leaked, err := tenant.BoundTenant(db)
	require.NoError(t, err)
	require.Empty(t, leaked,
		"tenant binding leaked onto the pooled connection: the next request on this "+
			"connection would read as tenant-a")

	// And the leak must not be observable as data either: an unbound read sees
	// only the platform pool, never tenant-a's rows.
	require.NoError(t, tenant.RunAsPlatform(context.Background(), db, func(tx *gorm.DB) error {
		require.Equal(t, []string{"platform-pool"}, labels(t, tx),
			"an unbound transaction must not inherit the previous tenant's visibility")
		return nil
	}))
}

// TestRunInTenantTx_SequentialTenantsOnOneConnection is the leak test's
// practical form: two tenants served back to back on a single connection, which
// is exactly what a busy node with a small pool does.
func TestRunInTenantTx_SequentialTenantsOnOneConnection(t *testing.T) {
	db := fixture(t, 1)

	for _, tc := range []struct {
		tenant string
		want   []string
	}{
		{"tenant-a", []string{"a-byok", "platform-pool"}},
		{"tenant-b", []string{"b-byok", "platform-pool"}},
		{"tenant-a", []string{"a-byok", "platform-pool"}},
	} {
		ctx := tenant.WithTenant(context.Background(), tenant.ID(tc.tenant))
		require.NoError(t, tenant.RunInTenantTx(ctx, db, func(tx *gorm.DB) error {
			require.ElementsMatchf(t, tc.want, labels(t, tx),
				"tenant %s saw the wrong rows on a reused connection", tc.tenant)
			return nil
		}))
	}
}

// TestRunInTenantTx_ConcurrentTenantsStayIsolated is the contended version.
// Serial calls can pass by accident of ordering; 24 goroutines over a 4
// connection pool cannot.
func TestRunInTenantTx_ConcurrentTenantsStayIsolated(t *testing.T) {
	db := fixture(t, 4)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 24 {
		id, want := "tenant-a", "a-byok"
		if i%2 == 1 {
			id, want = "tenant-b", "b-byok"
		}
		wg.Add(1)
		go func(id, want string) {
			defer wg.Done()
			<-start
			ctx := tenant.WithTenant(context.Background(), tenant.ID(id))
			err := tenant.RunInTenantTx(ctx, db, func(tx *gorm.DB) error {
				got := labels(t, tx)
				if len(got) != 2 {
					return fmt.Errorf("%s saw %d rows: %v", id, len(got), got)
				}
				for _, l := range got {
					if l != want && l != "platform-pool" {
						return fmt.Errorf("%s saw foreign row %q", id, l)
					}
				}
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}(id, want)
	}
	close(start)
	wg.Wait()
}

// TestRunAsPlatform_SeesOnlyPlatformRows documents that the cross-tenant escape
// hatch is not a master key. Under these policies an unbound transaction sees
// the platform pool and nothing else, so forgetting to bind fails closed.
func TestRunAsPlatform_SeesOnlyPlatformRows(t *testing.T) {
	db := fixture(t, 2)

	require.NoError(t, tenant.RunAsPlatform(context.Background(), db, func(tx *gorm.DB) error {
		require.Equal(t, []string{"platform-pool"}, labels(t, tx))
		bound, err := tenant.BoundTenant(tx)
		require.NoError(t, err)
		require.Empty(t, bound)
		return nil
	}))
}

// TestRunInTenantTx_RollbackDiscardsBinding checks that a failed transaction
// leaves no binding behind, since rollback is the path an error takes.
func TestRunInTenantTx_RollbackDiscardsBinding(t *testing.T) {
	db := fixture(t, 1)

	sentinel := fmt.Errorf("deliberate failure")
	ctx := tenant.WithTenant(context.Background(), "tenant-a")
	err := tenant.RunInTenantTx(ctx, db, func(tx *gorm.DB) error { return sentinel })
	require.ErrorIs(t, err, sentinel)

	leaked, err := tenant.BoundTenant(db)
	require.NoError(t, err)
	require.Empty(t, leaked, "a rolled-back transaction must not leave a binding")
}

// TestRunInTenantTx_TenantIsParameterNotInterpolated covers R32. A tenant id
// containing SQL metacharacters must be treated as data. If binding were built
// by string concatenation (the only way to write it as SET LOCAL), this would
// either error or bind something else.
func TestRunInTenantTx_TenantIsParameterNotInterpolated(t *testing.T) {
	db := fixture(t, 2)

	hostile := `'; DROP TABLE ` + bindTable + `; --`
	ctx := tenant.WithTenant(context.Background(), tenant.ID(hostile))
	require.NoError(t, tenant.RunInTenantTx(ctx, db, func(tx *gorm.DB) error {
		bound, err := tenant.BoundTenant(tx)
		require.NoError(t, err)
		require.Equal(t, hostile, bound, "the id must arrive verbatim, as a parameter")
		// No tenant owns rows under that id, so only the platform pool is visible.
		require.Equal(t, []string{"platform-pool"}, labels(t, tx))
		return nil
	}))

	// The table must still exist.
	require.NoError(t, tenant.RunAsPlatform(context.Background(), db, func(tx *gorm.DB) error {
		var n int64
		return tx.Table(bindTable).Count(&n).Error
	}))
}

// TestBoundTenant_OutsideTransactionReportsUnbound pins the invariant the
// binding design rests on: nothing is bound at the pool level, ever.
func TestBoundTenant_OutsideTransactionReportsUnbound(t *testing.T) {
	db := fixture(t, 1)

	bound, err := tenant.BoundTenant(db)
	require.NoError(t, err)
	require.Empty(t, bound)
}
