// Package billing contains durable usage, invoice-period, plan and balance
// primitives for M6/M7. Monetary values are integer micros, never float64.
package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tenant "github.com/luojinghua50/Joysteed-MaaS/internal/tenantid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type KeyOwnership string

const (
	KeyOwnershipPlatformPool KeyOwnership = "platform_pool"
	KeyOwnershipBYOK         KeyOwnership = "tenant_byok"
)

type UsageEvent struct {
	ID                 string       `gorm:"column:id;primaryKey;type:text"`
	TenantID           tenant.ID    `gorm:"column:tenant_id;type:text;not null;index"`
	IdempotencyKey     string       `gorm:"column:idempotency_key;type:text;not null;uniqueIndex"`
	PeriodStart        time.Time    `gorm:"column:period_start;not null;index"`
	KeyOwnership       KeyOwnership `gorm:"column:key_ownership;type:text;not null"`
	Model              string       `gorm:"column:model;type:text;not null"`
	PromptTokens       int64        `gorm:"column:prompt_tokens;not null"`
	CompletionTokens   int64        `gorm:"column:completion_tokens;not null"`
	TotalTokens        int64        `gorm:"column:total_tokens;not null"`
	ProviderCostMicros int64        `gorm:"column:provider_cost_micros;not null"`
	MarkupMicros       int64        `gorm:"column:markup_micros;not null"`
	ServiceFeeMicros   int64        `gorm:"column:service_fee_micros;not null"`
	AmountMicros       int64        `gorm:"column:amount_micros;not null"`
	CreatedAt          time.Time    `gorm:"column:created_at;not null;index"`
}

func (UsageEvent) TableName() string { return "billing_usage_events" }

type UsageInput struct {
	ID, IdempotencyKey                                 string
	TenantID                                           tenant.ID
	PeriodStart                                        time.Time
	KeyOwnership                                       KeyOwnership
	Model                                              string
	PromptTokens, CompletionTokens                     int64
	ProviderCostMicros, MarkupMicros, ServiceFeeMicros int64
}

type InvoicePeriod struct {
	ID        string     `gorm:"column:id;primaryKey;type:text"`
	TenantID  tenant.ID  `gorm:"column:tenant_id;type:text;not null;index:idx_billing_period,unique"`
	StartsAt  time.Time  `gorm:"column:starts_at;not null;index:idx_billing_period,unique"`
	EndsAt    time.Time  `gorm:"column:ends_at;not null"`
	Status    string     `gorm:"column:status;type:text;not null;index"`
	CreatedAt time.Time  `gorm:"column:created_at;not null"`
	ClosedAt  *time.Time `gorm:"column:closed_at"`
}

func (InvoicePeriod) TableName() string { return "billing_periods" }

type Invoice struct {
	ID                   string     `gorm:"column:id;primaryKey;type:text"`
	TenantID             tenant.ID  `gorm:"column:tenant_id;type:text;not null;index"`
	PeriodID             string     `gorm:"column:period_id;type:text;not null;uniqueIndex"`
	Currency             string     `gorm:"column:currency;type:text;not null"`
	PlatformCostMicros   int64      `gorm:"column:platform_cost_micros;not null"`
	TenantSubtotalMicros int64      `gorm:"column:tenant_subtotal_micros;not null"`
	Status               string     `gorm:"column:status;type:text;not null"`
	IssuedAt             *time.Time `gorm:"column:issued_at"`
	CreatedAt            time.Time  `gorm:"column:created_at;not null"`
}

func (Invoice) TableName() string { return "billing_invoices" }

type Account struct {
	TenantID       tenant.ID `gorm:"column:tenant_id;primaryKey;type:text"`
	Currency       string    `gorm:"column:currency;type:text;not null"`
	BalanceMicros  int64     `gorm:"column:balance_micros;not null"`
	ReservedMicros int64     `gorm:"column:reserved_micros;not null"`
	UpdatedAt      time.Time `gorm:"column:updated_at;not null"`
}

func (Account) TableName() string { return "billing_accounts" }

