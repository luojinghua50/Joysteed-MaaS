// Package tenantusage connects MaaS plan admission, concurrency fairness and
// durable usage accounting to Bifrost's typed provider lifecycle.
package tenantusage

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/billing"
	"github.com/luojinghua50/Joysteed-MaaS/internal/fairness"
	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	tenantid "github.com/luojinghua50/Joysteed-MaaS/internal/tenantid"
	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
)

const PluginName = "maas-tenantusage"

type BillingStore interface {
	CheckAllowance(context.Context, tenantid.ID, string, time.Time, time.Time, time.Time, int64) (billing.AllowanceStatus, error)
	RecordUsage(context.Context, billing.UsageInput) (*billing.UsageEvent, error)
	PriceUsage(context.Context, string, int64, int64) (int64, error)
}

type Semaphore interface {
	Acquire(context.Context, string, string, int, int, time.Duration) (fairness.Lease, error)
	Renew(context.Context, fairness.Lease, time.Duration) error
	Release(context.Context, fairness.Lease) error
}

type Counter interface {
	Usage(context.Context, tenantid.ID, string) (int64, error)
	ChargeOnce(context.Context, tenantid.ID, string, string, int64, time.Duration) (int64, bool, error)
}

type Config struct {
	Billing               BillingStore
	Semaphore             Semaphore
	Counter               Counter
	LeaseTTL              time.Duration
	ProviderMaxConcurrent int
	MarkupBasisPoints     int64
	Now                   func() time.Time
}

type Plugin struct {
	billing               BillingStore
	semaphore             Semaphore
	counter               Counter
	leaseTTL              time.Duration
	providerMaxConcurrent int
	markupBasisPoints     int64
	now                   func() time.Time
}

type admissionKey struct{}

type admission struct {
	mu        sync.Mutex
	settled   bool
	tenantID  tenantid.ID
	provider  string
	model     string
	requestID string
	attempt   int
	budget    string
	windowEnd time.Time
	startedAt time.Time
	lease     fairness.Lease
	stopRenew chan struct{}
}

