package controlplane

import (
	"errors"
	"fmt"
)

var (
	// ErrUnknownStatus reports a status this build does not recognise — an
	// unpopulated struct, or a row written by a newer version. Returned rather
	// than defaulting to any behaviour: for a status we cannot interpret, both
	// "allow" and "silently deny" are wrong answers, and only an error says so.
	ErrUnknownStatus = errors.New("controlplane: unknown tenant status")

	// ErrInvalidTransition reports a move the state machine does not permit.
	ErrInvalidTransition = errors.New("controlplane: invalid tenant status transition")

	// ErrSameStatus reports that the tenant is already in the requested status.
	//
	// Distinct from ErrInvalidTransition because callers need to tell the two
	// apart. Dunning and trial-expiry jobs retry after timeouts, and a retry
	// whose first attempt actually committed lands here; that is success from
	// the job's point of view. An illegal move is a bug. One error for both
	// would force jobs to either treat their own bugs as benign or alert on
	// every successful retry.
	ErrSameStatus = errors.New("controlplane: tenant is already in the requested status")
)

// allowedTransitions is the state machine, as an explicit adjacency map.
//
// Written as data rather than a switch so the machine can be enumerated: the
// tests walk every one of the 25 ordered status pairs against this map, which
// means a new status added to AllStatuses without a considered row here shows up
// as a test failure instead of an undefined move.
//
// Two properties are worth stating out loud, because they are decisions and not
// consequences of the diagram:
//
// StatusDeregistered is terminal — it has no outgoing edges. Reinstating a
// closed account is not a status flip: its tenant_id may have been purged from
// tenant-scoped tables, so flipping the row back to active would produce a
// tenant that exists and serves traffic with its data silently gone. Coming
// back means a new tenant record.
//
// StatusRegistered may go straight to StatusActive, skipping trial. A customer
// who signs a contract before touching the product never has a trial, and
// routing them through one would put a TrialEndsAt on a paying account.
var allowedTransitions = map[Status]map[Status]bool{
	StatusRegistered: {
		StatusTrial:        true,
		StatusActive:       true,
		StatusDeregistered: true,
	},
	StatusTrial: {
		StatusActive:       true, // converted
		StatusSuspended:    true, // trial expired
		StatusDeregistered: true,
	},
	StatusActive: {
		StatusSuspended:    true, // non-payment
		StatusDeregistered: true,
	},
	StatusSuspended: {
		StatusActive:       true, // paid up
		StatusDeregistered: true,
	},
	StatusDeregistered: {},
}

// CanTransition reports whether from → to is permitted, returning a specific
// error when it is not.
//
// Both statuses are validated before the map is consulted. A lookup miss on an
// unknown status would otherwise be indistinguishable from a known status with
// no such edge, and those two want different errors: one is a corrupt or
// forward-dated row, the other is an ordinary rejected move.
func CanTransition(from, to Status) error {
	if !from.Valid() {
		return fmt.Errorf("%w: %q (current)", ErrUnknownStatus, from)
	}
	if !to.Valid() {
		return fmt.Errorf("%w: %q (requested)", ErrUnknownStatus, to)
	}
	if from == to {
		return fmt.Errorf("%w: %q", ErrSameStatus, from)
	}
	if allowedTransitions[from][to] {
		return nil
	}
	return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, from, to)
}

// TerminalStatus reports whether s admits no further transitions, so callers can
// skip work (dunning, expiry scans) without hardcoding which status that is.
func TerminalStatus(s Status) bool {
	return s.Valid() && len(allowedTransitions[s]) == 0
}
