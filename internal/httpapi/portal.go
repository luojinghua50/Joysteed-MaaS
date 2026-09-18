package httpapi

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/audit"
	"github.com/luojinghua50/Joysteed-MaaS/internal/billing"
	"github.com/luojinghua50/Joysteed-MaaS/internal/member"
	"github.com/luojinghua50/Joysteed-MaaS/internal/rbac"
	tenant "github.com/luojinghua50/Joysteed-MaaS/internal/tenantid"
	"gorm.io/gorm"
)

const defaultPlanID = "developer"

type skuWriteRequest struct {
	ID                    string `json:"id"`
	Name                  string `json:"name"`
	PromptMicrosPer1M     *int64 `json:"prompt_micros_per_1m"`
	CompletionMicrosPer1M *int64 `json:"completion_micros_per_1m"`
	PromptMicrosPer1K     *int64 `json:"prompt_micros_per_1k"`
	CompletionMicrosPer1K *int64 `json:"completion_micros_per_1k"`
}

func (in skuWriteRequest) sku() (billing.SKU, error) {
	convertLegacy := func(value *int64) (int64, error) {
		if value == nil {
			return 0, nil
		}
		if *value < 0 || *value > math.MaxInt64/1000 {
			return 0, billing.ErrInvalidUsage
		}
		return *value * 1000, nil
	}
	var prompt int64
	if in.PromptMicrosPer1M != nil {
		prompt = *in.PromptMicrosPer1M
	} else {
		var err error
		prompt, err = convertLegacy(in.PromptMicrosPer1K)
		if err != nil {
			return billing.SKU{}, err
		}
	}
	var completion int64
	if in.CompletionMicrosPer1M != nil {
		completion = *in.CompletionMicrosPer1M
	} else {
		var err error
		completion, err = convertLegacy(in.CompletionMicrosPer1K)
		if err != nil {
			return billing.SKU{}, err
		}
	}
	return billing.SKU{
		ID:                    in.ID,
		Name:                  in.Name,
		PromptMicrosPer1M:     prompt,
		CompletionMicrosPer1M: completion,
	}, nil
}

func (s *Server) seedDefaultPlan(ctx context.Context) error {
	if err := s.billing.UpsertSKU(ctx, billing.SKU{ID: "*", Name: "Default model pricing", PromptMicrosPer1M: 0, CompletionMicrosPer1M: 0}); err != nil {
		return err
	}
	if err := s.billing.EnsurePlan(ctx, billing.Plan{ID: defaultPlanID, Name: "Developer", Currency: "USD", MonthlyMicros: 0, IncludedCreditMicros: 100_000_000, MaxConcurrent: 5, OveragePolicy: "reject"}, []string{"*"}); err != nil {
		return err
	}
	rows, err := s.tenants.List(ctx, 10000)
	if err != nil {
		return err
	}
	for _, row := range rows {
		var count int64
		if err := s.db.WithContext(ctx).Model(&billing.TenantPlan{}).Where("tenant_id = ?", row.ID).Count(&count).Error; err != nil {
			return err
		}
		if count == 0 {
			if err := s.billing.SetTenantPlan(ctx, row.ID, defaultPlanID, time.Now().UTC()); err != nil {
				return err
			}
		}
	}
	return nil
}

func parsePortalKeyPath(path string) (string, string, bool) {
	const prefix = "/api/portal/keys"
	if path == prefix {
		return "", "", true
	}
	if !strings.HasPrefix(path, prefix+"/") {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix+"/"), "/")
	if len(parts) < 1 || len(parts) > 2 {
		return "", "", false
	}
	id, err := url.PathUnescape(parts[0])
	if err != nil || id == "" || strings.Contains(id, "/") {
		return "", "", false
	}
	if len(parts) == 1 {
		return id, "", true
	}
	if parts[1] != "retry" && parts[1] != "reveal" {
		return "", "", false
	}
	return id, parts[1], true
}

func parseAdminPlanPath(path string) (tenant.ID, bool) {
	const prefix = "/api/admin/tenants/"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, "/plan") {
		return "", false
	}
	value := strings.TrimSuffix(strings.TrimPrefix(path, prefix), "/plan")
	if strings.Contains(value, "/") || value == "" {
		return "", false
	}
	decoded, err := url.PathUnescape(value)
	return tenant.ID(decoded), err == nil && decoded != ""
}

