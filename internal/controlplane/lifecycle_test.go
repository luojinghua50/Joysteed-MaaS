package controlplane_test

import (
	"testing"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/controlplane"
	"github.com/stretchr/testify/require"
)

// expectedTransitions restates the state machine independently of the
// implementation.
//
// Deriving it from the package's own allowedTransitions would make the matrix
// test below tautological — it would assert that the map equals itself and pass
// for any machine whatsoever. Written out by hand, a change on either side has
// to be justified against the other.
var expectedTransitions = map[controlplane.Status][]controlplane.Status{
	controlplane.StatusRegistered: {
		controlplane.StatusTrial, controlplane.StatusActive, controlplane.StatusDeregistered,
	},
	controlplane.StatusTrial: {
		controlplane.StatusActive, controlplane.StatusSuspended, controlplane.StatusDeregistered,
	},
	controlplane.StatusActive: {
		controlplane.StatusSuspended, controlplane.StatusDeregistered,
	},
	controlplane.StatusSuspended: {
		controlplane.StatusActive, controlplane.StatusDeregistered,
	},
	controlplane.StatusDeregistered: {},
}

// TestCanTransition_ExhaustiveMatrix walks all 25 ordered status pairs.
//
// Exhaustive rather than a few interesting cases on purpose: a status added to
// AllStatuses without a considered row in the transition table shows up here as
// a failure, instead of becoming a state whose every outgoing move is silently
// rejected.
func TestCanTransition_ExhaustiveMatrix(t *testing.T) {
	all := controlplane.AllStatuses()
	require.Len(t, all, 5, "matrix size assumption")
	require.Len(t, expectedTransitions, len(all),
		"every status needs a row in the expected table")

	for _, from := range all {
		allowed := map[controlplane.Status]bool{}
		for _, to := range expectedTransitions[from] {
			allowed[to] = true
		}
		for _, to := range all {
			t.Run(string(from)+"→"+string(to), func(t *testing.T) {
				err := controlplane.CanTransition(from, to)
				switch {
				case from == to:
					require.ErrorIs(t, err, controlplane.ErrSameStatus)
				case allowed[to]:
					require.NoError(t, err)
				default:
					require.ErrorIs(t, err, controlplane.ErrInvalidTransition)
				}
			})
		}
	}
}

// TestCanTransition_SameStatusIsNotInvalidTransition pins the distinction the
// two errors exist for. A retrying dunning job whose first attempt committed
// must be able to tell "already suspended" (success) from an illegal move (bug).
func TestCanTransition_SameStatusIsNotInvalidTransition(t *testing.T) {
	err := controlplane.CanTransition(controlplane.StatusActive, controlplane.StatusActive)
	require.ErrorIs(t, err, controlplane.ErrSameStatus)
	require.NotErrorIs(t, err, controlplane.ErrInvalidTransition)

	err = controlplane.CanTransition(controlplane.StatusDeregistered, controlplane.StatusActive)
	require.ErrorIs(t, err, controlplane.ErrInvalidTransition)
	require.NotErrorIs(t, err, controlplane.ErrSameStatus)
}

// TestCanTransition_UnknownStatusIsRejectedNotDefaulted covers the zero value,
// which is what a failed scan or a half-built struct produces. It must not be
// treated as any known status, in either position.
func TestCanTransition_UnknownStatusIsRejectedNotDefaulted(t *testing.T) {
	for _, bad := range []controlplane.Status{"", "bogus", "ACTIVE", "Active"} {
		t.Run("from/"+string(bad), func(t *testing.T) {
			err := controlplane.CanTransition(bad, controlplane.StatusActive)
			require.ErrorIs(t, err, controlplane.ErrUnknownStatus)
		})
		t.Run("to/"+string(bad), func(t *testing.T) {
			err := controlplane.CanTransition(controlplane.StatusActive, bad)
			require.ErrorIs(t, err, controlplane.ErrUnknownStatus)
		})
	}
}

