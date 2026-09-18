package tenantusage_test

import (
	"context"
	"testing"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/billing"
	"github.com/luojinghua50/Joysteed-MaaS/internal/fairness"
	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	tenantid "github.com/luojinghua50/Joysteed-MaaS/internal/tenantid"
	"github.com/luojinghua50/Joysteed-MaaS/plugins/tenantusage"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

type fakeBilling struct {
	allowance      billing.AllowanceStatus
	input          *billing.UsageInput
	price          int64
	allowanceModel string
	priceSKU       string
}

func (f *fakeBilling) CheckAllowance(_ context.Context, _ tenantid.ID, model string, _ time.Time, _ time.Time, _ time.Time, _ int64) (billing.AllowanceStatus, error) {
	f.allowanceModel = model
	return f.allowance, nil
}
func (f *fakeBilling) RecordUsage(_ context.Context, in billing.UsageInput) (*billing.UsageEvent, error) {
	f.input = &in
	return &billing.UsageEvent{}, nil
}
func (f *fakeBilling) PriceUsage(_ context.Context, sku string, _, _ int64) (int64, error) {
	f.priceSKU = sku
	return f.price, nil
}

type fakeSemaphore struct {
	acquired, released int
	err                error
	renewed            chan struct{}
}

func (f *fakeSemaphore) Acquire(_ context.Context, tenantID, provider string, _, _ int, _ time.Duration) (fairness.Lease, error) {
	f.acquired++
	if f.err != nil {
		return fairness.Lease{}, f.err
	}
	return fairness.Lease{Token: "lease", TenantID: tenantID, Provider: provider}, nil
}
func (f *fakeSemaphore) Renew(context.Context, fairness.Lease, time.Duration) error {
	if f.renewed != nil {
		select {
		case f.renewed <- struct{}{}:
		default:
		}
	}
	return nil
}
func (f *fakeSemaphore) Release(context.Context, fairness.Lease) error { f.released++; return nil }

type fakeCounter struct {
	used, charged int64
	calls         int
}

func (f *fakeCounter) Usage(context.Context, tenantid.ID, string) (int64, error) { return f.used, nil }
func (f *fakeCounter) ChargeOnce(_ context.Context, _ tenantid.ID, _, _ string, amount int64, _ time.Duration) (int64, bool, error) {
	f.calls++
	f.charged += amount
	return f.charged, true, nil
}

func requestContext(t *testing.T) *schemas.BifrostContext {
	t.Helper()
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	require.NoError(t, tenant.SetResolvedTenant(ctx, "tenant-a"))
	ctx.SetValue(schemas.BifrostContextKeyRequestID, "request-1")
	ctx.SetValue(schemas.BifrostContextKeyNumberOfRetries, 0)
	return ctx
}

func chatRequest() *schemas.BifrostRequest {
	return &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{Provider: schemas.OpenAI, Model: "gpt-test"}}
}

func TestPluginAdmitsSettlesAndReleases(t *testing.T) {
	bills := &fakeBilling{allowance: billing.AllowanceStatus{ModelAllowed: true, PricingConfigured: true, IncludedCreditMicros: 1000, RemainingCreditMicros: 1000, MaxConcurrent: 2, OveragePolicy: "reject", Allowed: true}, price: 250}
	semaphore := &fakeSemaphore{}
	counter := &fakeCounter{}
	plugin, err := tenantusage.New(tenantusage.Config{Billing: bills, Semaphore: semaphore, Counter: counter, MarkupBasisPoints: 1000, Now: func() time.Time { return time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC) }})
	require.NoError(t, err)
	ctx := requestContext(t)
	_, short, err := plugin.PreLLMHook(ctx, chatRequest())
	require.NoError(t, err)
	require.Nil(t, short)
	require.Equal(t, 1, semaphore.acquired)

	resp := &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{Usage: &schemas.BifrostLLMUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}, ExtraFields: schemas.BifrostResponseExtraFields{RequestType: schemas.ChatCompletionRequest}}}
	_, _, err = plugin.PostLLMHook(ctx, resp, nil)
	require.NoError(t, err)
	require.Equal(t, 1, semaphore.released)
	require.NotNil(t, bills.input)
	require.Equal(t, int64(15), bills.input.PromptTokens+bills.input.CompletionTokens)
	require.Equal(t, int64(250), bills.input.ProviderCostMicros)
	require.Equal(t, int64(25), bills.input.MarkupMicros)
	require.Equal(t, "openai/gpt-test", bills.allowanceModel)
	require.Equal(t, "openai/gpt-test", bills.priceSKU)
	require.Equal(t, "openai/gpt-test", bills.input.Model)
	require.Equal(t, "request-1", bills.input.GatewayLogID)
	require.Zero(t, bills.input.Attempt)
	require.Equal(t, billing.UsageStatusSuccess, bills.input.Status)
	require.NotNil(t, bills.input.DurationMS)
	require.Zero(t, *bills.input.DurationMS)
	require.Equal(t, 1, counter.calls)
	require.Equal(t, int64(275), counter.charged)

	_, _, _ = plugin.PostLLMHook(ctx, resp, nil)
	require.Equal(t, 1, semaphore.released)
	require.Equal(t, 1, counter.calls)
}