func New(cfg Config) (*Plugin, error) {
	if cfg.Billing == nil || cfg.Semaphore == nil {
		return nil, errors.New("tenantusage: billing and semaphore are required")
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 5 * time.Minute
	}
	if cfg.ProviderMaxConcurrent < 0 || cfg.MarkupBasisPoints < 0 {
		return nil, errors.New("tenantusage: invalid limits")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Plugin{billing: cfg.Billing, semaphore: cfg.Semaphore, counter: cfg.Counter, leaseTTL: cfg.LeaseTTL, providerMaxConcurrent: cfg.ProviderMaxConcurrent, markupBasisPoints: cfg.MarkupBasisPoints, now: cfg.Now}, nil
}

func (*Plugin) GetName() string                                                           { return PluginName }
func (*Plugin) Cleanup() error                                                            { return nil }
func (*Plugin) PreRequestHook(_ *schemas.BifrostContext, _ *schemas.BifrostRequest) error { return nil }

func (p *Plugin) PreLLMHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	if p == nil || ctx == nil || req == nil {
		return req, nil, nil
	}
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" {
		return req, shortCircuit(403, "tenant_required", "Resolved tenant identity is required"), nil
	}
	provider, model, _ := req.GetRequestFields()
	if model == "" {
		return req, nil, nil
	}
	providerName := string(provider)
	if providerName == "" {
		providerName = "unresolved"
	}
	sku := canonicalSKU(providerName, model)
	now := p.now().UTC()
	from, to := monthWindow(now)
	allowance, err := p.billing.CheckAllowance(ctx, tenantID, sku, now, from, to, 0)
	if err != nil {
		ctx.Log(schemas.LogLevelError, fmt.Sprintf("tenant allowance lookup failed: %v", err))
		return req, shortCircuit(503, "allowance_unavailable", "Tenant allowance is temporarily unavailable"), nil
	}
	if !allowance.ModelAllowed {
		return req, shortCircuit(403, "model_not_allowed", "Model is not available on the tenant plan"), nil
	}
	if !allowance.PricingConfigured {
		return req, shortCircuit(403, "pricing_not_configured", "Model pricing is not configured"), nil
	}
	budget := counterBudget(from)
	if p.counter != nil {
		used, counterErr := p.counter.Usage(ctx, tenantID, budget)
		if counterErr != nil {
			ctx.Log(schemas.LogLevelError, fmt.Sprintf("tenant allowance cache lookup failed: %v", counterErr))
			return req, shortCircuit(503, "allowance_unavailable", "Tenant allowance is temporarily unavailable"), nil
		}
		if used > allowance.UsedCreditMicros {
			allowance.UsedCreditMicros = used
			allowance.RemainingCreditMicros = allowance.IncludedCreditMicros - used
			if allowance.RemainingCreditMicros < 0 {
				allowance.RemainingCreditMicros = 0
			}
			allowance.Allowed = allowance.OveragePolicy != "reject" || used < allowance.IncludedCreditMicros
		}
	}
	if !allowance.Allowed {
		return req, shortCircuit(429, "credit_exhausted", "Tenant included credit is exhausted"), nil
	}
	lease, err := p.semaphore.Acquire(ctx, string(tenantID), providerName, allowance.MaxConcurrent, p.providerMaxConcurrent, p.leaseTTL)
	if errors.Is(err, fairness.ErrUnavailable) {
		return req, shortCircuit(429, "concurrency_exceeded", "Tenant concurrency limit is reached"), nil
	}
	if err != nil {
		ctx.Log(schemas.LogLevelError, fmt.Sprintf("tenant concurrency admission failed: %v", err))
		return req, shortCircuit(503, "admission_unavailable", "Tenant admission is temporarily unavailable"), nil
	}
	requestID := bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeyRequestID)
	attempt := bifrost.GetIntFromContext(ctx, schemas.BifrostContextKeyNumberOfRetries)
	state := &admission{tenantID: tenantID, provider: providerName, model: sku, requestID: requestID, attempt: attempt, budget: budget, windowEnd: to, startedAt: now, lease: lease, stopRenew: make(chan struct{})}
	ctx.SetValue(admissionKey{}, state)
	p.keepLeaseAlive(ctx, state)
	return req, nil, nil
}

func canonicalSKU(provider, model string) string {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" || provider == "unresolved" || strings.HasPrefix(model, provider+"/") {
		return model
	}
	return provider + "/" + model
}

