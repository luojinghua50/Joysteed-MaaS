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

type UsageStatus string

const (
	KeyOwnershipPlatformPool KeyOwnership = "platform_pool"
	KeyOwnershipBYOK         KeyOwnership = "tenant_byok"

	UsageStatusUnknown UsageStatus = "unknown"
	UsageStatusSuccess UsageStatus = "success"
	UsageStatusFailed  UsageStatus = "failed"
)

type UsageEvent struct {
	ID                 string       `gorm:"column:id;primaryKey;type:text"`
	TenantID           tenant.ID    `gorm:"column:tenant_id;type:text;not null;index"`
	IdempotencyKey     string       `gorm:"column:idempotency_key;type:text;not null;uniqueIndex"`
	GatewayLogID       string       `gorm:"column:gateway_log_id;type:text;index" json:"gateway_log_id,omitempty"`
	Attempt            int          `gorm:"column:attempt;not null;default:0" json:"attempt"`
	PeriodStart        time.Time    `gorm:"column:period_start;not null;index"`
	KeyOwnership       KeyOwnership `gorm:"column:key_ownership;type:text;not null"`
	Model              string       `gorm:"column:model;type:text;not null"`
	Status             UsageStatus  `gorm:"column:status;type:text;not null;default:unknown;index"`
	DurationMS         *int64       `gorm:"column:duration_ms"`
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
	GatewayLogID                                       string
	Attempt                                            int
	TenantID                                           tenant.ID
	PeriodStart                                        time.Time
	KeyOwnership                                       KeyOwnership
	Model                                              string
	Status                                             UsageStatus
	DurationMS                                         *int64
	PromptTokens, CompletionTokens                     int64
	ProviderCostMicros, MarkupMicros, ServiceFeeMicros int64
}

type UsageListFilter struct {
	Model  string
	Status UsageStatus
	Limit  int
	Offset int
}

