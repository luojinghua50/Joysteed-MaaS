package controlplane

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	tenant "github.com/luojinghua50/Joysteed-MaaS/internal/tenantid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrTenantNotFound is returned instead of gorm.ErrRecordNotFound so callers do
// not have to import gorm to tell "no such tenant" from a database failure. That
// distinction decides whether a request is rejected or retried.
var ErrTenantNotFound = errors.New("controlplane: tenant not found")

// ErrTrialNeedsEndDate reports an attempt to enter StatusTrial through
// Transition, which cannot carry the end date. Use StartTrial.
var ErrTrialNeedsEndDate = errors.New("controlplane: entering trial requires an end date; use StartTrial")

const statusConstraintName = "tenants_status_check"

// safeStatusLiteral matches the shape every Status constant has. Generated DDL
// cannot use bind parameters, so the CHECK constraint below is built by string
// concatenation; this guards that construction rather than trusting that every
// future constant stays free of quotes.
var safeStatusLiteral = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Store is the tenant registry.
type Store struct {
	db *gorm.DB
}

func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

// DB exposes the handle so the outbox writer (D11a) and the audit writer (M5.1)
// can enlist registry writes in their own transaction. Their whole reason for
// living in this database is that they must commit atomically with the change
// they describe, which is impossible if this store hides its handle.
func (s *Store) DB() *gorm.DB { return s.db }

// Migrate creates the tenants table and installs the status CHECK constraint.
//
// AutoMigrate is acceptable here and only here: tenants is this project's own
// table in its own database. Upstream Bifrost tables still have to go through
// framework/migrator, because AutoMigrate would create them without the
// corresponding rows in the migration-metadata table and leave later upstream
// upgrades in an undefined state (see deploy/postgres/01_bootstrap.sql).
func (s *Store) Migrate(ctx context.Context) error {
	if err := s.db.WithContext(ctx).AutoMigrate(&Tenant{}); err != nil {
		return fmt.Errorf("controlplane: migrate tenants: %w", err)
	}
	return s.installStatusConstraint(ctx)
}

// installStatusConstraint constrains status in the database, not just in Go.
//
// The Go state machine only governs writes that go through this package. A
// migration, a psql session or a future service writing this table directly does
// not, and a bad status there is not a rejected write but a tenant whose
// predicates all silently evaluate false — CanServeTraffic denies, and no error
// is raised anywhere. The constraint makes the database the last line.
//
// It is dropped and recreated so the set stays in step with AllStatuses instead
// of freezing whatever the set was when the table was first created.
func (s *Store) installStatusConstraint(ctx context.Context) error {
	literals := make([]string, 0, len(AllStatuses()))
	for _, st := range AllStatuses() {
		if !safeStatusLiteral.MatchString(string(st)) {
			return fmt.Errorf("controlplane: status %q is not a safe SQL literal", st)
		}
		literals = append(literals, "'"+string(st)+"'")
	}

	db := s.db.WithContext(ctx)
	if err := db.Exec(fmt.Sprintf(
		`ALTER TABLE tenants DROP CONSTRAINT IF EXISTS %s`, statusConstraintName)).Error; err != nil {
		return fmt.Errorf("controlplane: drop status constraint: %w", err)
	}
	if err := db.Exec(fmt.Sprintf(
		`ALTER TABLE tenants ADD CONSTRAINT %s CHECK (status IN (%s))`,
		statusConstraintName, strings.Join(literals, ", "))).Error; err != nil {
		return fmt.Errorf("controlplane: add status constraint: %w", err)
	}
	return nil
}

// Create inserts a tenant in StatusRegistered.
//
// The status is not a parameter. Every other status is reachable only through a
// transition, which means the state machine sees every status a tenant has ever
// held — a tenant created directly as active would have bypassed it, and the
// audit trail M5 builds on these transitions would start with a gap.
func (s *Store) Create(ctx context.Context, id tenant.ID, slug, name string) (*Tenant, error) {
	if id == "" {
		return nil, errors.New("controlplane: tenant id is required")
	}
	if slug == "" {
		return nil, errors.New("controlplane: tenant slug is required")
	}
	var out *Tenant
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		out, err = s.CreateTx(tx, id, slug, name)
		return err
	})
	return out, err
}

// CreateTx inserts a tenant using a caller-owned transaction. It is the seam
// used by the HTTP control plane to commit a tenant mutation and its audit row
// atomically in the same control-plane database.
func (s *Store) CreateTx(tx *gorm.DB, id tenant.ID, slug, name string) (*Tenant, error) {
	if tx == nil {
		return nil, errors.New("controlplane: transaction is required")
	}
	if id == "" {
		return nil, errors.New("controlplane: tenant id is required")
	}
	if slug == "" {
		return nil, errors.New("controlplane: tenant slug is required")
	}
	now := time.Now().UTC()
	t := &Tenant{ID: id, Slug: slug, Name: name, Status: StatusRegistered, StatusChangedAt: now}
	if err := tx.Create(t).Error; err != nil {
		return nil, fmt.Errorf("controlplane: create tenant %s: %w", id, err)
	}
	return t, nil
}

