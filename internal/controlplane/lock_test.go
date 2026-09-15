package controlplane

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// This file is in package controlplane, not controlplane_test, so it can drive
// lockTenant directly and interleave two transactions by hand.
//
// That is the point. The concurrent-goroutines test in store_test.go does not
// establish the lock's necessity — it passes with the FOR UPDATE removed,
// because eight goroutines racing through a fast transaction end up effectively
// serialised and each loser reads an already-committed status. Proving a lock
// matters needs the interleaving to be forced, not hoped for.

func lockTestEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// No schema isolation here, and that is a consequence of this file's location.
//
// store_test.go migrates and DROPs the same `tenants` table this file does. While
// the two lived in different directories they were different packages, and
// `go test ./...` runs different packages in PARALLEL — so they raced on that table
// and failed intermittently with "relation tenants does not exist". Sharing a
// directory puts them in ONE test binary, where tests run sequentially unless they
// call t.Parallel() (neither does), so the interleaving is no longer reachable.
func lockTestDSN() string {
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		lockTestEnv("MAAS_SPIKE_PG_HOST", "localhost"),
		lockTestEnv("MAAS_SPIKE_PG_PORT", "55432"),
		lockTestEnv("MAAS_SPIKE_PG_USER", "spike"),
		lockTestEnv("MAAS_SPIKE_PG_PASSWORD", "spike_password"),
		lockTestEnv("MAAS_SPIKE_PG_DB", "spike"))
}

func openLockTestDB(t *testing.T, maxConns int) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(postgres.Open(lockTestDSN()), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err, "start the spike Postgres first (see store_test.go header)")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(maxConns)
	sqlDB.SetMaxIdleConns(maxConns)
	t.Cleanup(func() { _ = sqlDB.Close() })
	require.NoError(t, db.Exec(`DROP TABLE IF EXISTS tenants`).Error)
	return db
}

// TestLockTenant_SerialisesReadValidateWrite is the test the row lock exists for.
//
// It pins two properties that together make Transition's read-validate-write
// safe, and it fails if FOR UPDATE is removed:
//
//  1. A second locking read BLOCKS while the first transaction holds the row.
//  2. When it unblocks it observes the COMMITTED status, so it validates against
//     the status its own write will actually follow.
//
// The counterfactual is asserted in the middle: a non-locking read taken at the
// same moment returns the STALE status. That is precisely what an unlocked
// Transition would validate against — it would approve active → suspended a
// second time, and under M5 that is a duplicate audit record plus, once dunning
// hangs off these transitions, a duplicate side effect.
func TestLockTenant_SerialisesReadValidateWrite(t *testing.T) {
	db := openLockTestDB(t, 5)
	ctx := context.Background()
	s := NewStore(db)
	require.NoError(t, s.Migrate(ctx))

	_, err := s.Create(ctx, "t-lock", "lock", "Lock Co")
	require.NoError(t, err)
	_, err = s.Transition(ctx, "t-lock", StatusActive)
	require.NoError(t, err)

	// tx1 locks the row, moves the tenant, and deliberately does not commit yet.
	tx1 := db.Begin()
	require.NoError(t, tx1.Error)
	committed := false
	defer func() {
		if !committed {
			tx1.Rollback()
		}
	}()

	held, err := lockTenant(tx1, "t-lock")
	require.NoError(t, err)
	require.Equal(t, StatusActive, held.Status)

	require.NoError(t, tx1.Model(&Tenant{}).Where("id = ?", "t-lock").
		Updates(map[string]any{
			"status":            string(StatusSuspended),
			"status_changed_at": time.Now().UTC(),
			"trial_ends_at":     nil,
		}).Error)

	// The counterfactual: MVCC lets an unlocked read through immediately, and
	// what it gets back is the pre-update status.
	var stale string
	require.NoError(t, db.Raw(
		`SELECT status FROM tenants WHERE id = ?`, "t-lock").Scan(&stale).Error)
	require.Equal(t, string(StatusActive), stale,
		"an unlocked read sees the stale status — this is what Transition would validate against without the lock")

	type outcome struct {
		status Status
		err    error
	}
	second := make(chan outcome, 1)
	go func() {
		tx2 := db.Begin()
		if tx2.Error != nil {
			second <- outcome{err: tx2.Error}
			return
		}
		defer tx2.Rollback()
		// require.* is not safe off the test goroutine, so the result travels
		// back over the channel and is asserted on the main goroutine.
		locked, err := lockTenant(tx2, "t-lock")
		if err != nil {
			second <- outcome{err: err}
			return
		}
		second <- outcome{status: locked.Status}
	}()

	select {
	case got := <-second:
		t.Fatalf("the second locking read must block while tx1 holds the row; it returned status=%q err=%v",
			got.status, got.err)
	case <-time.After(300 * time.Millisecond):
		// Blocked, as required. Postgres guarantees this rather than the timing
		// doing so: without FOR UPDATE the read returns in microseconds.
	}

	require.NoError(t, tx1.Commit().Error)
	committed = true

	select {
	case got := <-second:
		require.NoError(t, got.err)
		require.Equal(t, StatusSuspended, got.status,
			"the locking read must observe the committed status, not the one it would have read before blocking")
		require.ErrorIs(t, CanTransition(got.status, StatusSuspended), ErrSameStatus,
			"validating against the fresh status is what makes the duplicate move refusable")
	case <-time.After(5 * time.Second):
		t.Fatal("the second locking read never unblocked after tx1 committed")
	}
}
