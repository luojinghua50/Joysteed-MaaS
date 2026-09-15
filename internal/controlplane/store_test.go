package controlplane_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/controlplane"
	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// These tests run against Postgres, not SQLite. Two of them cannot be expressed
// anywhere else: the row-lock test needs SELECT ... FOR UPDATE, and the CHECK
// constraint test needs a database that enforces one.
//
//	docker run -d --name maas-rls-spike \
//	  -e POSTGRES_PASSWORD=spike_password -e POSTGRES_USER=spike \
//	  -e POSTGRES_DB=spike -p 55432:5432 postgres:16-alpine

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func dsn() string {
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		env("MAAS_SPIKE_PG_HOST", "localhost"),
		env("MAAS_SPIKE_PG_PORT", "55432"),
		env("MAAS_SPIKE_PG_USER", "spike"),
		env("MAAS_SPIKE_PG_PASSWORD", "spike_password"),
		env("MAAS_SPIKE_PG_DB", "spike"))
}

// newStore returns a migrated store over an empty tenants table.
func newStore(t *testing.T, maxConns int) (*controlplane.Store, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(postgres.Open(dsn()), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err, "start the spike Postgres first (see the file header)")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(maxConns)
	sqlDB.SetMaxIdleConns(maxConns)
	t.Cleanup(func() { _ = sqlDB.Close() })

	require.NoError(t, db.Exec(`DROP TABLE IF EXISTS tenants`).Error)
	s := controlplane.NewStore(db)
	require.NoError(t, s.Migrate(context.Background()))
	return s, db
}

// TestMigrate_IsIdempotent matters because installStatusConstraint drops and
// recreates the constraint. If that were not idempotent, the second boot of a
// deployed node would fail, and the failure would be at startup on an upgrade.
func TestMigrate_IsIdempotent(t *testing.T) {
	s, _ := newStore(t, 2)
	ctx := context.Background()
	require.NoError(t, s.Migrate(ctx))
	require.NoError(t, s.Migrate(ctx))

	created, err := s.Create(ctx, "t-idem", "idem", "Idempotent Co")
	require.NoError(t, err)
	require.Equal(t, controlplane.StatusRegistered, created.Status)
}

// TestCreate_StartsRegistered pins that status is not a Create parameter: every
// status other than the initial one is reachable only through the state machine,
// so the machine sees every status a tenant ever holds.
func TestCreate_StartsRegistered(t *testing.T) {
	s, _ := newStore(t, 2)
	ctx := context.Background()

	got, err := s.Create(ctx, "t-a", "acme", "Acme Corp")
	require.NoError(t, err)
	require.Equal(t, controlplane.StatusRegistered, got.Status)
	require.Nil(t, got.TrialEndsAt)
	require.False(t, got.StatusChangedAt.IsZero())
	require.False(t, got.CanServeTraffic(time.Now()),
		"a registered tenant is not provisioned yet")

	reloaded, err := s.Get(ctx, "t-a")
	require.NoError(t, err)
	require.Equal(t, controlplane.StatusRegistered, reloaded.Status)
	require.Equal(t, "acme", reloaded.Slug)
}

func TestCreate_RejectsEmptyIDAndSlug(t *testing.T) {
	s, _ := newStore(t, 2)
	ctx := context.Background()
	_, err := s.Create(ctx, "", "slug", "name")
	require.Error(t, err)
	_, err = s.Create(ctx, "t-x", "", "name")
	require.Error(t, err)
}

func TestCreate_SlugIsUnique(t *testing.T) {
	s, _ := newStore(t, 2)
	ctx := context.Background()
	_, err := s.Create(ctx, "t-1", "same-slug", "First")
	require.NoError(t, err)
	_, err = s.Create(ctx, "t-2", "same-slug", "Second")
	require.Error(t, err, "slug is the human-facing handle; two tenants cannot share one")
}

// TestGet_UnknownTenantIsTypedNotGormError: callers decide between rejecting a
// request and retrying it, so "no such tenant" must be distinguishable from a
// database failure without importing gorm.
func TestGet_UnknownTenantIsTypedNotGormError(t *testing.T) {
	s, _ := newStore(t, 2)
	_, err := s.Get(context.Background(), "t-nope")
	require.ErrorIs(t, err, controlplane.ErrTenantNotFound)
}

