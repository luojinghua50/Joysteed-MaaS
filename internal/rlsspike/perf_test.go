package rlsspike_test

import (
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/rlsspike"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// The benchmarks below answer the cost half of D11b. RLS requires the tenant to
// be bound per transaction, which means every read path that today issues a
// bare SELECT must acquire a transaction it does not currently need. Three
// shapes are measured so the transaction cost and the policy cost are separable:
//
//	BenchmarkBareSelect          no transaction, RLS disabled  (today's shape)
//	BenchmarkTxSelect            transaction, RLS disabled     (isolates txn cost)
//	BenchmarkTxSetConfigSelect   transaction + set_config, RLS (the RLS shape)
//
// Run with:
//
//	go test ./tests/rlsspike/ -bench . -benchtime 2000x -run '^$'
//
// Numbers are only meaningful relative to each other on the same host: this is
// a container Postgres over loopback, so absolute latency understates a real
// deployment's network hop while the relative txn overhead stays representative.

func benchFixture(b *testing.B, rls bool) *gorm.DB {
	b.Helper()
	bootstrap, err := rlsspike.OpenBootstrap()
	require.NoError(b, err)
	require.NoError(b, rlsspike.EnsureRoles(bootstrap))

	owner, err := rlsspike.OpenOwner()
	require.NoError(b, err)
	require.NoError(b, rlsspike.Setup(owner, true))
	require.NoError(b, rlsspike.GrantApp(owner))
	if !rls {
		// Measure the transaction cost without the policy's predicate.
		require.NoError(b, owner.Exec(
			`ALTER TABLE spike_provider_keys DISABLE ROW LEVEL SECURITY`).Error)
	}

	app, err := rlsspike.OpenApp()
	require.NoError(b, err)
	return app
}

func readLabels(tb testing.TB, db *gorm.DB) {
	tb.Helper()
	var out []string
	if err := db.Model(&rlsspike.ProviderKey{}).Order("label").Pluck("label", &out).Error; err != nil {
		tb.Fatal(err)
	}
}

// BenchmarkBareSelect is the shape Bifrost's read paths have today: a single
// SELECT, no transaction, no tenant binding.
func BenchmarkBareSelect(b *testing.B) {
	app := benchFixture(b, false)
	b.ResetTimer()
	for range b.N {
		readLabels(b, app)
	}
}

// BenchmarkTxSelect adds only the transaction, so the delta against
// BenchmarkBareSelect is the BEGIN/COMMIT round-trip cost.
func BenchmarkTxSelect(b *testing.B) {
	app := benchFixture(b, false)
	b.ResetTimer()
	for range b.N {
		err := app.Transaction(func(tx *gorm.DB) error {
			readLabels(b, tx)
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTxSetConfigSelect is the full RLS shape: transaction, set_config to
// bind the tenant, then the same unscoped SELECT with policies enforcing.
func BenchmarkTxSetConfigSelect(b *testing.B) {
	app := benchFixture(b, true)
	b.ResetTimer()
	for range b.N {
		err := app.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec(
				`SELECT set_config('`+rlsspike.TenantIDSetting+`', ?, true)`, "tenant-a").Error; err != nil {
				return err
			}
			readLabels(b, tx)
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

// The two benchmarks below repeat the comparison against a read with real
// server-side work (a 20k-row aggregate), because the trivial-query pair above
// overstates the cost: the RLS shape adds a FIXED number of round trips, so its
// relative overhead shrinks as the query itself gets more expensive. Which of
// the two ratios applies to Bifrost depends on its actual read mix.

func seedBulk(b *testing.B, owner *gorm.DB, n int) {
	b.Helper()
	require.NoError(b, owner.Exec(
		`ALTER TABLE spike_provider_keys DISABLE ROW LEVEL SECURITY`).Error)
	require.NoError(b, owner.Exec(`
		INSERT INTO spike_provider_keys (tenant_id, provider, label)
		SELECT CASE WHEN i % 3 = 0 THEN NULL
		            WHEN i % 3 = 1 THEN 'tenant-a'
		            ELSE 'tenant-b' END,
		       'openai', 'bulk-' || i
		FROM generate_series(1, ?) AS s(i)`, n).Error)
	require.NoError(b, owner.Exec(
		`ALTER TABLE spike_provider_keys ENABLE ROW LEVEL SECURITY`).Error)
}

func heavyRead(tb testing.TB, db *gorm.DB) {
	tb.Helper()
	var n int64
	if err := db.Raw(
		`SELECT count(*) FROM spike_provider_keys WHERE label LIKE 'bulk-%'`).
		Scan(&n).Error; err != nil {
		tb.Fatal(err)
	}
}

// BenchmarkHeavyBareSelect is the heavier read in today's shape.
func BenchmarkHeavyBareSelect(b *testing.B) {
	app := benchFixture(b, false)
	owner, err := rlsspike.OpenOwner()
	require.NoError(b, err)
	seedBulk(b, owner, 20000)
	require.NoError(b, owner.Exec(
		`ALTER TABLE spike_provider_keys DISABLE ROW LEVEL SECURITY`).Error)
	b.ResetTimer()
	for range b.N {
		heavyRead(b, app)
	}
}

// BenchmarkHeavyTxSetConfigSelect is the heavier read in the RLS shape.
func BenchmarkHeavyTxSetConfigSelect(b *testing.B) {
	app := benchFixture(b, true)
	owner, err := rlsspike.OpenOwner()
	require.NoError(b, err)
	seedBulk(b, owner, 20000)
	b.ResetTimer()
	for range b.N {
		err := app.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec(
				`SELECT set_config('`+rlsspike.TenantIDSetting+`', ?, true)`, "tenant-a").Error; err != nil {
				return err
			}
			heavyRead(b, tx)
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTxSetConfigSelect_Pipelined folds the tenant binding into the same
// round trip as the read. If the naive shape's overhead is dominated by extra
// round trips rather than server work, this recovers most of it — which is the
// difference between "RLS costs one round trip" and "RLS costs three".
func BenchmarkTxSetConfigSelect_Pipelined(b *testing.B) {
	app := benchFixture(b, true)
	b.ResetTimer()
	for range b.N {
		err := app.Transaction(func(tx *gorm.DB) error {
			var out []string
			return tx.Raw(
				`WITH bound AS (SELECT set_config('`+rlsspike.TenantIDSetting+`', ?, true))
				 SELECT label FROM spike_provider_keys, bound ORDER BY label`,
				"tenant-a").Scan(&out).Error
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}