type UsagePage struct {
	Events     []UsageEvent
	TotalCount int64
	Models     []string
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
	ID                   string    `gorm:"column:id;primaryKey;type:text" json:"id"`
	Name                 string    `gorm:"column:name;type:text;not null" json:"name"`
	Currency             string    `gorm:"column:currency;type:text;not null" json:"currency"`
	MonthlyMicros        int64     `gorm:"column:monthly_micros;not null" json:"monthly_micros"`
	IncludedCreditMicros int64     `gorm:"column:included_credit_micros;not null;default:0" json:"included_credit_micros"`
	MaxConcurrent        int       `gorm:"column:max_concurrent;not null;default:5" json:"max_concurrent"`
	OveragePolicy        string    `gorm:"column:overage_policy;type:text;not null" json:"overage_policy"`
	CreatedAt            time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

func (Plan) TableName() string { return "billing_plans" }

type SKU struct {
	ID                    string    `gorm:"column:id;primaryKey;type:text" json:"id"`
	Name                  string    `gorm:"column:name;type:text;not null" json:"name"`
	PromptMicrosPer1M     int64     `gorm:"column:prompt_micros_per_1m;not null;default:0" json:"prompt_micros_per_1m"`
	CompletionMicrosPer1M int64     `gorm:"column:completion_micros_per_1m;not null;default:0" json:"completion_micros_per_1m"`
	LegacyPromptPer1K     int64     `gorm:"column:prompt_micros_per_1k;not null;default:0" json:"-"`
	LegacyCompletionPer1K int64     `gorm:"column:completion_micros_per_1k;not null;default:0" json:"-"`
	CreatedAt             time.Time `gorm:"column:created_at;not null" json:"created_at"`
}

func (SKU) TableName() string { return "billing_skus" }

type PlanQuota struct {
	PlanID         string `gorm:"column:plan_id;primaryKey;type:text" json:"plan_id"`
	SKU            string `gorm:"column:sku;primaryKey;type:text" json:"sku"`
	IncludedTokens int64  `gorm:"column:included_tokens;not null" json:"included_tokens"`
	MaxConcurrent  int    `gorm:"column:max_concurrent;not null" json:"max_concurrent"`
	Priority       int    `gorm:"column:priority;not null" json:"priority"`
}

func (PlanQuota) TableName() string { return "billing_plan_quotas" }

// PlanModel is the explicit model allow-list for a plan. PlanQuota remains in
// the schema only so existing installations can migrate their model scopes.
type PlanModel struct {
	PlanID  string `gorm:"column:plan_id;primaryKey;type:text" json:"plan_id"`
	ModelID string `gorm:"column:model_id;primaryKey;type:text" json:"model_id"`
}

func (PlanModel) TableName() string { return "billing_plan_models" }

type TenantPlan struct {
	TenantID tenant.ID  `gorm:"column:tenant_id;primaryKey;type:text" json:"tenant_id"`
	PlanID   string     `gorm:"column:plan_id;type:text;not null" json:"plan_id"`
	StartsAt time.Time  `gorm:"column:starts_at;not null" json:"starts_at"`
	EndsAt   *time.Time `gorm:"column:ends_at" json:"ends_at,omitempty"`
}

func (TenantPlan) TableName() string { return "billing_tenant_plans" }

type PlanDetails struct {
	Plan   Plan     `json:"plan"`
	Models []string `json:"models"`
}

type TenantPlanDetails struct {
	Assignment TenantPlan      `json:"assignment"`
	Plan       Plan            `json:"plan"`
	Models     []string        `json:"models"`
	Credit     AllowanceStatus `json:"credit"`
}

// AllowanceStatus is the tenant's shared monetary allowance for one billing
// window. ModelAllowed is kept separate from Allowed so callers can return a
// precise denial reason without inferring it from the remaining balance.
type AllowanceStatus struct {
	PlanID                string `json:"plan_id"`
	Model                 string `json:"model,omitempty"`
	ModelAllowed          bool   `json:"model_allowed"`
	PricingConfigured     bool   `json:"pricing_configured"`
	IncludedCreditMicros  int64  `json:"included_credit_micros"`
	UsedCreditMicros      int64  `json:"used_credit_micros"`
	RemainingCreditMicros int64  `json:"remaining_credit_micros"`
	MaxConcurrent         int    `json:"max_concurrent"`
	OveragePolicy         string `json:"overage_policy"`
	Allowed               bool   `json:"allowed"`
}

// ModelPolicy is the model catalog a tenant may use after intersecting its
// active plan allow-list with the MaaS pricing catalog.
type ModelPolicy struct {
	PlanID       string
	PlanModels   []string
	PricedModels []string
}

func (p ModelPolicy) ModelAllowed(model string) bool {
	return modelIDMatches(p.PlanModels, model)
}

func (p ModelPolicy) PricingConfigured(model string) bool {
	return modelIDMatches(p.PricedModels, model)
}

func (p ModelPolicy) Allows(model string) bool {
	return p.ModelAllowed(model) && p.PricingConfigured(model)
}

type UsageSummary struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	AmountMicros     int64 `json:"amount_micros"`
	Requests         int64 `json:"requests"`
}

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
	migrator := s.db.Migrator()
	hadCreditColumn := migrator.HasColumn(&Plan{}, "IncludedCreditMicros")
	hadConcurrencyColumn := migrator.HasColumn(&Plan{}, "MaxConcurrent")
	hadPromptPer1MColumn := migrator.HasColumn(&SKU{}, "PromptMicrosPer1M")
	hadCompletionPer1MColumn := migrator.HasColumn(&SKU{}, "CompletionMicrosPer1M")
	// Legacy columns remain write-compatible for one release window but are
	// hidden from JSON so the public contract exposes only /1M rates.
	hadPromptPer1KColumn := migrator.HasColumn(&SKU{}, "LegacyPromptPer1K")
	hadCompletionPer1KColumn := migrator.HasColumn(&SKU{}, "LegacyCompletionPer1K")
	for _, model := range []any{&UsageEvent{}, &InvoicePeriod{}, &Invoice{}, &Account{}, &Reservation{}, &Plan{}, &PlanQuota{}, &PlanModel{}, &TenantPlan{}} {
		if err := s.db.WithContext(ctx).AutoMigrate(model); err != nil {
			return fmt.Errorf("billing: migrate: %w", err)
		}
	}
	// Once both current price columns exist the SKU shape is complete. Skipping
	// repeated AutoMigrate calls also avoids SQLite rebuilding compatibility
	// tables and replacing values in ALTER-added columns with their defaults.
	if !hadPromptPer1MColumn || !hadCompletionPer1MColumn || !hadPromptPer1KColumn || !hadCompletionPer1KColumn {
		if err := s.db.WithContext(ctx).AutoMigrate(&SKU{}); err != nil {
			return fmt.Errorf("billing: migrate SKU: %w", err)
		}
	}
	if err := s.migrateLegacyPlanConfig(ctx, hadCreditColumn, hadConcurrencyColumn); err != nil {
		return fmt.Errorf("billing: migrate legacy plan configuration: %w", err)
	}
	if err := s.migrateLegacySKUPrices(ctx, hadPromptPer1MColumn, hadCompletionPer1MColumn, hadPromptPer1KColumn, hadCompletionPer1KColumn); err != nil {
		return fmt.Errorf("billing: migrate legacy SKU prices: %w", err)
	}
	return nil
}

