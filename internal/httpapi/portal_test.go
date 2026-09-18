package httpapi

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/billing"
	"github.com/luojinghua50/Joysteed-MaaS/internal/member"
	"github.com/stretchr/testify/require"
)

func int64Pointer(value int64) *int64 { return &value }

func TestSKUWriteRequestConvertsLegacyRates(t *testing.T) {
	sku, err := (skuWriteRequest{
		ID:                    "openai/model-a",
		Name:                  "Model A",
		PromptMicrosPer1K:     int64Pointer(7),
		CompletionMicrosPer1K: int64Pointer(11),
	}).sku()
	require.NoError(t, err)
	require.Equal(t, int64(7_000), sku.PromptMicrosPer1M)
	require.Equal(t, int64(11_000), sku.CompletionMicrosPer1M)
}

func TestSKUWriteRequestPrefersCurrentRates(t *testing.T) {
	sku, err := (skuWriteRequest{
		PromptMicrosPer1M:     int64Pointer(13),
		CompletionMicrosPer1M: int64Pointer(17),
		PromptMicrosPer1K:     int64Pointer(7),
		CompletionMicrosPer1K: int64Pointer(11),
	}).sku()
	require.NoError(t, err)
	require.Equal(t, int64(13), sku.PromptMicrosPer1M)
	require.Equal(t, int64(17), sku.CompletionMicrosPer1M)
}

func TestSKUWriteRequestRejectsInvalidLegacyRates(t *testing.T) {
	for _, value := range []int64{-1, math.MaxInt64/1000 + 1} {
		_, err := (skuWriteRequest{PromptMicrosPer1K: int64Pointer(value)}).sku()
		require.ErrorIs(t, err, billing.ErrInvalidUsage)
	}
}

func TestParsePortalKeyRevealPath(t *testing.T) {
	keyID, action, ok := parsePortalKeyPath("/api/portal/keys/vk_123/reveal")
	require.True(t, ok)
	require.Equal(t, "vk_123", keyID)
	require.Equal(t, "reveal", action)
	_, _, ok = parsePortalKeyPath("/api/portal/keys/vk_123/secret")
	require.False(t, ok)
}

func TestPortalUsageSupportsPaginationAndFilters(t *testing.T) {
	server := requestLogTestServer(t, http.DefaultClient)
	require.NoError(t, server.billing.Migrate(context.Background()))
	access := addRequestLogTenant(t, server, "tenant-usage", "usage", "vk-usage", member.RoleDeveloper)
	now := time.Now().UTC()
	duration := int64(740)
	for _, input := range []billing.UsageInput{
		{TenantID: "tenant-usage", IdempotencyKey: "usage-success", PeriodStart: now, KeyOwnership: billing.KeyOwnershipPlatformPool, Model: "openai/model-a", Status: billing.UsageStatusSuccess},
		{TenantID: "tenant-usage", IdempotencyKey: "usage-failed", PeriodStart: now.Add(-time.Minute), KeyOwnership: billing.KeyOwnershipPlatformPool, Model: "openai/model-b", Status: billing.UsageStatusFailed, DurationMS: &duration},
	} {
		_, err := server.billing.RecordUsage(context.Background(), input)
		require.NoError(t, err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/portal/usage?limit=1&offset=0&status=failed&model=openai%2Fmodel-b", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)
	require.Equal(t, http.StatusOK, recorder.Code)
	var payload struct {
		Events     []billing.UsageEvent `json:"events"`
		Models     []string             `json:"models"`
		TotalCount int64                `json:"total_count"`
		Limit      int                  `json:"limit"`
		Offset     int                  `json:"offset"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &payload))
	require.Equal(t, int64(1), payload.TotalCount)
	require.Equal(t, 1, payload.Limit)
	require.Zero(t, payload.Offset)
	require.Equal(t, []string{"openai/model-a", "openai/model-b"}, payload.Models)
	require.Len(t, payload.Events, 1)
	require.Equal(t, billing.UsageStatusFailed, payload.Events[0].Status)
	require.Equal(t, duration, *payload.Events[0].DurationMS)

	invalid := httptest.NewRequest(http.MethodGet, "/api/portal/usage?status=not-a-status", nil)
	invalid.Header.Set("Authorization", "Bearer "+access)
	invalidRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(invalidRecorder, invalid)
	require.Equal(t, http.StatusBadRequest, invalidRecorder.Code)
}
