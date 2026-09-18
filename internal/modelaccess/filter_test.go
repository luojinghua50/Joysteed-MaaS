package modelaccess_test

import (
	"context"
	"testing"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/billing"
	"github.com/luojinghua50/Joysteed-MaaS/internal/modelaccess"
	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	tenantid "github.com/luojinghua50/Joysteed-MaaS/internal/tenantid"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

type policyStore struct {
	policy billing.ModelPolicy
	err    error
}

func (s policyStore) CurrentModelPolicy(context.Context, tenantid.ID, time.Time) (billing.ModelPolicy, error) {
	return s.policy, s.err
}

func TestFilterKeepsOnlyPlanPricedModelsAndRelevantStatuses(t *testing.T) {
	filter, err := modelaccess.New(policyStore{policy: billing.ModelPolicy{
		PlanID:       "gpt-month",
		PlanModels:   []string{"openai/gpt-5.5", "openai/gpt-5.6-sol", "openai/gpt-unpriced"},
		PricedModels: []string{"openai/gpt-5.5", "gpt-5.6-sol"},
	}})
	require.NoError(t, err)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	require.NoError(t, tenant.SetResolvedTenant(ctx, "tenant-a"))
	response := &schemas.BifrostListModelsResponse{
		Data: []schemas.Model{
			{ID: "anthropic/claude-opus"},
			{ID: "openai/gpt-5.5"},
			{ID: "openai/gpt-5.6-sol"},
			{ID: "openai/gpt-unpriced"},
		},
		KeyStatuses: []schemas.KeyStatus{
			{KeyID: "openai-key", Provider: schemas.OpenAI, Status: schemas.KeyStatusSuccess},
			{KeyID: "anthropic-key", Provider: schemas.Anthropic, Status: schemas.KeyStatusSuccess},
		},
	}

	require.NoError(t, filter.FilterListModels(ctx, response))
	require.Equal(t, []string{"openai/gpt-5.5", "openai/gpt-5.6-sol"}, []string{response.Data[0].ID, response.Data[1].ID})
	require.Len(t, response.KeyStatuses, 1)
	require.Equal(t, schemas.OpenAI, response.KeyStatuses[0].Provider)
}

func TestFilterFailsClosedWithoutResolvedTenant(t *testing.T) {
	filter, err := modelaccess.New(policyStore{policy: billing.ModelPolicy{PlanModels: []string{"*"}, PricedModels: []string{"*"}}})
	require.NoError(t, err)
	response := &schemas.BifrostListModelsResponse{Data: []schemas.Model{{ID: "openai/gpt-5.5"}}}

	err = filter.FilterListModels(schemas.NewBifrostContext(context.Background(), schemas.NoDeadline), response)
	require.ErrorIs(t, err, modelaccess.ErrTenantRequired)
}

func TestFilterUsesProviderHintForBareProviderResponses(t *testing.T) {
	filter, err := modelaccess.New(policyStore{policy: billing.ModelPolicy{
		PlanModels:   []string{"openai/gpt-5.5"},
		PricedModels: []string{"openai/gpt-5.5"},
	}})
	require.NoError(t, err)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	require.NoError(t, tenant.SetResolvedTenant(ctx, "tenant-a"))
	response := &schemas.BifrostListModelsResponse{
		Data:        []schemas.Model{{ID: "gpt-5.5"}},
		KeyStatuses: []schemas.KeyStatus{{Provider: schemas.OpenAI, Status: schemas.KeyStatusSuccess}},
	}

	require.NoError(t, filter.FilterListModels(ctx, response))
	require.Len(t, response.Data, 1)
	require.Len(t, response.KeyStatuses, 1)
}