func TestPluginCorrelatesFallbackAttemptWithItsGatewayLog(t *testing.T) {
	bills := &fakeBilling{allowance: billing.AllowanceStatus{ModelAllowed: true, PricingConfigured: true, IncludedCreditMicros: 1000, RemainingCreditMicros: 1000, MaxConcurrent: 2, OveragePolicy: "reject", Allowed: true}, price: 1}
	plugin, err := tenantusage.New(tenantusage.Config{Billing: bills, Semaphore: &fakeSemaphore{}})
	require.NoError(t, err)
	ctx := requestContext(t)
	ctx.SetValue(schemas.BifrostContextKeyNumberOfRetries, 2)
	_, short, err := plugin.PreLLMHook(ctx, chatRequest())
	require.NoError(t, err)
	require.Nil(t, short)
	ctx.SetValue(schemas.BifrostContextKeyFallbackRequestID, "fallback-request-2")
	resp := &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{Usage: &schemas.BifrostLLMUsage{PromptTokens: 1}, ExtraFields: schemas.BifrostResponseExtraFields{RequestType: schemas.ChatCompletionRequest}}}
	_, _, err = plugin.PostLLMHook(ctx, resp, nil)
	require.NoError(t, err)
	require.Equal(t, "fallback-request-2", bills.input.GatewayLogID)
	require.Equal(t, 2, bills.input.Attempt)
}

func TestPluginRejectsModelCreditAndConcurrency(t *testing.T) {
	bills := &fakeBilling{allowance: billing.AllowanceStatus{ModelAllowed: false, IncludedCreditMicros: 10, MaxConcurrent: 1, OveragePolicy: "reject", Allowed: false}}
	semaphore := &fakeSemaphore{}
	plugin, err := tenantusage.New(tenantusage.Config{Billing: bills, Semaphore: semaphore})
	require.NoError(t, err)
	_, short, err := plugin.PreLLMHook(requestContext(t), chatRequest())
	require.NoError(t, err)
	require.NotNil(t, short)
	require.Equal(t, "model_not_allowed", *short.Error.Error.Type)
	require.Zero(t, semaphore.acquired)

	bills.allowance.ModelAllowed = true
	bills.allowance.PricingConfigured = true
	_, short, err = plugin.PreLLMHook(requestContext(t), chatRequest())
	require.NoError(t, err)
	require.NotNil(t, short)
	require.Equal(t, "credit_exhausted", *short.Error.Error.Type)
	require.Zero(t, semaphore.acquired)

	bills.allowance.Allowed = true
	semaphore.err = fairness.ErrUnavailable
	_, short, err = plugin.PreLLMHook(requestContext(t), chatRequest())
	require.NoError(t, err)
	require.NotNil(t, short)
	require.Equal(t, "concurrency_exceeded", *short.Error.Error.Type)
}

func TestPluginRejectsModelWithoutConfiguredPricing(t *testing.T) {
	bills := &fakeBilling{allowance: billing.AllowanceStatus{ModelAllowed: true, PricingConfigured: false, IncludedCreditMicros: 1000, RemainingCreditMicros: 1000, MaxConcurrent: 1, OveragePolicy: "reject", Allowed: false}}
	semaphore := &fakeSemaphore{}
	plugin, err := tenantusage.New(tenantusage.Config{Billing: bills, Semaphore: semaphore})
	require.NoError(t, err)

	_, short, err := plugin.PreLLMHook(requestContext(t), chatRequest())
	require.NoError(t, err)
	require.NotNil(t, short)
	require.Equal(t, "pricing_not_configured", *short.Error.Error.Type)
	require.Zero(t, semaphore.acquired)
}

func TestPluginRenewsLongRunningLease(t *testing.T) {
	bills := &fakeBilling{allowance: billing.AllowanceStatus{ModelAllowed: true, PricingConfigured: true, IncludedCreditMicros: 1000, RemainingCreditMicros: 1000, MaxConcurrent: 1, OveragePolicy: "reject", Allowed: true}}
	semaphore := &fakeSemaphore{renewed: make(chan struct{}, 1)}
	plugin, err := tenantusage.New(tenantusage.Config{Billing: bills, Semaphore: semaphore, LeaseTTL: 30 * time.Millisecond})
	require.NoError(t, err)
	ctx := requestContext(t)
	_, short, err := plugin.PreLLMHook(ctx, chatRequest())
	require.NoError(t, err)
	require.Nil(t, short)
	select {
	case <-semaphore.renewed:
	case <-time.After(time.Second):
		t.Fatal("expected concurrency lease renewal")
	}
	_, _, err = plugin.PostLLMHook(ctx, nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1, semaphore.released)
}