func (s *Server) handlePortalLogin(w http.ResponseWriter, r *http.Request) {
	var in struct{ Tenant, Email, Password string }
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid login payload"})
		return
	}
	row, err := s.members.Authenticate(r.Context(), in.Tenant, in.Email, in.Password)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}
	principal := rbac.Principal{Type: rbac.PrincipalTenantUser, ID: row.ID, TenantID: row.TenantID}
	access, csrf, claims, err := s.sessions.Create(r.Context(), principal, s.cfg.SessionLifetime)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "session creation failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"access_token": access, "csrf_token": csrf, "expires_at": claims.ExpiresAt, "principal_type": principal.Type, "role": row.Role})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.session(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	if !claims.VerifyCSRF(r.Header.Get("X-CSRF-Token")) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "csrf validation failed"})
		return
	}
	if err := s.sessions.Revoke(r.Context(), bearerToken(r)); err != nil {
		writeDBError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePortalMe(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.session(r)
	if !ok || claims.Principal.Type != rbac.PrincipalTenantUser {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	row, err := s.members.Get(r.Context(), claims.Principal.TenantID, claims.Principal.ID)
	if err != nil || row.Status != member.StatusActive {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "member is unavailable"})
		return
	}
	permissions, err := s.rbac.Permissions(r.Context(), claims.Principal)
	if err != nil {
		writeDBError(w, err)
		return
	}
	var tenantRow struct {
		ID         tenant.ID
		Slug, Name string
	}
	if err := s.db.WithContext(r.Context()).Table("tenants").Select("id, slug, name").Where("id = ?", row.TenantID).Take(&tenantRow).Error; err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"principal": claims.Principal, "member": row, "tenant": tenantRow, "permissions": permissions, "expires_at": claims.ExpiresAt})
}

func monthWindow(now time.Time) (time.Time, time.Time) {
	now = now.UTC()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return from, from.AddDate(0, 1, 0)
}

func (s *Server) handlePortalUsage(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.authorize(w, r, rbac.PermissionBillingRead, false)
	if !ok {
		return
	}
	from, to := monthWindow(time.Now())
	limit := queryInt(r, "limit", 20)
	if limit > 100 {
		limit = 100
	}
	offset := queryInt(r, "offset", 0)
	if offset < 0 {
		offset = 0
	}
	status := billing.UsageStatus(strings.TrimSpace(r.URL.Query().Get("status")))
	if status != "" && status != billing.UsageStatusUnknown && status != billing.UsageStatusSuccess && status != billing.UsageStatusFailed {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid usage status"})
		return
	}
	page, err := s.billing.ListUsagePage(r.Context(), claims.Principal.TenantID, from, to, billing.UsageListFilter{
		Model: strings.TrimSpace(r.URL.Query().Get("model")), Status: status, Limit: limit, Offset: offset,
	})
	if err != nil {
		writeDBError(w, err)
		return
	}
	summary, err := s.billing.SummarizeUsage(r.Context(), claims.Principal.TenantID, from, to)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"from": from, "to": to, "summary": summary, "events": page.Events,
		"models": page.Models, "limit": limit, "offset": offset, "total_count": page.TotalCount,
	})
}

func (s *Server) handlePortalAudit(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.authorize(w, r, rbac.PermissionAuditRead, false)
	if !ok {
		return
	}
	rows, err := s.audit.ListTenant(r.Context(), claims.Principal.TenantID, 200)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) handlePortalPlan(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.authorize(w, r, rbac.PermissionBillingRead, false)
	if !ok {
		return
	}
	from, to := monthWindow(time.Now())
	details, err := s.billing.CurrentTenantPlan(r.Context(), claims.Principal.TenantID, time.Now(), from, to)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "tenant has no active plan"})
		return
	}
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, details)
}

func (s *Server) handlePlans(w http.ResponseWriter, r *http.Request) {
	permission := rbac.PermissionBillingRead
	if r.Method != http.MethodGet {
		permission = rbac.PermissionBillingManage
	}
	claims, ok := s.authorize(w, r, permission, r.Method != http.MethodGet)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := s.billing.ListPlans(r.Context())
		if err != nil {
			writeDBError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, rows)
	case http.MethodPost, http.MethodPut:
		var in struct {
			Plan   billing.Plan `json:"plan"`
			Models []string     `json:"models"`
		}
		if err := decodeJSON(r, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid plan payload"})
			return
		}
		in.Plan.ID = strings.TrimSpace(in.Plan.ID)
		in.Plan.Name = strings.TrimSpace(in.Plan.Name)
		for i := range in.Models {
			in.Models[i] = strings.TrimSpace(in.Models[i])
		}
		if in.Plan.OveragePolicy != "" && in.Plan.OveragePolicy != "reject" && in.Plan.OveragePolicy != "allow" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "overage_policy must be reject or allow"})
			return
		}
		var before *billing.PlanDetails
		var after billing.PlanDetails
		action := "plan.create"
		status := http.StatusCreated
		err := s.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
			if r.Method == http.MethodPut {
				current, err := planDetailsTx(tx, in.Plan.ID)
				if err != nil {
					return err
				}
				before = &current
				if err := s.billing.UpdatePlanTx(tx, in.Plan, in.Models); err != nil {
					return err
				}
				action = "plan.update"
				status = http.StatusOK
			} else if err := s.billing.CreatePlanTx(tx, in.Plan, in.Models); err != nil {
				return err
			}
			var err error
			after, err = planDetailsTx(tx, in.Plan.ID)
			if err != nil {
				return err
			}
			_, err = s.audit.AppendTx(tx, audit.Input{Principal: claims.Principal, Action: action, ResourceType: "plan", ResourceID: in.Plan.ID, Before: before, After: after, SourceIP: r.RemoteAddr, RequestID: r.Header.Get("X-Request-ID")})
			return err
		})
		if errors.Is(err, billing.ErrInvalidUsage) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "plan not found"})
			return
		}
		if err != nil {
			writeDBError(w, err)
			return
		}
		writeJSON(w, status, after)
	default:
		methodNotAllowed(w)
	}
}