// TestCanTransition_UnknownToUnknownReportsTheCurrentStatusFirst: with both
// sides invalid the current status is the more useful diagnosis, since it points
// at a corrupt row rather than a bad request.
func TestCanTransition_UnknownToUnknownReportsTheCurrentStatusFirst(t *testing.T) {
	err := controlplane.CanTransition("junk-a", "junk-b")
	require.ErrorIs(t, err, controlplane.ErrUnknownStatus)
	require.Contains(t, err.Error(), "junk-a")
	require.Contains(t, err.Error(), "current")
}

func TestTerminalStatus(t *testing.T) {
	require.True(t, controlplane.TerminalStatus(controlplane.StatusDeregistered))
	for _, s := range []controlplane.Status{
		controlplane.StatusRegistered,
		controlplane.StatusTrial,
		controlplane.StatusActive,
		controlplane.StatusSuspended,
	} {
		require.False(t, controlplane.TerminalStatus(s), s)
	}
	// An unknown status is not terminal: it has no edges, but reporting it as
	// terminal would let expiry and dunning scans skip a corrupt row quietly.
	require.False(t, controlplane.TerminalStatus(""))
	require.False(t, controlplane.TerminalStatus("bogus"))
}

func TestStatus_Valid(t *testing.T) {
	for _, s := range controlplane.AllStatuses() {
		require.True(t, s.Valid(), s)
	}
	for _, s := range []controlplane.Status{"", " ", "active ", "Active", "bogus"} {
		require.False(t, s.Valid(), s)
	}
}

// TestCanServeTraffic covers the gate the data plane consults. The trial rows are
// the reason it takes a clock: an expired trial must be refused on the strength
// of the timestamp alone, without waiting for the expiry job to move its status.
func TestCanServeTraffic(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	future := now.Add(24 * time.Hour)
	past := now.Add(-24 * time.Hour)

	cases := []struct {
		name   string
		tenant *controlplane.Tenant
		want   bool
	}{
		{"nil tenant", nil, false},
		{"zero value", &controlplane.Tenant{}, false},
		{"unknown status", &controlplane.Tenant{Status: "bogus"}, false},
		{"registered", &controlplane.Tenant{Status: controlplane.StatusRegistered}, false},
		{"active", &controlplane.Tenant{Status: controlplane.StatusActive}, true},
		{"suspended", &controlplane.Tenant{Status: controlplane.StatusSuspended}, false},
		{"deregistered", &controlplane.Tenant{Status: controlplane.StatusDeregistered}, false},
		{"trial in window", &controlplane.Tenant{
			Status: controlplane.StatusTrial, TrialEndsAt: &future}, true},
		{"trial expired", &controlplane.Tenant{
			Status: controlplane.StatusTrial, TrialEndsAt: &past}, false},
		{"trial ending exactly now", &controlplane.Tenant{
			Status: controlplane.StatusTrial, TrialEndsAt: &now}, false},
		// A trial with no end date is a provisioning bug. Refused rather than
		// read as an unlimited trial, which is the more generous and more
		// expensive interpretation.
		{"trial without end date", &controlplane.Tenant{
			Status: controlplane.StatusTrial}, false},
		// An end date left behind on a non-trial status must not gate a paying
		// customer; the status decides, the date only refines a trial.
		{"active with stale trial end date", &controlplane.Tenant{
			Status: controlplane.StatusActive, TrialEndsAt: &past}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, c.tenant.CanServeTraffic(now))
		})
	}
}

func TestTrialExpired(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)

	require.False(t, (*controlplane.Tenant)(nil).TrialExpired(now))
	require.True(t, (&controlplane.Tenant{
		Status: controlplane.StatusTrial, TrialEndsAt: &past}).TrialExpired(now))
	require.True(t, (&controlplane.Tenant{
		Status: controlplane.StatusTrial, TrialEndsAt: &now}).TrialExpired(now))
	require.False(t, (&controlplane.Tenant{
		Status: controlplane.StatusTrial, TrialEndsAt: &future}).TrialExpired(now))
	require.False(t, (&controlplane.Tenant{
		Status: controlplane.StatusTrial}).TrialExpired(now))
	// Only trials expire. A suspended tenant carrying an old date is not owed a
	// transition by the expiry job.
	require.False(t, (&controlplane.Tenant{
		Status: controlplane.StatusSuspended, TrialEndsAt: &past}).TrialExpired(now))
}