func TestTransition_HappyPath(t *testing.T) {
	s, _ := newStore(t, 2)
	ctx := context.Background()
	_, err := s.Create(ctx, "t-b", "beta", "Beta Inc")
	require.NoError(t, err)

	active, err := s.Transition(ctx, "t-b", controlplane.StatusActive)
	require.NoError(t, err)
	require.Equal(t, controlplane.StatusActive, active.Status)
	require.True(t, active.CanServeTraffic(time.Now()))

	suspended, err := s.Transition(ctx, "t-b", controlplane.StatusSuspended)
	require.NoError(t, err)
	require.False(t, suspended.CanServeTraffic(time.Now()))

	back, err := s.Transition(ctx, "t-b", controlplane.StatusActive)
	require.NoError(t, err, "a suspended tenant that pays up comes back")
	require.True(t, back.CanServeTraffic(time.Now()))

	gone, err := s.Transition(ctx, "t-b", controlplane.StatusDeregistered)
	require.NoError(t, err)
	require.True(t, controlplane.TerminalStatus(gone.Status))
}

// TestTransition_DeregisteredIsTerminalInTheDatabaseToo: the state machine says
// deregistered has no outgoing edges; this proves the store enforces it rather
// than only the pure function doing so.
func TestTransition_DeregisteredIsTerminal(t *testing.T) {
	s, _ := newStore(t, 2)
	ctx := context.Background()
	_, err := s.Create(ctx, "t-c", "gamma", "Gamma")
	require.NoError(t, err)
	_, err = s.Transition(ctx, "t-c", controlplane.StatusDeregistered)
	require.NoError(t, err)

	for _, to := range []controlplane.Status{
		controlplane.StatusRegistered,
		controlplane.StatusActive,
		controlplane.StatusSuspended,
	} {
		_, err := s.Transition(ctx, "t-c", to)
		require.ErrorIs(t, err, controlplane.ErrInvalidTransition, to)
	}

	still, err := s.Get(ctx, "t-c")
	require.NoError(t, err)
	require.Equal(t, controlplane.StatusDeregistered, still.Status)
}

func TestTransition_SameStatusIsDistinguishable(t *testing.T) {
	s, _ := newStore(t, 2)
	ctx := context.Background()
	_, err := s.Create(ctx, "t-d", "delta", "Delta")
	require.NoError(t, err)
	_, err = s.Transition(ctx, "t-d", controlplane.StatusActive)
	require.NoError(t, err)

	_, err = s.Transition(ctx, "t-d", controlplane.StatusActive)
	require.ErrorIs(t, err, controlplane.ErrSameStatus)
	require.NotErrorIs(t, err, controlplane.ErrInvalidTransition)
}

// TestTransition_RefusesTrialTarget: Transition has nowhere to put the end date,
// so allowing StatusTrial here would be the one path that produces a trial with
// no deadline — which CanServeTraffic refuses, leaving a tenant that looks
// provisioned and serves nothing.
func TestTransition_RefusesTrialTarget(t *testing.T) {
	s, _ := newStore(t, 2)
	ctx := context.Background()
	_, err := s.Create(ctx, "t-e", "epsilon", "Epsilon")
	require.NoError(t, err)

	_, err = s.Transition(ctx, "t-e", controlplane.StatusTrial)
	require.ErrorIs(t, err, controlplane.ErrTrialNeedsEndDate)

	unchanged, err := s.Get(ctx, "t-e")
	require.NoError(t, err)
	require.Equal(t, controlplane.StatusRegistered, unchanged.Status)
}

func TestTransition_UnknownTenant(t *testing.T) {
	s, _ := newStore(t, 2)
	_, err := s.Transition(context.Background(), "t-ghost", controlplane.StatusActive)
	require.ErrorIs(t, err, controlplane.ErrTenantNotFound)
}