func (s *Store) migrateLegacySKUPrices(ctx context.Context, hadPromptPer1M, hadCompletionPer1M, hadPromptPer1K, hadCompletionPer1K bool) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if !hadPromptPer1M && hadPromptPer1K {
			if err := tx.Model(&SKU{}).Where("1 = 1").UpdateColumn("prompt_micros_per_1m", gorm.Expr("prompt_micros_per_1k * ?", 1000)).Error; err != nil {
				return err
			}
		}
		if !hadCompletionPer1M && hadCompletionPer1K {
			if err := tx.Model(&SKU{}).Where("1 = 1").UpdateColumn("completion_micros_per_1m", gorm.Expr("completion_micros_per_1k * ?", 1000)).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) migrateLegacyPlanConfig(ctx context.Context, hadCreditColumn, hadConcurrencyColumn bool) error {
	var quotas []PlanQuota
	if err := s.db.WithContext(ctx).Find(&quotas).Error; err != nil {
		return err
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, quota := range quotas {
			model := PlanModel{PlanID: quota.PlanID, ModelID: quota.SKU}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model).Error; err != nil {
				return err
			}
		}
		if !hadCreditColumn {
			if err := tx.Model(&Plan{}).Where("included_credit_micros = 0 AND monthly_micros > 0").Update("included_credit_micros", gorm.Expr("monthly_micros")).Error; err != nil {
				return err
			}
		}
		if !hadConcurrencyColumn {
			var plans []Plan
			if err := tx.Find(&plans).Error; err != nil {
				return err
			}
			for _, plan := range plans {
				var maxConcurrent int
				if err := tx.Model(&PlanQuota{}).Where("plan_id = ?", plan.ID).Select("COALESCE(MAX(max_concurrent), 0)").Scan(&maxConcurrent).Error; err != nil {
					return err
				}
				if maxConcurrent > 0 {
					if err := tx.Model(&Plan{}).Where("id = ?", plan.ID).Update("max_concurrent", maxConcurrent).Error; err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
}

// RecordUsage is replay-safe: an already seen idempotency key returns the
// original row and never increments accounting a second time.
func (s *Store) RecordUsage(ctx context.Context, in UsageInput) (*UsageEvent, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("billing: database is required")
	}
	if in.TenantID == "" || strings.TrimSpace(in.IdempotencyKey) == "" || in.PeriodStart.IsZero() ||
		in.KeyOwnership != KeyOwnershipPlatformPool && in.KeyOwnership != KeyOwnershipBYOK ||
		in.PromptTokens < 0 || in.CompletionTokens < 0 || in.ProviderCostMicros < 0 || in.MarkupMicros < 0 || in.ServiceFeeMicros < 0 ||
		in.DurationMS != nil && *in.DurationMS < 0 || !validUsageStatus(in.Status, true) {
		return nil, ErrInvalidUsage
	}
	if in.Status == "" {
		in.Status = UsageStatusUnknown
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
		row := UsageEvent{ID: id, TenantID: in.TenantID, IdempotencyKey: in.IdempotencyKey, GatewayLogID: strings.TrimSpace(in.GatewayLogID), Attempt: in.Attempt, PeriodStart: in.PeriodStart.UTC(), KeyOwnership: in.KeyOwnership, Model: in.Model, Status: in.Status, DurationMS: in.DurationMS, PromptTokens: in.PromptTokens, CompletionTokens: in.CompletionTokens, TotalTokens: total, ProviderCostMicros: in.ProviderCostMicros, MarkupMicros: in.MarkupMicros, ServiceFeeMicros: in.ServiceFeeMicros, AmountMicros: amount, CreatedAt: time.Now().UTC()}
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

func validUsageStatus(status UsageStatus, allowEmpty bool) bool {
	return allowEmpty && status == "" || status == UsageStatusUnknown || status == UsageStatusSuccess || status == UsageStatusFailed
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

func (s *Store) ListUsagePage(ctx context.Context, tenantID tenant.ID, from, to time.Time, filter UsageListFilter) (UsagePage, error) {
	if s == nil || s.db == nil || tenantID == "" || from.IsZero() || to.IsZero() || !from.Before(to) ||
		filter.Offset < 0 || !validUsageStatus(filter.Status, true) {
		return UsagePage{}, ErrInvalidUsage
	}
	if filter.Limit <= 0 || filter.Limit > 100 {
		filter.Limit = 20
	}
	base := s.db.WithContext(ctx).Model(&UsageEvent{}).
		Where("tenant_id = ? AND period_start >= ? AND period_start < ?", tenantID, from.UTC(), to.UTC())
	model := strings.TrimSpace(filter.Model)
	query := base
	if model != "" {
		query = query.Where("model = ?", model)
	}
	if filter.Status != "" {
		query = query.Where("status = ?", filter.Status)
	}
	var page UsagePage
	if err := query.Count(&page.TotalCount).Error; err != nil {
		return UsagePage{}, fmt.Errorf("billing: count usage: %w", err)
	}
	if err := query.Order("period_start DESC").Limit(filter.Limit).Offset(filter.Offset).Find(&page.Events).Error; err != nil {
		return UsagePage{}, fmt.Errorf("billing: list usage page: %w", err)
	}
	if page.Events == nil {
		page.Events = []UsageEvent{}
	}
	modelsQuery := s.db.WithContext(ctx).Model(&UsageEvent{}).
		Where("tenant_id = ? AND period_start >= ? AND period_start < ?", tenantID, from.UTC(), to.UTC())
	if err := modelsQuery.Distinct("model").Where("model <> ''").Order("model ASC").Pluck("model", &page.Models).Error; err != nil {
		return UsagePage{}, fmt.Errorf("billing: list usage models: %w", err)
	}
	if page.Models == nil {
		page.Models = []string{}
	}
	return page, nil
}

func (s *Store) GetUsage(ctx context.Context, tenantID tenant.ID, id string) (*UsageEvent, error) {
	if s == nil || s.db == nil || tenantID == "" || strings.TrimSpace(id) == "" {
		return nil, ErrInvalidUsage
	}
	var row UsageEvent
	err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, strings.TrimSpace(id)).Take(&row).Error
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// EffectiveGatewayLogID keeps pre-migration usage rows navigable without
// making the billing event ID format the long-term correlation contract.
func (u UsageEvent) EffectiveGatewayLogID() string {
	if value := strings.TrimSpace(u.GatewayLogID); value != "" {
		return value
	}
	if index := strings.LastIndex(u.ID, ":attempt:"); index > 0 {
		return u.ID[:index]
	}
	return strings.TrimSpace(u.ID)
}

func (s *Store) SetTenantPlan(ctx context.Context, tenantID tenant.ID, planID string, starts time.Time) error {
	if s == nil || s.db == nil {
		return ErrInvalidUsage
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return s.SetTenantPlanTx(tx, tenantID, planID, starts)
	})
}

// SetTenantPlanTx updates a tenant assignment in a caller-owned transaction.
func (s *Store) SetTenantPlanTx(tx *gorm.DB, tenantID tenant.ID, planID string, starts time.Time) error {
	if tx == nil || tenantID == "" || strings.TrimSpace(planID) == "" || starts.IsZero() {
		return ErrInvalidUsage
	}
	var plan Plan
	if err := tx.Where("id = ?", planID).First(&plan).Error; err != nil {
		return err
	}
	row := TenantPlan{TenantID: tenantID, PlanID: planID, StartsAt: starts.UTC()}
	return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "tenant_id"}}, DoUpdates: clause.AssignmentColumns([]string{"plan_id", "starts_at", "ends_at"})}).Create(&row).Error
}