type Reservation struct {
	ID             string    `gorm:"column:id;primaryKey;type:text"`
	TenantID       tenant.ID `gorm:"column:tenant_id;type:text;not null;index"`
	IdempotencyKey string    `gorm:"column:idempotency_key;type:text;not null;uniqueIndex"`
	AmountMicros   int64     `gorm:"column:amount_micros;not null"`
	Status         string    `gorm:"column:status;type:text;not null;index"`
	CreatedAt      time.Time `gorm:"column:created_at;not null"`
	UpdatedAt      time.Time `gorm:"column:updated_at;not null"`
}

func (Reservation) TableName() string { return "billing_reservations" }

type Plan struct {
	ID            string    `gorm:"column:id;primaryKey;type:text"`
	Name          string    `gorm:"column:name;type:text;not null"`
	Currency      string    `gorm:"column:currency;type:text;not null"`
	MonthlyMicros int64     `gorm:"column:monthly_micros;not null"`
	OveragePolicy string    `gorm:"column:overage_policy;type:text;not null"`
	CreatedAt     time.Time `gorm:"column:created_at;not null"`
}

func (Plan) TableName() string { return "billing_plans" }

type SKU struct {
	ID                    string    `gorm:"column:id;primaryKey;type:text"`
	Name                  string    `gorm:"column:name;type:text;not null"`
	PromptMicrosPer1K     int64     `gorm:"column:prompt_micros_per_1k;not null"`
	CompletionMicrosPer1K int64     `gorm:"column:completion_micros_per_1k;not null"`
	CreatedAt             time.Time `gorm:"column:created_at;not null"`
}

func (SKU) TableName() string { return "billing_skus" }

type PlanQuota struct {
	PlanID         string `gorm:"column:plan_id;primaryKey;type:text"`
	SKU            string `gorm:"column:sku;primaryKey;type:text"`
	IncludedTokens int64  `gorm:"column:included_tokens;not null"`
	MaxConcurrent  int    `gorm:"column:max_concurrent;not null"`
	Priority       int    `gorm:"column:priority;not null"`
}

func (PlanQuota) TableName() string { return "billing_plan_quotas" }

type TenantPlan struct {
	TenantID tenant.ID  `gorm:"column:tenant_id;primaryKey;type:text"`
	PlanID   string     `gorm:"column:plan_id;type:text;not null"`
	StartsAt time.Time  `gorm:"column:starts_at;not null"`
	EndsAt   *time.Time `gorm:"column:ends_at"`
}

func (TenantPlan) TableName() string { return "billing_tenant_plans" }

var (
	ErrInvalidUsage        = errors.New("billing: invalid usage")
	ErrUsageConflict       = errors.New("billing: idempotency key belongs to another tenant")
	ErrInsufficientBalance = errors.New("billing: insufficient available balance")
	ErrReservationState    = errors.New("billing: invalid reservation state")
)

type Store struct{ db *gorm.DB }

func NewStore(db *gorm.DB) *Store { return &Store{db: db} }
func (s *Store) DB() *gorm.DB {
	if s == nil {
		return nil
	}
	return s.db
}

func (s *Store) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("billing: database is required")
	}
	for _, model := range []any{&UsageEvent{}, &InvoicePeriod{}, &Invoice{}, &Account{}, &Reservation{}, &Plan{}, &SKU{}, &PlanQuota{}, &TenantPlan{}} {
		if err := s.db.WithContext(ctx).AutoMigrate(model); err != nil {
			return fmt.Errorf("billing: migrate: %w", err)
		}
	}
	return nil
}

