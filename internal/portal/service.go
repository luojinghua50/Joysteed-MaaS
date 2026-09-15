// Package portal is the server-side boundary for the tenant self-service
// portal. It intentionally contains no UI assumptions; every method derives
// scope from the authenticated Principal and never accepts a tenant id from a
// tenant-user request.
package portal

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/audit"
	"github.com/luojinghua50/Joysteed-MaaS/internal/billing"
	"github.com/luojinghua50/Joysteed-MaaS/internal/rbac"
	tenant "github.com/luojinghua50/Joysteed-MaaS/internal/tenantid"
)

var ErrPortalTenantRequired = errors.New("portal: tenant principal required")

type Service struct {
	RBAC       *rbac.Store
	AuditStore *audit.Store
	Billing    *billing.Store
}

func (s *Service) tenantPrincipal(p rbac.Principal, permission rbac.Permission) (tenant.ID, error) {
	if !p.Valid() || p.Type != rbac.PrincipalTenantUser || p.TenantID == "" {
		return "", ErrPortalTenantRequired
	}
	if s == nil || s.RBAC == nil {
		return "", errors.New("portal: rbac store is required")
	}
	if err := s.RBAC.Authorize(context.Background(), p, permission); err != nil {
		return "", err
	}
	return p.TenantID, nil
}

func (s *Service) Usage(ctx context.Context, p rbac.Principal, from, to time.Time, limit int) ([]billing.UsageEvent, error) {
	tid, err := s.tenantPrincipal(p, rbac.PermissionBillingRead)
	if err != nil {
		return nil, err
	}
	if s.Billing == nil {
		return nil, errors.New("portal: billing store is required")
	}
	return s.Billing.ListUsage(ctx, tid, from, to, limit)
}

func (s *Service) Audit(ctx context.Context, p rbac.Principal, limit int) ([]audit.Entry, error) {
	tid, err := s.tenantPrincipal(p, rbac.PermissionAuditRead)
	if err != nil {
		return nil, err
	}
	if s.AuditStore == nil {
		return nil, errors.New("portal: audit store is required")
	}
	return s.AuditStore.ListTenant(ctx, tid, limit)
}

func (s *Service) ReserveBudget(ctx context.Context, p rbac.Principal, key string, amount int64) (*billing.Reservation, error) {
	tid, err := s.tenantPrincipal(p, rbac.PermissionBudgetManage)
	if err != nil {
		return nil, err
	}
	if s.Billing == nil {
		return nil, errors.New("portal: billing store is required")
	}
	return s.Billing.Reserve(ctx, tid, key, amount)
}

func (s *Service) SetPlan(ctx context.Context, p rbac.Principal, planID string, starts time.Time) error {
	tid, err := s.tenantPrincipal(p, rbac.PermissionBillingManage)
	if err != nil {
		return err
	}
	if strings.TrimSpace(planID) == "" || s.Billing == nil {
		return errors.New("portal: invalid plan request")
	}
	return s.Billing.SetTenantPlan(ctx, tid, planID, starts)
}