func (s *Store) ListPlans(ctx context.Context) ([]PlanDetails, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("billing: database is required")
	}
	var plans []Plan
	if err := s.db.WithContext(ctx).Order("monthly_micros ASC, id ASC").Find(&plans).Error; err != nil {
		return nil, err
	}
	out := make([]PlanDetails, 0, len(plans))
	for _, plan := range plans {
		models, err := s.listPlanModels(ctx, plan.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, PlanDetails{Plan: plan, Models: models})
	}
	return out, nil
}

func (s *Store) CurrentTenantPlan(ctx context.Context, tenantID tenant.ID, at, from, to time.Time) (*TenantPlanDetails, error) {
	if s == nil || s.db == nil || tenantID == "" || at.IsZero() || from.IsZero() || to.IsZero() || !from.Before(to) {
		return nil, ErrInvalidUsage
	}
	var assignment TenantPlan
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND starts_at <= ? AND (ends_at IS NULL OR ends_at > ?)", tenantID, at.UTC(), at.UTC()).Order("starts_at DESC").First(&assignment).Error; err != nil {
		return nil, err
	}
	var plan Plan
	if err := s.db.WithContext(ctx).Where("id = ?", assignment.PlanID).Take(&plan).Error; err != nil {
		return nil, err
	}
	models, err := s.listPlanModels(ctx, plan.ID)
	if err != nil {
		return nil, err
	}
	credit, err := s.allowanceStatus(ctx, tenantID, assignment.PlanID, plan, "", from, to, 0)
	if err != nil {
		return nil, err
	}
	return &TenantPlanDetails{Assignment: assignment, Plan: plan, Models: models, Credit: credit}, nil
}