// Get returns one tenant by id.
func (s *Store) Get(ctx context.Context, id tenant.ID) (*Tenant, error) {
	var t Tenant
	err := s.db.WithContext(ctx).Where("id = ?", string(id)).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrTenantNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("controlplane: get tenant %s: %w", id, err)
	}
	return &t, nil
}

// List returns tenants for platform administration. Tenant-facing handlers
// must not call this method; they use tenant-scoped portal services instead.
func (s *Store) List(ctx context.Context, limit int) ([]Tenant, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var out []Tenant
	if err := s.db.WithContext(ctx).Order("created_at DESC").Limit(limit).Find(&out).Error; err != nil {
		return nil, fmt.Errorf("controlplane: list tenants: %w", err)
	}
	return out, nil
}

// Transition moves a tenant to a new status, validating the move under a row
// lock.
//
// The lock is the point of the method. Read-then-write without it lets two
// concurrent callers both observe "active" and then apply conflicting moves —
// the dunning job suspends while an admin deregisters — and the second write
// wins having validated against a status that no longer held. SELECT ... FOR
// UPDATE serialises the pair, so each caller validates against the status its
// own write will actually follow.
func (s *Store) Transition(ctx context.Context, id tenant.ID, to Status) (*Tenant, error) {
	if to == StatusTrial {
		// Structural, not a validation nicety: this signature has nowhere to put
		// TrialEndsAt, so allowing it here would be the one way to produce a
		// trial with no end date — which CanServeTraffic refuses, leaving a
		// tenant that looks provisioned and serves nothing.
		return nil, ErrTrialNeedsEndDate
	}

	var out *Tenant
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		t, err := lockTenant(tx, id)
		if err != nil {
			return err
		}
		if err := CanTransition(t.Status, to); err != nil {
			return err
		}
		now := time.Now().UTC()
		updates := map[string]any{
			"status":            string(to),
			"status_changed_at": now,
			// Leaving trial clears the end date. A stale TrialEndsAt on an active
			// tenant is a live tripwire: it reads as "this trial ended", and any
			// later code that consults the field rather than the status would
			// stop serving a paying customer.
			"trial_ends_at": nil,
		}
		if err := tx.Model(&Tenant{}).Where("id = ?", string(id)).Updates(updates).Error; err != nil {
			return fmt.Errorf("controlplane: transition tenant %s to %s: %w", id, to, err)
		}
		t.Status = to
		t.StatusChangedAt = now
		t.TrialEndsAt = nil
		out = t
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// StartTrial moves a tenant into StatusTrial with its end date, in one write.
//
// Separate from Transition rather than a variant of it because the end date is
// mandatory for this target and meaningless for every other one. Passing it as
// an always-nil-except-here parameter would make "trial without an end date"
// expressible at every call site.
func (s *Store) StartTrial(ctx context.Context, id tenant.ID, endsAt time.Time) (*Tenant, error) {
	if endsAt.IsZero() {
		return nil, errors.New("controlplane: trial end date is required")
	}

	var out *Tenant
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		t, err := lockTenant(tx, id)
		if err != nil {
			return err
		}
		if err := CanTransition(t.Status, StatusTrial); err != nil {
			return err
		}
		now := time.Now().UTC()
		endsAtUTC := endsAt.UTC()
		if err := tx.Model(&Tenant{}).Where("id = ?", string(id)).Updates(map[string]any{
			"status":            string(StatusTrial),
			"status_changed_at": now,
			"trial_ends_at":     endsAtUTC,
		}).Error; err != nil {
			return fmt.Errorf("controlplane: start trial for tenant %s: %w", id, err)
		}
		t.Status = StatusTrial
		t.StatusChangedAt = now
		t.TrialEndsAt = &endsAtUTC
		out = t
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// lockTenant reads a tenant FOR UPDATE inside tx.
func lockTenant(tx *gorm.DB, id tenant.ID) (*Tenant, error) {
	var t Tenant
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", string(id)).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrTenantNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("controlplane: lock tenant %s: %w", id, err)
	}
	return &t, nil
}

// ListExpiredTrials returns trials past their end date, oldest first — the work
// queue for the expiry job.
//
// The comparison happens in SQL rather than by filtering in Go so the scan stays
// proportional to the number of expired trials rather than to the tenant count.
func (s *Store) ListExpiredTrials(ctx context.Context, now time.Time) ([]Tenant, error) {
	var out []Tenant
	err := s.db.WithContext(ctx).
		Where("status = ? AND trial_ends_at IS NOT NULL AND trial_ends_at <= ?",
			string(StatusTrial), now.UTC()).
		Order("trial_ends_at ASC").
		Find(&out).Error
	if err != nil {
		return nil, fmt.Errorf("controlplane: list expired trials: %w", err)
	}
	return out, nil
}