func (p *Plugin) PostLLMHook(ctx *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	if p == nil || ctx == nil {
		return resp, bifrostErr, nil
	}
	state, _ := ctx.Value(admissionKey{}).(*admission)
	if state == nil {
		return resp, bifrostErr, nil
	}
	requestType, _, _, _ := bifrost.GetResponseFields(resp, bifrostErr)
	if bifrost.IsStreamRequestType(requestType) && bifrostErr == nil && !bifrost.IsFinalChunk(ctx) {
		return resp, bifrostErr, nil
	}
	state.mu.Lock()
	if state.settled {
		state.mu.Unlock()
		return resp, bifrostErr, nil
	}
	state.settled = true
	close(state.stopRenew)
	state.mu.Unlock()

	if err := p.semaphore.Release(ctx, state.lease); err != nil {
		ctx.Log(schemas.LogLevelError, fmt.Sprintf("tenant concurrency release failed: %v", err))
	}
	usage := responseUsage(resp, bifrostErr)
	if usage == nil {
		return resp, bifrostErr, nil
	}
	usage = usage.DeepCopy()
	usage.NormalizeProviderCost()
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	providerMicros := costMicros(usage.Cost)
	if providerMicros == 0 {
		priced, priceErr := p.billing.PriceUsage(ctx, state.model, int64(usage.PromptTokens), int64(usage.CompletionTokens))
		if priceErr != nil {
			ctx.Log(schemas.LogLevelError, fmt.Sprintf("tenant usage pricing failed: %v", priceErr))
		} else {
			providerMicros = priced
		}
	}
	if usage.TotalTokens <= 0 && providerMicros <= 0 {
		return resp, bifrostErr, nil
	}
	idempotency := state.requestID
	if idempotency == "" {
		idempotency = "lease:" + state.lease.Token
	}
	gatewayLogID := state.requestID
	if fallbackID := bifrost.GetStringFromContext(ctx, schemas.BifrostContextKeyFallbackRequestID); fallbackID != "" {
		gatewayLogID = fallbackID
	}
	idempotency += ":attempt:" + strconv.Itoa(state.attempt)
	markup := providerMicros * p.markupBasisPoints / 10_000
	chargeMicros := providerMicros + markup
	durationMS := p.now().Sub(state.startedAt).Milliseconds()
	if durationMS < 0 {
		durationMS = 0
	}
	status := billing.UsageStatusSuccess
	if bifrostErr != nil {
		status = billing.UsageStatusFailed
	}
	if p.counter != nil && chargeMicros > 0 {
		ttl := time.Until(state.windowEnd) + 24*time.Hour
		if ttl < time.Hour {
			ttl = time.Hour
		}
		if _, _, err := p.counter.ChargeOnce(ctx, state.tenantID, state.budget, idempotency, chargeMicros, ttl); err != nil {
			ctx.Log(schemas.LogLevelError, fmt.Sprintf("tenant allowance cache charge failed: %v", err))
		}
	}
	_, err := p.billing.RecordUsage(ctx, billing.UsageInput{ID: idempotency, IdempotencyKey: idempotency, GatewayLogID: gatewayLogID, Attempt: state.attempt, TenantID: state.tenantID, PeriodStart: p.now().UTC(), KeyOwnership: billing.KeyOwnershipPlatformPool, Model: state.model, Status: status, DurationMS: &durationMS, PromptTokens: int64(usage.PromptTokens), CompletionTokens: int64(usage.CompletionTokens), ProviderCostMicros: providerMicros, MarkupMicros: markup})
	if err != nil {
		// Redis is charged first, so a transient ledger failure cannot reopen the
		// allowance. Bifrost request logs remain the recovery source for replay.
		ctx.Log(schemas.LogLevelError, fmt.Sprintf("tenant usage persistence failed: %v", err))
	}
	return resp, bifrostErr, nil
}

func (p *Plugin) keepLeaseAlive(ctx *schemas.BifrostContext, state *admission) {
	interval := p.leaseTTL / 3
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-state.stopRenew:
				return
			case <-ticker.C:
				if err := p.semaphore.Renew(ctx, state.lease, p.leaseTTL); err != nil {
					ctx.Log(schemas.LogLevelError, fmt.Sprintf("tenant concurrency renewal failed: %v", err))
					if errors.Is(err, fairness.ErrLeaseExpired) {
						return
					}
				}
			}
		}
	}()
}

func monthWindow(now time.Time) (time.Time, time.Time) {
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return from, from.AddDate(0, 1, 0)
}

func counterBudget(from time.Time) string {
	return "month:" + from.Format("2006-01") + ":credit"
}