// CurrentModelPolicy loads the active plan model allow-list and the configured
// pricing catalog. Callers intersect this policy with Bifrost's live and
// virtual-key-filtered model response.
func (s *Store) CurrentModelPolicy(ctx context.Context, tenantID tenant.ID, at time.Time) (ModelPolicy, error) {
	if s == nil || s.db == nil || tenantID == "" || at.IsZero() {
		return ModelPolicy{}, ErrInvalidUsage
	}
	var assignment TenantPlan
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND starts_at <= ? AND (ends_at IS NULL OR ends_at > ?)", tenantID, at.UTC(), at.UTC()).Order("starts_at DESC").First(&assignment).Error; err != nil {
		return ModelPolicy{}, err
	}
	models, err := s.listPlanModels(ctx, assignment.PlanID)
	if err != nil {
		return ModelPolicy{}, err
	}
	var skus []SKU
	if err := s.db.WithContext(ctx).Select("id").Order("id ASC").Find(&skus).Error; err != nil {
		return ModelPolicy{}, err
	}
	priced := make([]string, 0, len(skus))
	for _, sku := range skus {
		priced = append(priced, sku.ID)
	}
	return ModelPolicy{PlanID: assignment.PlanID, PlanModels: models, PricedModels: priced}, nil
}

func (s *Store) listPlanModels(ctx context.Context, planID string) ([]string, error) {
	var rows []PlanModel
	if err := s.db.WithContext(ctx).Where("plan_id = ?", planID).Order("model_id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	models := make([]string, 0, len(rows))
	for _, row := range rows {
		models = append(models, row.ModelID)
	}
	return models, nil
}

func (s *Store) SummarizeUsage(ctx context.Context, tenantID tenant.ID, from, to time.Time) (UsageSummary, error) {
	if s == nil || s.db == nil || tenantID == "" || from.IsZero() || to.IsZero() || !from.Before(to) {
		return UsageSummary{}, ErrInvalidUsage
	}
	var out UsageSummary
	err := s.db.WithContext(ctx).Model(&UsageEvent{}).
		Select("COALESCE(SUM(prompt_tokens),0) AS prompt_tokens, COALESCE(SUM(completion_tokens),0) AS completion_tokens, COALESCE(SUM(total_tokens),0) AS total_tokens, COALESCE(SUM(amount_micros),0) AS amount_micros, COUNT(*) AS requests").
		Where("tenant_id = ? AND period_start >= ? AND period_start < ?", tenantID, from.UTC(), to.UTC()).Scan(&out).Error
	return out, err
}

func (s *Store) UpsertSKU(ctx context.Context, sku SKU) error {
	if s == nil || s.db == nil {
		return ErrInvalidUsage
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return s.UpsertSKUTx(tx, sku)
	})
}

