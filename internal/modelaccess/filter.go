// Package modelaccess applies MaaS plan and pricing policy to Bifrost's live
// model catalog after Bifrost has already enforced API-key permissions.
package modelaccess

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/billing"
	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	tenantid "github.com/luojinghua50/Joysteed-MaaS/internal/tenantid"
	"github.com/maximhq/bifrost/core/schemas"
)

var (
	ErrTenantRequired = errors.New("modelaccess: resolved tenant is required")
	ErrInvalidFilter  = errors.New("modelaccess: policy store and response are required")
)

type PolicyStore interface {
	CurrentModelPolicy(context.Context, tenantid.ID, time.Time) (billing.ModelPolicy, error)
}

type Filter struct {
	store PolicyStore
	now   func() time.Time
}

func New(store PolicyStore) (*Filter, error) {
	if store == nil {
		return nil, ErrInvalidFilter
	}
	return &Filter{store: store, now: time.Now}, nil
}

// FilterListModels keeps only models allowed by both the tenant's active plan
// and the MaaS pricing catalog. Bifrost has already applied live-provider and
// virtual-key restrictions before invoking this filter.
func (f *Filter) FilterListModels(ctx *schemas.BifrostContext, response *schemas.BifrostListModelsResponse) error {
	if f == nil || f.store == nil || response == nil {
		return ErrInvalidFilter
	}
	tenantID := tenant.FromContext(ctx)
	if tenantID == "" {
		return ErrTenantRequired
	}
	policy, err := f.store.CurrentModelPolicy(ctx, tenantID, f.now().UTC())
	if err != nil {
		return err
	}

	providerHint := singleStatusProvider(response.KeyStatuses)
	allowedProviders := make(map[string]struct{})
	models := make([]schemas.Model, 0, len(response.Data))
	for _, model := range response.Data {
		provider, allowed := allowedModel(policy, model, providerHint)
		if !allowed {
			continue
		}
		models = append(models, model)
		if provider != "" {
			allowedProviders[strings.ToLower(provider)] = struct{}{}
		}
	}
	response.Data = models

	statuses := make([]schemas.KeyStatus, 0, len(response.KeyStatuses))
	for _, status := range response.KeyStatuses {
		if _, allowed := allowedProviders[strings.ToLower(string(status.Provider))]; allowed {
			statuses = append(statuses, status)
		}
	}
	response.KeyStatuses = statuses
	resetProviderPagination(response)
	return nil
}

func allowedModel(policy billing.ModelPolicy, model schemas.Model, providerHint string) (string, bool) {
	id := strings.TrimSpace(model.ID)
	if id == "" {
		return "", false
	}
	provider := providerFromQualifiedID(id)
	candidates := []string{id}
	if provider == "" {
		provider = providerHint
		if model.OwnedBy != nil && strings.TrimSpace(*model.OwnedBy) != "" {
			provider = strings.TrimSpace(*model.OwnedBy)
		}
		if provider != "" {
			candidates = append([]string{provider + "/" + id}, candidates...)
		}
	}
	for _, candidate := range candidates {
		if policy.Allows(candidate) {
			return provider, true
		}
	}
	return "", false
}

func providerFromQualifiedID(id string) string {
	if slash := strings.IndexByte(id, '/'); slash > 0 {
		return id[:slash]
	}
	return ""
}

func singleStatusProvider(statuses []schemas.KeyStatus) string {
	provider := ""
	for _, status := range statuses {
		current := strings.TrimSpace(string(status.Provider))
		if current == "" {
			continue
		}
		if provider != "" && !strings.EqualFold(provider, current) {
			return ""
		}
		provider = current
	}
	return provider
}

func resetProviderPagination(response *schemas.BifrostListModelsResponse) {
	if response.FirstID == nil && response.LastID == nil {
		return
	}
	if len(response.Data) == 0 {
		response.FirstID = nil
		response.LastID = nil
		return
	}
	first, last := response.Data[0].ID, response.Data[len(response.Data)-1].ID
	response.FirstID = &first
	response.LastID = &last
}