func TestStartTrial_SetsEndDateAndServesTraffic(t *testing.T) {
	s, _ := newStore(t, 2)
	ctx := context.Background()
	_, err := s.Create(ctx, "t-f", "zeta", "Zeta")
	require.NoError(t, err)

	endsAt := time.Now().Add(14 * 24 * time.Hour).UTC()
	trial, err := s.StartTrial(ctx, "t-f", endsAt)
	require.NoError(t, err)
	require.Equal(t, controlplane.StatusTrial, trial.Status)
	require.NotNil(t, trial.TrialEndsAt)
	require.WithinDuration(t, endsAt, *trial.TrialEndsAt, time.Second)
	require.True(t, trial.CanServeTraffic(time.Now()))

	reloaded, err := s.Get(ctx, "t-f")
	require.NoError(t, err)
	require.NotNil(t, reloaded.TrialEndsAt, "the end date must survive the round trip")
	require.True(t, reloaded.CanServeTraffic(time.Now()))
}

func TestStartTrial_RequiresEndDate(t *testing.T) {
	s, _ := newStore(t, 2)
	ctx := context.Background()
	_, err := s.Create(ctx, "t-g", "eta", "Eta")
	require.NoError(t, err)

	_, err = s.StartTrial(ctx, "t-g", time.Time{})
	require.Error(t, err)

	unchanged, err := s.Get(ctx, "t-g")
	require.NoError(t, err)
	require.Equal(t, controlplane.StatusRegistered, unchanged.Status)
}

// TestStartTrial_ExpiredTrialStopsServingWithoutAnyStatusChange is the reason
// CanServeTraffic reads a clock. The expiry job has not run — the stored status
// is still "trial" — and the tenant must already be refused. Keying off status
// alone would serve expired trials for free for as long as that job stayed
// wedged, with nothing raising an error.
func TestStartTrial_ExpiredTrialStopsServingWithoutStatusChange(t *testing.T) {
	s, _ := newStore(t, 2)
	ctx := context.Background()
	_, err := s.Create(ctx, "t-h", "theta", "Theta")
	require.NoError(t, err)

	past := time.Now().Add(-time.Hour).UTC()
	_, err = s.StartTrial(ctx, "t-h", past)
	require.NoError(t, err)

	got, err := s.Get(ctx, "t-h")
	require.NoError(t, err)
	require.Equal(t, controlplane.StatusTrial, got.Status, "status is untouched")
	require.False(t, got.CanServeTraffic(time.Now()))
	require.True(t, got.TrialExpired(time.Now()))
}

// TestTransition_LeavingTrialClearsTheEndDate: a stale TrialEndsAt on an active
// tenant is a live tripwire — it reads as "this trial ended", so any later code
// consulting the field would stop serving a paying customer.
func TestTransition_LeavingTrialClearsTheEndDate(t *testing.T) {
	s, _ := newStore(t, 2)
	ctx := context.Background()
	_, err := s.Create(ctx, "t-i", "iota", "Iota")
	require.NoError(t, err)
	_, err = s.StartTrial(ctx, "t-i", time.Now().Add(time.Hour))
	require.NoError(t, err)

	converted, err := s.Transition(ctx, "t-i", controlplane.StatusActive)
	require.NoError(t, err)
	require.Nil(t, converted.TrialEndsAt)

	reloaded, err := s.Get(ctx, "t-i")
	require.NoError(t, err)
	require.Nil(t, reloaded.TrialEndsAt, "the end date must be cleared in the row, not just in memory")
	require.True(t, reloaded.CanServeTraffic(time.Now()))
}

