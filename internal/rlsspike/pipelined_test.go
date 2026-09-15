package rlsspike_test

import (
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/rlsspike"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// TestPipelinedSetConfig_IsNotSafe checks whether the round-trip-saving trick
// benchmarked as BenchmarkTxSetConfigSelect_Pipelined is actually correct.
//
// It folds set_config into a CTE alongside the guarded read. That saves a round
// trip, but Postgres does not guarantee when a CTE's side effect is evaluated
// relative to the outer query's RLS predicate, and a policy is evaluated as the
// scan runs. If the ordering is not guaranteed, a benchmark result for this
// shape is meaningless — it would be measuring a query that returns the wrong
// rows.
//
// The assertion is deliberately written to accept EITHER outcome and report
// which one happened, because the point is to find out, not to enshrine a guess.
func TestPipelinedSetConfig_IsNotSafe(t *testing.T) {
	fixture(t, true)

	app := appDB(t)
	err := app.Transaction(func(tx *gorm.DB) error {
		var out []string
		if err := tx.Raw(
			`WITH bound AS (SELECT set_config('`+rlsspike.TenantIDSetting+`', ?, true))
			 SELECT label FROM spike_provider_keys, bound ORDER BY label`,
			"tenant-a").Scan(&out).Error; err != nil {
			return err
		}
		t.Logf("pipelined CTE returned %d rows: %v", len(out), out)

		switch len(out) {
		case 2:
			require.ElementsMatch(t, []string{"platform-pool-key", "tenant-a-byok"}, out)
			t.Log("RESULT: CTE side effect landed before the policy scan on THIS " +
				"Postgres version and plan shape. Still unsafe to rely on: " +
				"unguaranteed by the docs, and a plan change silently flips it to " +
				"fail-closed (1 row) with no error.")
		case 1:
			require.Equal(t, []string{"platform-pool-key"}, out)
			t.Log("RESULT: policy was evaluated BEFORE the CTE bound the tenant, so " +
				"the tenant's own rows were silently dropped. The pipelined shape is " +
				"wrong; its benchmark number must be discarded.")
		default:
			t.Fatalf("unexpected row count %d: %v", len(out), out)
		}
		return nil
	})
	require.NoError(t, err)
}

// TestSeparateSetConfig_IsCorrect pins the shape that IS guaranteed: bind the
// tenant in its own statement, then read. This is the shape whose cost
// BenchmarkTxSetConfigSelect reports, and the one a real implementation should use.
func TestSeparateSetConfig_IsCorrect(t *testing.T) {
	fixture(t, true)

	app := appDB(t)
	err := app.Transaction(func(tx *gorm.DB) error {
		setTenant(t, tx, "tenant-a", true)
		require.ElementsMatch(t,
			[]string{"platform-pool-key", "tenant-a-byok"}, labels(t, tx))
		return nil
	})
	require.NoError(t, err)
}