// UpsertSKUTx writes model pricing in a caller-owned transaction.
func (s *Store) UpsertSKUTx(tx *gorm.DB, sku SKU) error {
	if tx == nil || strings.TrimSpace(sku.ID) == "" || strings.TrimSpace(sku.Name) == "" || sku.PromptMicrosPer1M < 0 || sku.CompletionMicrosPer1M < 0 {
		return ErrInvalidUsage
	}
	sku.ID = strings.TrimSpace(sku.ID)
	sku.Name = strings.TrimSpace(sku.Name)
	sku.LegacyPromptPer1K = sku.PromptMicrosPer1M / 1000
	sku.LegacyCompletionPer1K = sku.CompletionMicrosPer1M / 1000
	if sku.CreatedAt.IsZero() {
		sku.CreatedAt = time.Now().UTC()
	}
	return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoUpdates: clause.AssignmentColumns([]string{"name", "prompt_micros_per_1m", "completion_micros_per_1m", "prompt_micros_per_1k", "completion_micros_per_1k"})}).Create(&sku).Error
}

func (s *Store) ListSKUs(ctx context.Context) ([]SKU, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("billing: database is required")
	}
	var rows []SKU
	if err := s.db.WithContext(ctx).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *Store) PriceUsage(ctx context.Context, model string, promptTokens, completionTokens int64) (int64, error) {
	if s == nil || s.db == nil || strings.TrimSpace(model) == "" || promptTokens < 0 || completionTokens < 0 {
		return 0, ErrInvalidUsage
	}
	var sku SKU
	candidates := skuCandidates(model)
	for index, candidate := range candidates {
		err := s.db.WithContext(ctx).Where("id = ?", candidate).Take(&sku).Error
		if err == nil {
			break
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, err
		}
		if index == len(candidates)-1 {
			return 0, err
		}
	}
	// Round each side up to a micro so a configured non-zero price cannot
	// silently become free for requests smaller than one million tokens.
	price := func(tokens, rate int64) int64 {
		if tokens == 0 || rate == 0 {
			return 0
		}
		return (tokens*rate + 999_999) / 1_000_000
	}
	return price(promptTokens, sku.PromptMicrosPer1M) + price(completionTokens, sku.CompletionMicrosPer1M), nil
}

// skuCandidates keeps pre-canonicalization catalogs working while new entries
// use provider/model. Exact provider-qualified rows always win, followed by a
// legacy bare-model row and finally the all-model fallback.
func skuCandidates(model string) []string {
	model = strings.TrimSpace(model)
	candidates := []string{model}
	if slash := strings.IndexByte(model, '/'); slash > 0 && slash+1 < len(model) {
		candidates = append(candidates, model[slash+1:])
	}
	if model != "*" {
		candidates = append(candidates, "*")
	}
	return candidates
}

