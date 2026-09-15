package tenant

import (
	"context"
	"errors"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/configstore"
	"github.com/maximhq/bifrost/framework/configstore/tables"
	"gorm.io/gorm"
)

// TenantScopedStore is the application-facing guard around ConfigStore.
//
// The embedded interface preserves the complete upstream surface and its
// compile-time compatibility. The methods below are the tenant-visible paths
// used by the data plane and control-plane handlers; they normalize a tenant
// context into the upstream QueryScope before dispatch. A call that omits both
// a tenant and WithPlatformScope fails before reaching the database.
//
// New methods added upstream are deliberately promoted unchanged. They remain
// unavailable to tenant callers until an explicit wrapper is added, which is a
// reviewable failure mode rather than silently inventing a scope for them.
type TenantScopedStore struct {
	configstore.ConfigStore
}

var _ configstore.ConfigStore = (*TenantScopedStore)(nil)

func NewTenantScopedStore(inner configstore.ConfigStore) *TenantScopedStore {
	return &TenantScopedStore{ConfigStore: inner}
}

func (s *TenantScopedStore) scoped(ctx context.Context) (context.Context, error) {
	if s == nil || s.ConfigStore == nil {
		return nil, errors.New("tenant: config store is nil")
	}
	if IsPlatformScope(ctx) {
		return ctx, nil
	}
	id := FromContext(ctx)
	if id == "" {
		return nil, ErrMissingTenant
	}
	return ScopedContext(ctx, id)
}

func (s *TenantScopedStore) GetVirtualKeys(ctx context.Context) ([]tables.TableVirtualKey, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetVirtualKeys(ctx)
}

func (s *TenantScopedStore) GetVirtualKeysPaginated(ctx context.Context, params configstore.VirtualKeyQueryParams) ([]tables.TableVirtualKey, int64, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, 0, err
	}
	return s.ConfigStore.GetVirtualKeysPaginated(ctx, params)
}

func (s *TenantScopedStore) GetRedactedVirtualKeys(ctx context.Context, ids []string) ([]tables.TableVirtualKey, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetRedactedVirtualKeys(ctx, ids)
}

func (s *TenantScopedStore) GetVirtualKey(ctx context.Context, id string) (*tables.TableVirtualKey, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetVirtualKey(ctx, id)
}

func (s *TenantScopedStore) GetVirtualKeyByValue(ctx context.Context, value string) (*tables.TableVirtualKey, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetVirtualKeyByValue(ctx, value)
}

func (s *TenantScopedStore) GetVirtualKeyQuotaByValue(ctx context.Context, value string) (*tables.TableVirtualKey, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetVirtualKeyQuotaByValue(ctx, value)
}

func (s *TenantScopedStore) GetTeams(ctx context.Context, customerID string) ([]tables.TableTeam, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetTeams(ctx, customerID)
}

func (s *TenantScopedStore) GetTeamsPaginated(ctx context.Context, params configstore.TeamsQueryParams) ([]tables.TableTeam, int64, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, 0, err
	}
	return s.ConfigStore.GetTeamsPaginated(ctx, params)
}

func (s *TenantScopedStore) GetTeam(ctx context.Context, id string) (*tables.TableTeam, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetTeam(ctx, id)
}

func (s *TenantScopedStore) GetCustomers(ctx context.Context) ([]tables.TableCustomer, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetCustomers(ctx)
}

func (s *TenantScopedStore) GetCustomersPaginated(ctx context.Context, params configstore.CustomersQueryParams) ([]tables.TableCustomer, int64, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, 0, err
	}
	return s.ConfigStore.GetCustomersPaginated(ctx, params)
}

func (s *TenantScopedStore) GetCustomer(ctx context.Context, id string) (*tables.TableCustomer, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetCustomer(ctx, id)
}

func (s *TenantScopedStore) GetRateLimits(ctx context.Context) ([]tables.TableRateLimit, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetRateLimits(ctx)
}

func (s *TenantScopedStore) GetBudgets(ctx context.Context) ([]tables.TableBudget, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetBudgets(ctx)
}

func (s *TenantScopedStore) GetRoutingRules(ctx context.Context) ([]tables.TableRoutingRule, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetRoutingRules(ctx)
}

func (s *TenantScopedStore) GetModelConfigs(ctx context.Context) ([]tables.TableModelConfig, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetModelConfigs(ctx)
}

func (s *TenantScopedStore) GetPricingOverrides(ctx context.Context, filters configstore.PricingOverrideFilters) ([]tables.TablePricingOverride, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetPricingOverrides(ctx, filters)
}

func (s *TenantScopedStore) GetModelParameters(ctx context.Context) ([]tables.TableModelParameters, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetModelParameters(ctx)
}

func (s *TenantScopedStore) GetKeysByIDs(ctx context.Context, ids []string) ([]tables.TableKey, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetKeysByIDs(ctx, ids)
}

func (s *TenantScopedStore) GetKeysByProvider(ctx context.Context, provider string) ([]tables.TableKey, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetKeysByProvider(ctx, provider)
}

func (s *TenantScopedStore) GetAllRedactedKeys(ctx context.Context, ids []string) ([]schemas.Key, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetAllRedactedKeys(ctx, ids)
}

func (s *TenantScopedStore) GetVirtualKeyProviderConfigs(ctx context.Context, virtualKeyID string) ([]tables.TableVirtualKeyProviderConfig, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetVirtualKeyProviderConfigs(ctx, virtualKeyID)
}