// RecordUsage is replay-safe: an already seen idempotency key returns the
// original row and never increments accounting a second time.
func (s *Store) RecordUsage(ctx context.Context, in UsageInput) (*UsageEvent, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("billing: database is required")
	}
	if in.TenantID == "" || strings.TrimSpace(in.IdempotencyKey) == "" || in.PeriodStart.IsZero() ||
		in.KeyOwnership != KeyOwnershipPlatformPool && in.KeyOwnership != KeyOwnershipBYOK ||
		in.PromptTokens < 0 || in.CompletionTokens < 0 || in.ProviderCostMicros < 0 || in.MarkupMicros < 0 || in.ServiceFeeMicros < 0 {
		return nil, ErrInvalidUsage
	}
	var out UsageEvent
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing UsageEvent
		if err := tx.Where("idempotency_key = ?", in.IdempotencyKey).First(&existing).Error; err == nil {
			if existing.TenantID != in.TenantID {
				return ErrUsageConflict
			}
			out = existing
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		id := in.ID
		if id == "" {
			id = in.IdempotencyKey
		}
		total := in.PromptTokens + in.CompletionTokens
		var amount int64
		if in.KeyOwnership == KeyOwnershipPlatformPool {
			amount = in.ProviderCostMicros + in.MarkupMicros
		} else {
			// BYOK has no platform token cost or platform markup; only the
			// contracted service fee is tenant-billable.
			amount = in.ServiceFeeMicros
		}
		row := UsageEvent{ID: id, TenantID: in.TenantID, IdempotencyKey: in.IdempotencyKey, PeriodStart: in.PeriodStart.UTC(), KeyOwnership: in.KeyOwnership, Model: in.Model, PromptTokens: in.PromptTokens, CompletionTokens: in.CompletionTokens, TotalTokens: total, ProviderCostMicros: in.ProviderCostMicros, MarkupMicros: in.MarkupMicros, ServiceFeeMicros: in.ServiceFeeMicros, AmountMicros: amount, CreatedAt: time.Now().UTC()}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "idempotency_key"}}, DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
		if err := tx.Where("idempotency_key = ?", in.IdempotencyKey).First(&out).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("billing: record usage: %w", err)
	}
	return &out, nil
}

func (s *Store) EnsurePeriod(ctx context.Context, tenantID tenant.ID, starts, ends time.Time) (*InvoicePeriod, error) {
	if s == nil || s.db == nil || tenantID == "" || starts.IsZero() || ends.IsZero() || !starts.Before(ends) {
		return nil, ErrInvalidUsage
	}
	row := InvoicePeriod{ID: fmt.Sprintf("%s:%d", tenantID, starts.Unix()), TenantID: tenantID, StartsAt: starts.UTC(), EndsAt: ends.UTC(), Status: "open", CreatedAt: time.Now().UTC()}
	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "tenant_id"}, {Name: "starts_at"}}, DoNothing: true}).Create(&row).Error; err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND starts_at = ?", tenantID, starts.UTC()).First(&row).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

// IssueInvoice closes a period and materializes its totals exactly once. The
// ownership snapshot on each usage row determines platform cost; no key lookup
// is performed while billing historical data.
func (s *Store) IssueInvoice(ctx context.Context, periodID string) (*Invoice, error) {
	if s == nil || s.db == nil || strings.TrimSpace(periodID) == "" {
		return nil, ErrInvalidUsage
	}
	var out Invoice
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var period InvoicePeriod
		if err := tx.Where("id = ?", periodID).First(&period).Error; err != nil {
			return err
		}
		if err := tx.Where("period_id = ?", periodID).First(&out).Error; err == nil {
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var totals struct {
			Subtotal     int64
			PlatformCost int64
		}
		if err := tx.Model(&UsageEvent{}).Select("COALESCE(SUM(amount_micros),0) AS subtotal, COALESCE(SUM(CASE WHEN key_ownership = ? THEN provider_cost_micros ELSE 0 END),0) AS platform_cost", KeyOwnershipPlatformPool).Where("tenant_id = ? AND period_start >= ? AND period_start < ?", period.TenantID, period.StartsAt, period.EndsAt).Scan(&totals).Error; err != nil {
			return err
		}
		now := time.Now().UTC()
		out = Invoice{ID: periodID + ":invoice", TenantID: period.TenantID, PeriodID: periodID, Currency: "USD", PlatformCostMicros: totals.PlatformCost, TenantSubtotalMicros: totals.Subtotal, Status: "issued", IssuedAt: &now, CreatedAt: now}
		if err := tx.Create(&out).Error; err != nil {
			return err
		}
		return tx.Model(&period).Updates(map[string]any{"status": "closed", "closed_at": now}).Error
	})
	if err != nil {
		return nil, fmt.Errorf("billing: issue invoice: %w", err)
	}
	return &out, nil
}

