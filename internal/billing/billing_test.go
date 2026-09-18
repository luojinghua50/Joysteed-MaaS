package billing_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/billing"
	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func billingStore(t *testing.T) *billing.Store {
	db, err := gorm.Open(sqlite.Open("file:billing_"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	s := billing.NewStore(db)
	require.NoError(t, s.Migrate(context.Background()))
	return s
}

func TestUsageIsIdempotentAndSnapshotsOwnership(t *testing.T) {
	s := billingStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	in := billing.UsageInput{TenantID: "t", IdempotencyKey: "req-1", PeriodStart: now, KeyOwnership: billing.KeyOwnershipPlatformPool, PromptTokens: 2, CompletionTokens: 3, ProviderCostMicros: 10, MarkupMicros: 4}
	a, err := s.RecordUsage(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, int64(14), a.AmountMicros)
	require.Equal(t, int64(5), a.TotalTokens)
	in.ProviderCostMicros = 999
	b, err := s.RecordUsage(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, a.ID, b.ID)
	require.Equal(t, int64(14), b.AmountMicros)
	byok := in
	byok.IdempotencyKey = "req-2"
	byok.KeyOwnership = billing.KeyOwnershipBYOK
	byok.ServiceFeeMicros = 7
	byok.ProviderCostMicros = 1000
	c, err := s.RecordUsage(context.Background(), byok)
	require.NoError(t, err)
	require.Equal(t, int64(7), c.AmountMicros)
	period, err := s.EnsurePeriod(context.Background(), "t", now.Add(-time.Hour), now.Add(time.Hour))
	require.NoError(t, err)
	invoice, err := s.IssueInvoice(context.Background(), period.ID)
	require.NoError(t, err)
	require.Equal(t, int64(21), invoice.TenantSubtotalMicros)
	require.Equal(t, int64(10), invoice.PlatformCostMicros)
	replayed, err := s.IssueInvoice(context.Background(), period.ID)
	require.NoError(t, err)
	require.Equal(t, invoice.ID, replayed.ID)
}

func TestUsagePersistsGatewayLogCorrelationAndSupportsLegacyRows(t *testing.T) {
	s := billingStore(t)
	now := time.Now().UTC()
	row, err := s.RecordUsage(context.Background(), billing.UsageInput{TenantID: "t", IdempotencyKey: "billing-1", GatewayLogID: "gateway-1", Attempt: 2, PeriodStart: now, KeyOwnership: billing.KeyOwnershipPlatformPool})
	require.NoError(t, err)
	require.Equal(t, "gateway-1", row.GatewayLogID)
	require.Equal(t, 2, row.Attempt)
	loaded, err := s.GetUsage(context.Background(), "t", row.ID)
	require.NoError(t, err)
	require.Equal(t, "gateway-1", loaded.EffectiveGatewayLogID())

	legacy := billing.UsageEvent{ID: "gateway-legacy:attempt:3"}
	require.Equal(t, "gateway-legacy", legacy.EffectiveGatewayLogID())
	_, err = s.GetUsage(context.Background(), "other-tenant", row.ID)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestUsagePageFiltersAndPaginatesLedgerFields(t *testing.T) {
	s := billingStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	duration := int64(125)
	inputs := []billing.UsageInput{
		{TenantID: "t", IdempotencyKey: "usage-1", PeriodStart: now.Add(-time.Minute), KeyOwnership: billing.KeyOwnershipPlatformPool, Model: "openai/model-a", Status: billing.UsageStatusSuccess, DurationMS: &duration},
		{TenantID: "t", IdempotencyKey: "usage-2", PeriodStart: now.Add(-2 * time.Minute), KeyOwnership: billing.KeyOwnershipPlatformPool, Model: "openai/model-b", Status: billing.UsageStatusFailed},
		{TenantID: "t", IdempotencyKey: "usage-3", PeriodStart: now.Add(-3 * time.Minute), KeyOwnership: billing.KeyOwnershipPlatformPool, Model: "openai/model-a"},
	}
	for _, input := range inputs {
		_, err := s.RecordUsage(context.Background(), input)
		require.NoError(t, err)
	}

	page, err := s.ListUsagePage(context.Background(), "t", now.Add(-time.Hour), now.Add(time.Hour), billing.UsageListFilter{Limit: 2, Offset: 1})
	require.NoError(t, err)
	require.Equal(t, int64(3), page.TotalCount)
	require.Len(t, page.Events, 2)
	require.Equal(t, "usage-2", page.Events[0].ID)
	require.Equal(t, []string{"openai/model-a", "openai/model-b"}, page.Models)

	filtered, err := s.ListUsagePage(context.Background(), "t", now.Add(-time.Hour), now.Add(time.Hour), billing.UsageListFilter{Model: "openai/model-a", Status: billing.UsageStatusSuccess, Limit: 20})
	require.NoError(t, err)
	require.Equal(t, int64(1), filtered.TotalCount)
	require.Equal(t, billing.UsageStatusSuccess, filtered.Events[0].Status)
	require.Equal(t, duration, *filtered.Events[0].DurationMS)

	_, err = s.ListUsagePage(context.Background(), "t", now.Add(-time.Hour), now.Add(time.Hour), billing.UsageListFilter{Status: "not-a-status"})
	require.ErrorIs(t, err, billing.ErrInvalidUsage)
}

func TestPrepaidReservationLifecycle(t *testing.T) {
	s := billingStore(t)
	require.NoError(t, s.UpsertAccount(context.Background(), billing.Account{TenantID: tenant.ID("t"), Currency: "USD", BalanceMicros: 100}))
	r, err := s.Reserve(context.Background(), "t", "claim", 60)
	require.NoError(t, err)
	require.Equal(t, "reserved", r.Status)
	_, err = s.Reserve(context.Background(), "t", "claim", 60)
	require.NoError(t, err)
	_, err = s.Reserve(context.Background(), "t", "other", 50)
	require.ErrorIs(t, err, billing.ErrInsufficientBalance)
	require.NoError(t, s.Capture(context.Background(), r.ID))
	require.NoError(t, s.Capture(context.Background(), r.ID))
}

func TestPlanAllowanceUsesSharedDurableCost(t *testing.T) {
	s := billingStore(t)
	require.NoError(t, s.CreatePlan(context.Background(), billing.Plan{ID: "starter", Name: "Starter", IncludedCreditMicros: 10, MaxConcurrent: 2, OveragePolicy: "reject"}, []string{"openai/model-a"}))
	require.NoError(t, s.SetTenantPlan(context.Background(), "t", "starter", time.Now().Add(-time.Hour)))
	now := time.Now().UTC()
	_, err := s.RecordUsage(context.Background(), billing.UsageInput{TenantID: "t", IdempotencyKey: "allowance-1", PeriodStart: now, KeyOwnership: billing.KeyOwnershipPlatformPool, Model: "openai/model-a", PromptTokens: 6, ProviderCostMicros: 6})
	require.NoError(t, err)
	status, err := s.CheckAllowance(context.Background(), "t", "openai/model-a", now, now.Add(-time.Hour), now.Add(time.Hour), 5)
	require.NoError(t, err)
	require.True(t, status.ModelAllowed)
	require.Equal(t, int64(6), status.UsedCreditMicros)
	require.Equal(t, int64(4), status.RemainingCreditMicros)
	require.False(t, status.Allowed)
}

func TestPlanAllowanceMatchesQualifiedLegacyAndWildcardModels(t *testing.T) {
	s := billingStore(t)
	now := time.Now().UTC()
	require.NoError(t, s.CreatePlan(context.Background(), billing.Plan{ID: "qualified", Name: "Qualified", IncludedCreditMicros: 300, MaxConcurrent: 4, OveragePolicy: "reject"}, []string{"openai/model-a"}))
	require.NoError(t, s.DB().Create(&billing.PlanModel{PlanID: "qualified", ModelID: "legacy-model"}).Error)
	require.NoError(t, s.SetTenantPlan(context.Background(), "t", "qualified", now.Add(-time.Hour)))

	qualified, err := s.CheckAllowance(context.Background(), "t", "openai/model-a", now, now.Add(-time.Hour), now.Add(time.Hour), 0)
	require.NoError(t, err)
	require.True(t, qualified.ModelAllowed)
	require.Equal(t, 4, qualified.MaxConcurrent)

	legacy, err := s.CheckAllowance(context.Background(), "t", "anthropic/legacy-model", now, now.Add(-time.Hour), now.Add(time.Hour), 0)
	require.NoError(t, err)
	require.True(t, legacy.ModelAllowed)

	disallowed, err := s.CheckAllowance(context.Background(), "t", "openai/model-b", now, now.Add(-time.Hour), now.Add(time.Hour), 0)
	require.NoError(t, err)
	require.False(t, disallowed.ModelAllowed)
	require.False(t, disallowed.Allowed)

	require.NoError(t, s.CreatePlan(context.Background(), billing.Plan{ID: "wildcard", Name: "Wildcard", IncludedCreditMicros: 300, MaxConcurrent: 2, OveragePolicy: "reject"}, []string{"*"}))
	require.NoError(t, s.SetTenantPlan(context.Background(), "t", "wildcard", now.Add(-time.Hour)))
	fallback, err := s.CheckAllowance(context.Background(), "t", "openai/model-b", now, now.Add(-time.Hour), now.Add(time.Hour), 0)
	require.NoError(t, err)
	require.True(t, fallback.ModelAllowed)
	require.Equal(t, 2, fallback.MaxConcurrent)
}

func TestModelPolicyAndAllowanceRequireConfiguredPricing(t *testing.T) {
	s := billingStore(t)
	now := time.Now().UTC()
	require.NoError(t, s.CreatePlan(context.Background(), billing.Plan{ID: "priced", Name: "Priced", IncludedCreditMicros: 300, MaxConcurrent: 2, OveragePolicy: "reject"}, []string{"openai/model-a", "openai/model-b"}))
	require.NoError(t, s.SetTenantPlan(context.Background(), "t", "priced", now.Add(-time.Hour)))
	require.NoError(t, s.UpsertSKU(context.Background(), billing.SKU{ID: "model-a", Name: "Legacy price", PromptMicrosPer1M: 1}))

	policy, err := s.CurrentModelPolicy(context.Background(), "t", now)
	require.NoError(t, err)
	require.True(t, policy.Allows("openai/model-a"))
	require.False(t, policy.Allows("openai/model-b"))

	priced, err := s.CheckAllowance(context.Background(), "t", "openai/model-a", now, now.Add(-time.Hour), now.Add(time.Hour), 0)
	require.NoError(t, err)
	require.True(t, priced.ModelAllowed)
	require.True(t, priced.PricingConfigured)
	require.True(t, priced.Allowed)

	unpriced, err := s.CheckAllowance(context.Background(), "t", "openai/model-b", now, now.Add(-time.Hour), now.Add(time.Hour), 0)
	require.NoError(t, err)
	require.True(t, unpriced.ModelAllowed)
	require.False(t, unpriced.PricingConfigured)
	require.False(t, unpriced.Allowed)
}

func TestCatalogTxRollsBackPlanAndSKU(t *testing.T) {
	s := billingStore(t)
	sentinel := errors.New("audit failed")
	err := s.DB().Transaction(func(tx *gorm.DB) error {
		require.NoError(t, s.CreatePlanTx(tx, billing.Plan{ID: "rollback-plan", Name: "Rollback", IncludedCreditMicros: 100, MaxConcurrent: 1, OveragePolicy: "reject"}, []string{"*"}))
		require.NoError(t, s.UpsertSKUTx(tx, billing.SKU{ID: "rollback-model", Name: "Rollback model", PromptMicrosPer1M: 1}))
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)
	var plans, skus int64
	require.NoError(t, s.DB().Model(&billing.Plan{}).Where("id = ?", "rollback-plan").Count(&plans).Error)
	require.NoError(t, s.DB().Model(&billing.SKU{}).Where("id = ?", "rollback-model").Count(&skus).Error)
	require.Zero(t, plans)
	require.Zero(t, skus)
}

func TestUpdatePlanReplacesMutableTermsAndModels(t *testing.T) {
	s := billingStore(t)
	require.NoError(t, s.CreatePlan(context.Background(), billing.Plan{ID: "editable", Name: "Before", MonthlyMicros: 10, IncludedCreditMicros: 100, MaxConcurrent: 2, OveragePolicy: "reject"}, []string{"openai/model-a"}))
	var created billing.Plan
	require.NoError(t, s.DB().Where("id = ?", "editable").Take(&created).Error)

	require.NoError(t, s.DB().Transaction(func(tx *gorm.DB) error {
		return s.UpdatePlanTx(tx, billing.Plan{ID: "editable", Name: "After", MonthlyMicros: 20, IncludedCreditMicros: 250, MaxConcurrent: 7, OveragePolicy: "allow"}, []string{"openai/model-b", "openai/model-c"})
	}))
	rows, err := s.ListPlans(context.Background())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "After", rows[0].Plan.Name)
	require.Equal(t, int64(20), rows[0].Plan.MonthlyMicros)
	require.Equal(t, int64(250), rows[0].Plan.IncludedCreditMicros)
	require.Equal(t, 7, rows[0].Plan.MaxConcurrent)
	require.Equal(t, "allow", rows[0].Plan.OveragePolicy)
	require.Equal(t, created.CreatedAt, rows[0].Plan.CreatedAt)
	require.Equal(t, []string{"openai/model-b", "openai/model-c"}, rows[0].Models)
}

func TestSKUListAndFallbackPricing(t *testing.T) {
	s := billingStore(t)
	require.NoError(t, s.UpsertSKU(context.Background(), billing.SKU{ID: "*", Name: "Fallback", PromptMicrosPer1M: 100_000, CompletionMicrosPer1M: 200_000}))
	require.NoError(t, s.UpsertSKU(context.Background(), billing.SKU{ID: "model-a", Name: "Model A", PromptMicrosPer1M: 300_000, CompletionMicrosPer1M: 400_000}))
	require.NoError(t, s.UpsertSKU(context.Background(), billing.SKU{ID: "openai/model-a", Name: "OpenAI Model A", PromptMicrosPer1M: 500_000, CompletionMicrosPer1M: 600_000}))
	rows, err := s.ListSKUs(context.Background())
	require.NoError(t, err)
	require.Len(t, rows, 3)
	exact, err := s.PriceUsage(context.Background(), "model-a", 1000, 1000)
	require.NoError(t, err)
	require.Equal(t, int64(700), exact)
	qualified, err := s.PriceUsage(context.Background(), "openai/model-a", 1000, 1000)
	require.NoError(t, err)
	require.Equal(t, int64(1100), qualified)
	legacy, err := s.PriceUsage(context.Background(), "anthropic/model-a", 1000, 1000)
	require.NoError(t, err)
	require.Equal(t, int64(700), legacy)
	fallback, err := s.PriceUsage(context.Background(), "unknown", 1000, 1000)
	require.NoError(t, err)
	require.Equal(t, int64(300), fallback)
}

func TestMigrateConvertsLegacySKUPricesOnce(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:billing_legacy_"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.Exec(`CREATE TABLE billing_skus (
		id text PRIMARY KEY,
		name text NOT NULL,
		prompt_micros_per_1k integer NOT NULL,
		completion_micros_per_1k integer NOT NULL,
		created_at datetime NOT NULL
	)`).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO billing_skus (id, name, prompt_micros_per_1k, completion_micros_per_1k, created_at) VALUES (?, ?, ?, ?, ?)",
		"openai/model-a", "Model A", int64(7), int64(11), time.Now().UTC(),
	).Error)

	store := billing.NewStore(db)
	require.NoError(t, store.Migrate(context.Background()))
	var migrated billing.SKU
	require.NoError(t, db.Where("id = ?", "openai/model-a").Take(&migrated).Error)
	require.Equal(t, int64(7_000), migrated.PromptMicrosPer1M)
	require.Equal(t, int64(11_000), migrated.CompletionMicrosPer1M)

	// A repeated startup sees the new columns and must not scale them again.
	require.NoError(t, store.Migrate(context.Background()))
	migrated = billing.SKU{}
	require.NoError(t, db.Where("id = ?", "openai/model-a").Take(&migrated).Error)
	require.Equal(t, int64(7_000), migrated.PromptMicrosPer1M)
	require.Equal(t, int64(11_000), migrated.CompletionMicrosPer1M)
}
