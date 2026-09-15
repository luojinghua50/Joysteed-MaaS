package billing_test

import (
	"context"
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

func TestPlanQuotaCheckUsesDurableUsage(t *testing.T) {
	s := billingStore(t)
	require.NoError(t, s.CreatePlan(context.Background(), billing.Plan{ID: "starter", Name: "Starter", OveragePolicy: "reject"}, []billing.PlanQuota{{PlanID: "starter", SKU: "model-a", IncludedTokens: 10, MaxConcurrent: 2, Priority: 1}}))
	require.NoError(t, s.SetTenantPlan(context.Background(), "t", "starter", time.Now().Add(-time.Hour)))
	now := time.Now().UTC()
	_, err := s.RecordUsage(context.Background(), billing.UsageInput{TenantID: "t", IdempotencyKey: "quota-1", PeriodStart: now, KeyOwnership: billing.KeyOwnershipBYOK, Model: "model-a", PromptTokens: 6})
	require.NoError(t, err)
	status, err := s.CheckQuota(context.Background(), "t", "model-a", now, now.Add(-time.Hour), now.Add(time.Hour), 5)
	require.NoError(t, err)
	require.Equal(t, int64(6), status.UsedTokens)
	require.False(t, status.Allowed)
}