func (s *Store) ListUsage(ctx context.Context, tenantID tenant.ID, from, to time.Time, limit int) ([]UsageEvent, error) {
	if s == nil || s.db == nil || tenantID == "" || from.IsZero() || to.IsZero() || !from.Before(to) {
		return nil, ErrInvalidUsage
	}
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	var rows []UsageEvent
	err := s.db.WithContext(ctx).Where("tenant_id = ? AND period_start >= ? AND period_start < ?", tenantID, from.UTC(), to.UTC()).Order("period_start DESC").Limit(limit).Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("billing: list usage: %w", err)
	}
	return rows, nil
}

func (s *Store) SetTenantPlan(ctx context.Context, tenantID tenant.ID, planID string, starts time.Time) error {
	if s == nil || s.db == nil || tenantID == "" || strings.TrimSpace(planID) == "" || starts.IsZero() {
		return ErrInvalidUsage
	}
	var plan Plan
	if err := s.db.WithContext(ctx).Where("id = ?", planID).First(&plan).Error; err != nil {
		return err
	}
	row := TenantPlan{TenantID: tenantID, PlanID: planID, StartsAt: starts.UTC()}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "tenant_id"}}, DoUpdates: clause.AssignmentColumns([]string{"plan_id", "starts_at", "ends_at"})}).Create(&row).Error
}

type QuotaStatus struct {
	PlanID         string
	SKU            string
	IncludedTokens int64
	UsedTokens     int64
	Remaining      int64
	MaxConcurrent  int
	Priority       int
	OveragePolicy  string
	Allowed        bool
}

// CheckQuota reads the tenant's current plan and usage snapshot. The caller
// supplies the accounting window explicitly so monthly, daily, or contract
// periods do not get inferred differently by different services.
func (s *Store) CheckQuota(ctx context.Context, tenantID tenant.ID, sku string, at, from, to time.Time, additionalTokens int64) (QuotaStatus, error) {
	if s == nil || s.db == nil || tenantID == "" || strings.TrimSpace(sku) == "" || at.IsZero() || from.IsZero() || to.IsZero() || !from.Before(to) || additionalTokens < 0 {
		return QuotaStatus{}, ErrInvalidUsage
	}
	var assignment TenantPlan
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND starts_at <= ? AND (ends_at IS NULL OR ends_at > ?)", tenantID, at.UTC(), at.UTC()).Order("starts_at DESC").First(&assignment).Error; err != nil {
		return QuotaStatus{}, err
	}
	var plan Plan
	if err := s.db.WithContext(ctx).Where("id = ?", assignment.PlanID).First(&plan).Error; err != nil {
		return QuotaStatus{}, err
	}
	var quota PlanQuota
	if err := s.db.WithContext(ctx).Where("plan_id = ? AND sku = ?", assignment.PlanID, sku).First(&quota).Error; err != nil {
		return QuotaStatus{}, err
	}
	var used struct{ Tokens int64 }
	if err := s.db.WithContext(ctx).Model(&UsageEvent{}).Select("COALESCE(SUM(total_tokens),0) AS tokens").Where("tenant_id = ? AND model = ? AND period_start >= ? AND period_start < ?", tenantID, sku, from.UTC(), to.UTC()).Scan(&used).Error; err != nil {
		return QuotaStatus{}, err
	}
	remaining := quota.IncludedTokens - used.Tokens
	return QuotaStatus{PlanID: assignment.PlanID, SKU: sku, IncludedTokens: quota.IncludedTokens, UsedTokens: used.Tokens, Remaining: remaining, MaxConcurrent: quota.MaxConcurrent, Priority: quota.Priority, OveragePolicy: plan.OveragePolicy, Allowed: plan.OveragePolicy != "reject" || used.Tokens+additionalTokens <= quota.IncludedTokens}, nil
}