// TestTransition_ConcurrentSameTargetHasExactlyOneWinner checks that concurrent
// duplicate transitions converge: one caller applies the move, every other gets
// ErrSameStatus, nobody gets an unexpected error, and the row lands on the
// intended status.
//
// It does NOT establish that the row lock is necessary — it was measured to pass
// with the FOR UPDATE removed, because eight goroutines through a short
// transaction end up effectively serialised and each loser reads an
// already-committed status. That property is proved deterministically in
// lock_test.go instead. Kept here for what it does cover: the API's behaviour
// under real concurrency, including that losing is a typed error rather than a
// crash or a silent overwrite.
func TestTransition_ConcurrentSameTargetHasExactlyOneWinner(t *testing.T) {
	const goroutines = 8
	s, _ := newStore(t, goroutines+2)
	ctx := context.Background()
	_, err := s.Create(ctx, "t-race", "race", "Race Co")
	require.NoError(t, err)
	_, err = s.Transition(ctx, "t-race", controlplane.StatusActive)
	require.NoError(t, err)

	var (
		mu        sync.Mutex
		succeeded int
		sameCount int
		other     []error
		start     sync.WaitGroup
		done      sync.WaitGroup
	)
	start.Add(1)
	for i := 0; i < goroutines; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			_, err := s.Transition(ctx, "t-race", controlplane.StatusSuspended)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, controlplane.ErrSameStatus):
				sameCount++
			default:
				other = append(other, err)
			}
		}()
	}
	start.Done()
	done.Wait()

	require.Empty(t, other, "no transition should fail for an unexpected reason")
	require.Equal(t, 1, succeeded, "exactly one caller may apply the transition")
	require.Equal(t, goroutines-1, sameCount,
		"every loser must see the committed status, not a stale one")

	final, err := s.Get(ctx, "t-race")
	require.NoError(t, err)
	require.Equal(t, controlplane.StatusSuspended, final.Status)
}

// TestStatusCheckConstraint_RejectsUnknownStatusFromRawSQL goes around the Go
// state machine on purpose. The machine only governs writes through this
// package; a migration or a psql session does not. A bad status written that way
// is not a rejected write but a tenant whose every predicate silently evaluates
// false — CanServeTraffic denies and nothing reports an error. This proves the
// database refuses it.
func TestStatusCheckConstraint_RejectsUnknownStatusFromRawSQL(t *testing.T) {
	s, db := newStore(t, 2)
	ctx := context.Background()
	_, err := s.Create(ctx, "t-j", "kappa", "Kappa")
	require.NoError(t, err)

	err = db.Exec(`UPDATE tenants SET status = 'bogus' WHERE id = ?`, "t-j").Error
	require.Error(t, err, "the CHECK constraint must reject a status Go does not know")

	err = db.Exec(
		`INSERT INTO tenants (id, slug, name, status, status_changed_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, now(), now(), now())`,
		"t-k", "lambda", "Lambda", "ACTIVE").Error
	require.Error(t, err, "status is case-sensitive; 'ACTIVE' is not a status")

	// Every status the Go side accepts must be accepted by the constraint too,
	// or the two have drifted in the opposite direction.
	for _, st := range controlplane.AllStatuses() {
		err := db.Exec(`UPDATE tenants SET status = ? WHERE id = ?`, string(st), "t-j").Error
		require.NoError(t, err, st)
	}
}

// TestListExpiredTrials returns the expiry job's work queue.
func TestListExpiredTrials(t *testing.T) {
	s, _ := newStore(t, 2)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, tc := range []struct {
		id     tenant.ID
		slug   string
		endsAt time.Time
	}{
		{"t-exp-old", "exp-old", now.Add(-48 * time.Hour)},
		{"t-exp-new", "exp-new", now.Add(-1 * time.Hour)},
		{"t-live", "live", now.Add(24 * time.Hour)},
	} {
		_, err := s.Create(ctx, tc.id, tc.slug, string(tc.id))
		require.NoError(t, err)
		_, err = s.StartTrial(ctx, tc.id, tc.endsAt)
		require.NoError(t, err)
	}
	// An active tenant is never in the queue, even though it once was a trial.
	_, err := s.Create(ctx, "t-paying", "paying", "Paying")
	require.NoError(t, err)
	_, err = s.StartTrial(ctx, "t-paying", now.Add(-72*time.Hour))
	require.NoError(t, err)
	_, err = s.Transition(ctx, "t-paying", controlplane.StatusActive)
	require.NoError(t, err)

	got, err := s.ListExpiredTrials(ctx, now)
	require.NoError(t, err)
	ids := make([]tenant.ID, 0, len(got))
	for _, g := range got {
		ids = append(ids, g.ID)
	}
	require.Equal(t, []tenant.ID{"t-exp-old", "t-exp-new"}, ids,
		"expired trials only, oldest first")
}