func responseUsage(resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) *schemas.BifrostLLMUsage {
	if resp == nil {
		if bifrostErr != nil {
			return bifrostErr.ExtraFields.BilledUsage
		}
		return nil
	}
	switch {
	case resp.TextCompletionResponse != nil:
		return resp.TextCompletionResponse.Usage
	case resp.ChatResponse != nil:
		return resp.ChatResponse.Usage
	case resp.ResponsesResponse != nil && resp.ResponsesResponse.Usage != nil:
		return resp.ResponsesResponse.Usage.ToBifrostLLMUsage()
	case resp.ResponsesStreamResponse != nil && resp.ResponsesStreamResponse.Response != nil && resp.ResponsesStreamResponse.Response.Usage != nil:
		return resp.ResponsesStreamResponse.Response.Usage.ToBifrostLLMUsage()
	case resp.CompactionResponse != nil && resp.CompactionResponse.Usage != nil:
		return resp.CompactionResponse.Usage.ToBifrostLLMUsage()
	case resp.EmbeddingResponse != nil:
		return resp.EmbeddingResponse.Usage
	case resp.RerankResponse != nil:
		return resp.RerankResponse.Usage
	case resp.SpeechResponse != nil && resp.SpeechResponse.Usage != nil:
		return speechUsage(resp.SpeechResponse.Usage)
	case resp.SpeechStreamResponse != nil && resp.SpeechStreamResponse.Usage != nil:
		return speechUsage(resp.SpeechStreamResponse.Usage)
	case resp.TranscriptionResponse != nil && resp.TranscriptionResponse.Usage != nil:
		return transcriptionUsage(resp.TranscriptionResponse.Usage)
	case resp.TranscriptionStreamResponse != nil && resp.TranscriptionStreamResponse.Usage != nil:
		return transcriptionUsage(resp.TranscriptionStreamResponse.Usage)
	case resp.ImageGenerationResponse != nil && resp.ImageGenerationResponse.Usage != nil:
		value := resp.ImageGenerationResponse.Usage.DeepCopy()
		value.NormalizeProviderCost()
		return &schemas.BifrostLLMUsage{PromptTokens: value.InputTokens, CompletionTokens: value.OutputTokens, TotalTokens: value.TotalTokens, Cost: value.Cost}
	case resp.PassthroughResponse != nil && resp.PassthroughResponse.PassthroughUsage != nil:
		return resp.PassthroughResponse.PassthroughUsage.LLMUsage
	case resp.CountTokensResponse != nil:
		total := resp.CountTokensResponse.InputTokens
		completion := 0
		if resp.CountTokensResponse.OutputTokens != nil {
			completion = *resp.CountTokensResponse.OutputTokens
		}
		if resp.CountTokensResponse.TotalTokens != nil {
			total = *resp.CountTokensResponse.TotalTokens
		}
		return &schemas.BifrostLLMUsage{PromptTokens: resp.CountTokensResponse.InputTokens, CompletionTokens: completion, TotalTokens: total}
	default:
		return nil
	}
}

func speechUsage(value *schemas.SpeechUsage) *schemas.BifrostLLMUsage {
	if value == nil {
		return nil
	}
	return &schemas.BifrostLLMUsage{PromptTokens: value.InputTokens, CompletionTokens: value.OutputTokens, TotalTokens: value.TotalTokens, Cost: value.Cost}
}

func transcriptionUsage(value *schemas.TranscriptionUsage) *schemas.BifrostLLMUsage {
	if value == nil {
		return nil
	}
	usage := &schemas.BifrostLLMUsage{Cost: value.Cost}
	if value.InputTokens != nil {
		usage.PromptTokens = *value.InputTokens
	}
	if value.OutputTokens != nil {
		usage.CompletionTokens = *value.OutputTokens
	}
	if value.TotalTokens != nil {
		usage.TotalTokens = *value.TotalTokens
	}
	return usage
}

func costMicros(cost *schemas.BifrostCost) int64 {
	if cost == nil || cost.TotalCost <= 0 {
		return 0
	}
	return int64(math.Round(cost.TotalCost * 1_000_000))
}

func shortCircuit(status int, typ, message string) *schemas.LLMPluginShortCircuit {
	allowFallbacks := false
	return &schemas.LLMPluginShortCircuit{Error: &schemas.BifrostError{StatusCode: &status, Error: &schemas.ErrorField{Type: &typ, Message: message}, AllowFallbacks: &allowFallbacks}}
}

var _ schemas.LLMPlugin = (*Plugin)(nil)