func (s *Store) Reserve(ctx context.Context, tenantID tenant.ID, idempotency string, amount int64) (*Reservation, error) {
	if s == nil || s.db == nil || tenantID == "" || strings.TrimSpace(idempotency) == "" || amount <= 0 {
		return nil, ErrInvalidUsage
	}
	var out Reservation
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("idempotency_key = ?", idempotency).First(&out).Error; err == nil {
			if out.TenantID != tenantID || out.AmountMicros != amount {
				return ErrReservationState
			}
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var account Account
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ?", tenantID).First(&account).Error; err != nil {
			return err
		}
		if account.BalanceMicros-account.ReservedMicros < amount {
			return ErrInsufficientBalance
		}
		now := time.Now().UTC()
		if err := tx.Model(&account).Updates(map[string]any{"reserved_micros": account.ReservedMicros + amount, "updated_at": now}).Error; err != nil {
			return err
		}
		out = Reservation{ID: idempotency, TenantID: tenantID, IdempotencyKey: idempotency, AmountMicros: amount, Status: "reserved", CreatedAt: now, UpdatedAt: now}
		return tx.Create(&out).Error
	})
	if err != nil {
		return nil, fmt.Errorf("billing: reserve: %w", err)
	}
	return &out, nil
}

func (s *Store) Release(ctx context.Context, id string) error {
	return s.transitionReservation(ctx, id, "reserved", "released", false)
}
func (s *Store) Capture(ctx context.Context, id string) error {
	return s.transitionReservation(ctx, id, "reserved", "captured", true)
}

func (s *Store) transitionReservation(ctx context.Context, id, from, to string, capture bool) error {
	if s == nil || s.db == nil || id == "" {
		return ErrInvalidUsage
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var r Reservation
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).First(&r).Error; err != nil {
			return err
		}
		if r.Status != from {
			if r.Status == to {
				return nil
			}
			return ErrReservationState
		}
		var account Account
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ?", r.TenantID).First(&account).Error; err != nil {
			return err
		}
		updates := map[string]any{"reserved_micros": account.ReservedMicros - r.AmountMicros, "updated_at": time.Now().UTC()}
		if capture {
			updates["balance_micros"] = account.BalanceMicros - r.AmountMicros
		}
		if account.ReservedMicros < r.AmountMicros || (capture && account.BalanceMicros < r.AmountMicros) {
			return ErrReservationState
		}
		if err := tx.Model(&account).Updates(updates).Error; err != nil {
			return err
		}
		return tx.Model(&r).Update("status", to).Error
	})
}

func (s *Store) UpsertAccount(ctx context.Context, account Account) error {
	if s == nil || s.db == nil || account.TenantID == "" || account.BalanceMicros < 0 || account.ReservedMicros < 0 || account.ReservedMicros > account.BalanceMicros {
		return ErrInvalidUsage
	}
	if account.Currency == "" {
		account.Currency = "USD"
	}
	account.UpdatedAt = time.Now().UTC()
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "tenant_id"}}, DoUpdates: clause.AssignmentColumns([]string{"currency", "balance_micros", "reserved_micros", "updated_at"})}).Create(&account).Error
}

func (s *Store) CreatePlan(ctx context.Context, p Plan, quotas []PlanQuota) error {
	if s == nil || s.db == nil || p.ID == "" || p.Name == "" || p.MonthlyMicros < 0 {
		return ErrInvalidUsage
	}
	p.CreatedAt = time.Now().UTC()
	if p.Currency == "" {
		p.Currency = "USD"
	}
	if p.OveragePolicy == "" {
		p.OveragePolicy = "reject"
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&p).Error; err != nil {
			return err
		}
		for _, q := range quotas {
			if q.PlanID == "" {
				q.PlanID = p.ID
			}
			if err := tx.Create(&q).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