func modelIDMatches(configured []string, model string) bool {
	if len(configured) == 0 {
		return false
	}
	candidates := skuCandidates(model)
	for _, configuredModel := range configured {
		for _, candidate := range candidates {
			if configuredModel == candidate {
				return true
			}
		}
	}
	return false
}

// CheckAllowance reads a tenant's shared credit balance and model access for
// an explicit accounting window. Charges are settled after the provider
// response, so a concurrent request can exceed a reject-policy limit by its
// final charge; this method intentionally does not claim reservation semantics.
func (s *Store) CheckAllowance(ctx context.Context, tenantID tenant.ID, model string, at, from, to time.Time, additionalMicros int64) (AllowanceStatus, error) {
	if s == nil || s.db == nil || tenantID == "" || strings.TrimSpace(model) == "" || at.IsZero() || from.IsZero() || to.IsZero() || !from.Before(to) || additionalMicros < 0 {
		return AllowanceStatus{}, ErrInvalidUsage
	}
	var assignment TenantPlan
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND starts_at <= ? AND (ends_at IS NULL OR ends_at > ?)", tenantID, at.UTC(), at.UTC()).Order("starts_at DESC").First(&assignment).Error; err != nil {
		return AllowanceStatus{}, err
	}
	var plan Plan
	if err := s.db.WithContext(ctx).Where("id = ?", assignment.PlanID).First(&plan).Error; err != nil {
		return AllowanceStatus{}, err
	}
	return s.allowanceStatus(ctx, tenantID, assignment.PlanID, plan, strings.TrimSpace(model), from, to, additionalMicros)
}

func (s *Store) allowanceStatus(ctx context.Context, tenantID tenant.ID, planID string, plan Plan, model string, from, to time.Time, additionalMicros int64) (AllowanceStatus, error) {
	modelAllowed := model == ""
	pricingConfigured := model == ""
	if model != "" {
		var matches int64
		if err := s.db.WithContext(ctx).Model(&PlanModel{}).Where("plan_id = ? AND model_id IN ?", planID, skuCandidates(model)).Count(&matches).Error; err != nil {
			return AllowanceStatus{}, err
		}
		modelAllowed = matches > 0
		matches = 0
		if err := s.db.WithContext(ctx).Model(&SKU{}).Where("id IN ?", skuCandidates(model)).Count(&matches).Error; err != nil {
			return AllowanceStatus{}, err
		}
		pricingConfigured = matches > 0
	}
	var used struct{ Amount int64 }
	if err := s.db.WithContext(ctx).Model(&UsageEvent{}).
		Select("COALESCE(SUM(amount_micros),0) AS amount").
		Where("tenant_id = ? AND period_start >= ? AND period_start < ?", tenantID, from.UTC(), to.UTC()).
		Scan(&used).Error; err != nil {
		return AllowanceStatus{}, err
	}
	remaining := plan.IncludedCreditMicros - used.Amount
	if remaining < 0 {
		remaining = 0
	}
	creditAllowed := plan.OveragePolicy != "reject"
	if plan.OveragePolicy == "reject" {
		if additionalMicros > 0 {
			creditAllowed = used.Amount+additionalMicros <= plan.IncludedCreditMicros
		} else {
			creditAllowed = used.Amount < plan.IncludedCreditMicros
		}
	}
	return AllowanceStatus{
		PlanID: planID, Model: model, ModelAllowed: modelAllowed, PricingConfigured: pricingConfigured,
		IncludedCreditMicros: plan.IncludedCreditMicros, UsedCreditMicros: used.Amount,
		RemainingCreditMicros: remaining, MaxConcurrent: plan.MaxConcurrent,
		OveragePolicy: plan.OveragePolicy, Allowed: modelAllowed && pricingConfigured && creditAllowed,
	}, nil
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

func (s *Store) CreatePlan(ctx context.Context, p Plan, models []string) error {
	if s == nil || s.db == nil {
		return ErrInvalidUsage
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return s.CreatePlanTx(tx, p, models)
	})
}