func (s *TenantScopedStore) GetVirtualKeyMCPConfigs(ctx context.Context, virtualKeyID string) ([]tables.TableVirtualKeyMCPConfig, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetVirtualKeyMCPConfigs(ctx, virtualKeyID)
}

func (s *TenantScopedStore) GetVirtualKeyMCPConfigsByMCPClientID(ctx context.Context, mcpClientID uint) ([]tables.TableVirtualKeyMCPConfig, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetVirtualKeyMCPConfigsByMCPClientID(ctx, mcpClientID)
}

func (s *TenantScopedStore) GetVirtualKeyMCPConfigsByMCPClientIDs(ctx context.Context, ids []uint) ([]tables.TableVirtualKeyMCPConfig, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetVirtualKeyMCPConfigsByMCPClientIDs(ctx, ids)
}

func (s *TenantScopedStore) GetVirtualKeyMCPConfigsByMCPClientStringIDs(ctx context.Context, ids []string) ([]tables.TableVirtualKeyMCPConfig, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetVirtualKeyMCPConfigsByMCPClientStringIDs(ctx, ids)
}

func (s *TenantScopedStore) GetVirtualMCPs(ctx context.Context) ([]tables.TableVirtualMCP, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetVirtualMCPs(ctx)
}

func (s *TenantScopedStore) GetVirtualMCPAssignments(ctx context.Context) (map[string][]uint, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetVirtualMCPAssignments(ctx)
}

func (s *TenantScopedStore) GetVirtualMCPByID(ctx context.Context, id uint) (*tables.TableVirtualMCP, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetVirtualMCPByID(ctx, id)
}

func (s *TenantScopedStore) GetVirtualMCPsPaginated(ctx context.Context, params configstore.VirtualMCPsQueryParams) ([]tables.TableVirtualMCP, int64, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, 0, err
	}
	return s.ConfigStore.GetVirtualMCPsPaginated(ctx, params)
}

func (s *TenantScopedStore) GetTeamByName(ctx context.Context, name, customerID string) (*tables.TableTeam, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetTeamByName(ctx, name, customerID)
}

func (s *TenantScopedStore) GetTeamBySourceID(ctx context.Context, sourceID string) (*tables.TableTeam, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetTeamBySourceID(ctx, sourceID)
}

func (s *TenantScopedStore) GetRateLimit(ctx context.Context, id string, tx ...*gorm.DB) (*tables.TableRateLimit, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetRateLimit(ctx, id, tx...)
}

func (s *TenantScopedStore) GetBudget(ctx context.Context, id string, tx ...*gorm.DB) (*tables.TableBudget, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetBudget(ctx, id, tx...)
}

func (s *TenantScopedStore) GetRoutingRulesByScope(ctx context.Context, scope, scopeID string) ([]tables.TableRoutingRule, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetRoutingRulesByScope(ctx, scope, scopeID)
}

func (s *TenantScopedStore) GetRoutingRule(ctx context.Context, id string) (*tables.TableRoutingRule, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetRoutingRule(ctx, id)
}

func (s *TenantScopedStore) GetRedactedRoutingRules(ctx context.Context, ids []string) ([]tables.TableRoutingRule, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetRedactedRoutingRules(ctx, ids)
}

func (s *TenantScopedStore) GetRoutingRulesPaginated(ctx context.Context, params configstore.RoutingRulesQueryParams) ([]tables.TableRoutingRule, int64, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, 0, err
	}
	return s.ConfigStore.GetRoutingRulesPaginated(ctx, params)
}

func (s *TenantScopedStore) GetModelConfigsByScopeAndScopeIDs(ctx context.Context, scope string, ids []string) ([]tables.TableModelConfig, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetModelConfigsByScopeAndScopeIDs(ctx, scope, ids)
}

func (s *TenantScopedStore) GetProviderGovernanceModelConfigs(ctx context.Context) ([]tables.TableModelConfig, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetProviderGovernanceModelConfigs(ctx)
}

func (s *TenantScopedStore) GetModelConfigsPaginated(ctx context.Context, params configstore.ModelConfigsQueryParams) ([]tables.TableModelConfig, int64, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, 0, err
	}
	return s.ConfigStore.GetModelConfigsPaginated(ctx, params)
}

func (s *TenantScopedStore) GetModelConfig(ctx context.Context, scope string, scopeID *string, model string, provider *string) (*tables.TableModelConfig, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetModelConfig(ctx, scope, scopeID, model, provider)
}

func (s *TenantScopedStore) GetModelConfigByID(ctx context.Context, id string) (*tables.TableModelConfig, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetModelConfigByID(ctx, id)
}

func (s *TenantScopedStore) GetPricingOverridesPaginated(ctx context.Context, params configstore.PricingOverridesQueryParams) ([]tables.TablePricingOverride, int64, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, 0, err
	}
	return s.ConfigStore.GetPricingOverridesPaginated(ctx, params)
}

func (s *TenantScopedStore) GetPricingOverrideByID(ctx context.Context, id string) (*tables.TablePricingOverride, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetPricingOverrideByID(ctx, id)
}

func (s *TenantScopedStore) GetModelParametersByModel(ctx context.Context, model string) (*tables.TableModelParameters, error) {
	ctx, err := s.scoped(ctx)
	if err != nil {
		return nil, err
	}
	return s.ConfigStore.GetModelParametersByModel(ctx, model)
}
