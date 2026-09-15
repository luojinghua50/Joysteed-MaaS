// Package controlplane holds the MaaS control plane's own data model, starting
// with the tenant registry.
//
// It is a separate database from the data plane's configstore, per D11a → ④.
// Two things depend on that separation: the audit record and the config change
// it describes have to commit in one transaction (M5.1), and the transactional
// outbox that tells data-plane nodes "tenant X moved to v42" has to be written
// in that same transaction (D11a / §9.3). Neither is possible if the registry
// lives in the store the data plane is concurrently reloading.
package controlplane

import (
	"fmt"
	"time"

	tenant "github.com/luojinghua50/Joysteed-MaaS/internal/tenantid"
)

// Status is a tenant's lifecycle state, one of the five in MAAS_TECH_DESIGN.md
// §4 M1 (注册 / 试用 / 正式 / 欠费停服 / 注销).
//
// Stored as text rather than an integer. An integer status is unreadable when
// someone is querying this table during an incident, and renumbering the
// constants silently reinterprets every existing row instead of failing.
type Status string

const (
	// StatusRegistered — the account exists but has not been provisioned. No
	// traffic, because no plan has been chosen yet.
	StatusRegistered Status = "registered"
	// StatusTrial — provisioned on trial terms. Serves traffic until TrialEndsAt.
	StatusTrial Status = "trial"
	// StatusActive — paying customer in good standing.
	StatusActive Status = "active"
	// StatusSuspended — cut off for non-payment. Config and data are retained;
	// only traffic stops.
	StatusSuspended Status = "suspended"
	// StatusDeregistered — terminal. See the note on allowedTransitions for why
	// there is no way back.
	StatusDeregistered Status = "deregistered"
)

// AllStatuses is the single source of truth for the valid set. The database
// CHECK constraint is generated from it (see Migrate) rather than repeated as a
// literal, so the two cannot drift.
func AllStatuses() []Status {
	return []Status{
		StatusRegistered,
		StatusTrial,
		StatusActive,
		StatusSuspended,
		StatusDeregistered,
	}
}

// Valid reports whether s is a status this code knows how to interpret.
//
// Every predicate below is defined only for valid statuses and refuses
// otherwise. The zero value "" is not a status, so a Tenant that was never
// populated — a failed scan, a struct built by a test helper — cannot come out
// of this package looking like it is allowed to do anything.
func (s Status) Valid() bool {
	switch s {
	case StatusRegistered, StatusTrial, StatusActive, StatusSuspended, StatusDeregistered:
		return true
	}
	return false
}

func (s Status) String() string { return string(s) }

// servesTraffic reports whether the status alone permits serving requests.
// Deliberately unexported: status alone is not sufficient for a trial, whose
// expiry also matters. Callers go through Tenant.CanServeTraffic.
func (s Status) servesTraffic() bool {
	return s == StatusTrial || s == StatusActive
}

// Tenant is the registry record. Its ID is what lands in every tenant_id column
// added by the M1 migration and what RunInTenantTx binds into app.tenant_id, so
// it is opaque and immutable. Slug is the human-facing handle and may change;
// nothing references it.
type Tenant struct {
	ID   tenant.ID `gorm:"column:id;type:text;primaryKey"`
	Slug string    `gorm:"column:slug;type:text;not null;uniqueIndex"`
	Name string    `gorm:"column:name;type:text;not null"`

	Status Status `gorm:"column:status;type:text;not null;index"`

	// TrialEndsAt is required while StatusTrial and ignored otherwise. Nil in
	// any other status.
	TrialEndsAt *time.Time `gorm:"column:trial_ends_at"`

	// StatusChangedAt records when the current status was entered. The full
	// history belongs in the audit log (M5), not in denormalised per-status
	// timestamp columns here; this field exists because expiry and dunning jobs
	// need "how long has it been in this state" without joining the audit table.
	StatusChangedAt time.Time `gorm:"column:status_changed_at;not null"`

	CreatedAt time.Time `gorm:"column:created_at;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;not null"`
}

func (Tenant) TableName() string { return "tenants" }

// CanServeTraffic is the gate the data plane consults after resolving a tenant.
//
// It takes now because of the failure mode that motivates the whole function: a
// trial that has passed TrialEndsAt must stop serving even though its stored
// status is still "trial". Expiry is enforced by a scheduled job, and if that
// job is wedged, keying off the stored status alone would serve expired trials
// free of charge indefinitely, with nothing reporting an error. Reading the
// clock at the decision point makes the check independent of the job's health.
func (t *Tenant) CanServeTraffic(now time.Time) bool {
	if t == nil || !t.Status.Valid() || !t.Status.servesTraffic() {
		return false
	}
	if t.Status == StatusTrial {
		// A trial with no end date is a provisioning bug, not an unlimited
		// trial. Refuse rather than grant the more generous reading.
		if t.TrialEndsAt == nil {
			return false
		}
		return now.Before(*t.TrialEndsAt)
	}
	return true
}

// TrialExpired reports whether t is a trial past its end date, i.e. one the
// expiry job still owes a transition to StatusSuspended.
func (t *Tenant) TrialExpired(now time.Time) bool {
	if t == nil || t.Status != StatusTrial || t.TrialEndsAt == nil {
		return false
	}
	return !now.Before(*t.TrialEndsAt)
}

func (t *Tenant) String() string {
	if t == nil {
		return "<nil tenant>"
	}
	return fmt.Sprintf("tenant %s (%s, %s)", t.ID, t.Slug, t.Status)
}