// CreatePlanTx creates a plan and its model allow-list in a caller-owned transaction.
func (s *Store) CreatePlanTx(tx *gorm.DB, p Plan, models []string) error {
	if tx == nil || !validPlan(p, models) {
		return ErrInvalidUsage
	}
	p.ID = strings.TrimSpace(p.ID)
	p.Name = strings.TrimSpace(p.Name)
	p.CreatedAt = time.Now().UTC()
	if p.Currency == "" {
		p.Currency = "USD"
	}
	if p.OveragePolicy == "" {
		p.OveragePolicy = "reject"
	}
	if err := tx.Create(&p).Error; err != nil {
		return err
	}
	for _, model := range normalizeModels(models) {
		if err := tx.Create(&PlanModel{PlanID: p.ID, ModelID: model}).Error; err != nil {
			return err
		}
	}
	return nil
}

// UpdatePlanTx updates the mutable commercial terms and replaces the model
// allow-list while keeping the plan ID and creation timestamp stable.
func (s *Store) UpdatePlanTx(tx *gorm.DB, p Plan, models []string) error {
	if tx == nil || !validPlan(p, models) {
		return ErrInvalidUsage
	}
	p.ID = strings.TrimSpace(p.ID)
	p.Name = strings.TrimSpace(p.Name)
	if p.Currency == "" {
		p.Currency = "USD"
	}
	if p.OveragePolicy == "" {
		p.OveragePolicy = "reject"
	}
	result := tx.Model(&Plan{}).Where("id = ?", p.ID).Updates(map[string]any{
		"name":                   p.Name,
		"currency":               p.Currency,
		"monthly_micros":         p.MonthlyMicros,
		"included_credit_micros": p.IncludedCreditMicros,
		"max_concurrent":         p.MaxConcurrent,
		"overage_policy":         p.OveragePolicy,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	if err := tx.Where("plan_id = ?", p.ID).Delete(&PlanModel{}).Error; err != nil {
		return err
	}
	for _, model := range normalizeModels(models) {
		if err := tx.Create(&PlanModel{PlanID: p.ID, ModelID: model}).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) EnsurePlan(ctx context.Context, p Plan, models []string) error {
	if s == nil || s.db == nil || !validPlan(p, models) {
		return ErrInvalidUsage
	}
	if p.Currency == "" {
		p.Currency = "USD"
	}
	if p.OveragePolicy == "" {
		p.OveragePolicy = "reject"
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoUpdates: clause.AssignmentColumns([]string{"name", "currency", "monthly_micros", "included_credit_micros", "max_concurrent", "overage_policy"})}).Create(&p).Error; err != nil {
			return err
		}
		normalized := normalizeModels(models)
		if err := tx.Where("plan_id = ? AND model_id NOT IN ?", p.ID, normalized).Delete(&PlanModel{}).Error; err != nil {
			return err
		}
		for _, model := range normalized {
			row := PlanModel{PlanID: p.ID, ModelID: model}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func validPlan(p Plan, models []string) bool {
	if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.Name) == "" || p.MonthlyMicros < 0 || p.IncludedCreditMicros < 0 || p.MaxConcurrent <= 0 || len(models) == 0 || p.OveragePolicy != "" && p.OveragePolicy != "reject" && p.OveragePolicy != "allow" {
		return false
	}
	seen := make(map[string]struct{}, len(models))
	for _, raw := range models {
		model := strings.TrimSpace(raw)
		if !validModelID(model) {
			return false
		}
		if _, exists := seen[model]; exists {
			return false
		}
		seen[model] = struct{}{}
	}
	return true
}

func validModelID(model string) bool {
	if model == "*" {
		return true
	}
	slash := strings.IndexByte(model, '/')
	return slash > 0 && slash < len(model)-1 && !strings.ContainsAny(model, " \t\r\n")
}

func normalizeModels(models []string) []string {
	out := make([]string, 0, len(models))
	for _, model := range models {
		out = append(out, strings.TrimSpace(model))
	}
	return out
}