func planDetailsTx(tx *gorm.DB, planID string) (billing.PlanDetails, error) {
	var details billing.PlanDetails
	if err := tx.Where("id = ?", strings.TrimSpace(planID)).Take(&details.Plan).Error; err != nil {
		return details, err
	}
	var rows []billing.PlanModel
	if err := tx.Where("plan_id = ?", details.Plan.ID).Order("model_id ASC").Find(&rows).Error; err != nil {
		return details, err
	}
	details.Models = make([]string, 0, len(rows))
	for _, row := range rows {
		details.Models = append(details.Models, row.ModelID)
	}
	return details, nil
}

func (s *Server) handleTenantPlan(w http.ResponseWriter, r *http.Request, tenantID tenant.ID) {
	switch r.Method {
	case http.MethodGet:
		if _, ok := s.authorize(w, r, rbac.PermissionBillingRead, false); !ok {
			return
		}
		from, to := monthWindow(time.Now())
		details, err := s.billing.CurrentTenantPlan(r.Context(), tenantID, time.Now(), from, to)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "tenant has no active plan"})
			return
		}
		if err != nil {
			writeDBError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, details)
	case http.MethodPut:
		s.handleTenantPlanUpdate(w, r, tenantID)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleTenantPlanUpdate(w http.ResponseWriter, r *http.Request, tenantID tenant.ID) {
	claims, ok := s.authorize(w, r, rbac.PermissionBillingManage, true)
	if !ok {
		return
	}
	var in struct {
		PlanID string `json:"plan_id"`
	}
	if err := decodeJSON(r, &in); err != nil || strings.TrimSpace(in.PlanID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "plan_id is required"})
		return
	}
	var after billing.TenantPlan
	var before *billing.TenantPlan
	err := s.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		var current billing.TenantPlan
		if err := tx.Where("tenant_id = ?", tenantID).Take(&current).Error; err == nil {
			before = &current
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := s.billing.SetTenantPlanTx(tx, tenantID, in.PlanID, time.Now().UTC()); err != nil {
			return err
		}
		if err := tx.Where("tenant_id = ?", tenantID).Take(&after).Error; err != nil {
			return err
		}
		_, err := s.audit.AppendTx(tx, audit.Input{Principal: claims.Principal, TenantID: tenantID, Action: "tenant.plan.set", ResourceType: "tenant_plan", ResourceID: string(tenantID), Before: before, After: after, SourceIP: r.RemoteAddr, RequestID: r.Header.Get("X-Request-ID")})
		return err
	})
	if err != nil {
		writeDBError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSKUs(w http.ResponseWriter, r *http.Request) {
	permission := rbac.PermissionBillingRead
	if r.Method != http.MethodGet {
		permission = rbac.PermissionBillingManage
	}
	claims, ok := s.authorize(w, r, permission, r.Method != http.MethodGet)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := s.billing.ListSKUs(r.Context())
		if err != nil {
			writeDBError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, rows)
	case http.MethodPut, http.MethodPost:
		var request skuWriteRequest
		if err := decodeJSON(r, &request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid SKU payload"})
			return
		}
		in, err := request.sku()
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		var before *billing.SKU
		var after billing.SKU
		err = s.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
			var current billing.SKU
			if err := tx.Where("id = ?", strings.TrimSpace(in.ID)).Take(&current).Error; err == nil {
				before = &current
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if err := s.billing.UpsertSKUTx(tx, in); err != nil {
				return err
			}
			if err := tx.Where("id = ?", strings.TrimSpace(in.ID)).Take(&after).Error; err != nil {
				return err
			}
			_, err := s.audit.AppendTx(tx, audit.Input{Principal: claims.Principal, Action: "sku.upsert", ResourceType: "sku", ResourceID: after.ID, Before: before, After: after, SourceIP: r.RemoteAddr, RequestID: r.Header.Get("X-Request-ID")})
			return err
		})
		if errors.Is(err, billing.ErrInvalidUsage) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err != nil {
			writeDBError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, after)
	default:
		methodNotAllowed(w)
	}
}
