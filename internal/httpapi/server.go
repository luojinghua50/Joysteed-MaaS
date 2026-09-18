// Package httpapi exposes the runnable MaaS control-plane API used by the
// Compose deployment. It intentionally keeps the data-plane Bifrost process
// separate: this server owns tenants, sessions, RBAC, billing metadata and
// the administration surface.
package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/audit"
	"github.com/luojinghua50/Joysteed-MaaS/internal/authn"
	"github.com/luojinghua50/Joysteed-MaaS/internal/billing"
	"github.com/luojinghua50/Joysteed-MaaS/internal/configbus"
	"github.com/luojinghua50/Joysteed-MaaS/internal/controlplane"
	"github.com/luojinghua50/Joysteed-MaaS/internal/member"
	"github.com/luojinghua50/Joysteed-MaaS/internal/rbac"
	tenant "github.com/luojinghua50/Joysteed-MaaS/internal/tenantid"
	"github.com/luojinghua50/Joysteed-MaaS/internal/virtualkey"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type Config struct {
	AdminUsername        string
	AdminPassword        string
	SessionLifetime      time.Duration
	CORSOrigin           string
	KeyEncryptionKey     string
	GatewayInternalURL   string
	GatewayInternalToken string
	GatewayHTTPClient    *http.Client
}

type Server struct {
	db                *gorm.DB
	tenants           *controlplane.Store
	rbac              *rbac.Store
	audit             *audit.Store
	billing           *billing.Store
	members           *member.Store
	keys              *virtualkey.Store
	sessions          *authn.Store
	bus               *configbus.Store
	notifier          configbus.Notifier
	redis             redis.UniversalClient
	gatewayHTTPClient *http.Client
	cfg               Config
}

func New(db *gorm.DB, redisClient redis.UniversalClient, cfg Config) (*Server, error) {
	if cfg.AdminUsername == "" {
		cfg.AdminUsername = "admin"
	}
	if cfg.SessionLifetime <= 0 {
		cfg.SessionLifetime = 12 * time.Hour
	}
	if cfg.CORSOrigin == "" {
		cfg.CORSOrigin = "http://localhost:3000"
	}
	if cfg.GatewayHTTPClient == nil {
		cfg.GatewayHTTPClient = http.DefaultClient
	}
	cipher, err := virtualkey.NewCipher(cfg.KeyEncryptionKey)
	if err != nil {
		return nil, err
	}
	keys, err := virtualkey.NewStore(db, cipher)
	if err != nil {
		return nil, err
	}
	var notifier configbus.Notifier
	if redisClient != nil {
		notifier, err = configbus.NewRedisNotifier(redisClient, configbus.DefaultChannel)
		if err != nil {
			return nil, err
		}
	}
	return &Server{
		db: db, tenants: controlplane.NewStore(db), rbac: rbac.NewStore(db),
		audit: audit.NewStore(db), billing: billing.NewStore(db), redis: redisClient,
		members: member.NewStore(db), keys: keys, sessions: authn.NewStore(db),
		bus: configbus.NewStore(db), notifier: notifier, gatewayHTTPClient: cfg.GatewayHTTPClient, cfg: cfg,
	}, nil
}

// Migrate creates only MaaS control-plane tables. Bifrost's own configstore
// migration remains owned by the Bifrost data-plane process.
func (s *Server) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("httpapi: database is required")
	}
	if err := s.tenants.Migrate(ctx); err != nil {
		return err
	}
	if err := s.rbac.Migrate(ctx); err != nil {
		return err
	}
	if err := s.audit.Migrate(ctx); err != nil {
		return err
	}
	if err := s.billing.Migrate(ctx); err != nil {
		return err
	}
	if err := s.members.Migrate(ctx); err != nil {
		return err
	}
	if err := s.sessions.Migrate(ctx); err != nil {
		return err
	}
	if err := s.keys.Migrate(ctx); err != nil {
		return err
	}
	if err := s.seedAdmin(ctx); err != nil {
		return err
	}
	tenants, err := s.tenants.List(ctx, 10000)
	if err != nil {
		return err
	}
	for _, row := range tenants {
		if err := s.members.EnsureTenantRoles(ctx, row.ID); err != nil {
			return err
		}
	}
	return s.seedDefaultPlan(ctx)
}

func (s *Server) seedAdmin(ctx context.Context) error {
	var role rbac.Role
	err := s.db.WithContext(ctx).Where("id = ?", "platform-admin").First(&role).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		permissions := []rbac.Permission{
			rbac.PermissionTenantRead, rbac.PermissionTenantManage,
			rbac.PermissionMemberManage, rbac.PermissionKeyManage,
			rbac.PermissionBudgetManage, rbac.PermissionBillingRead,
			rbac.PermissionBillingManage, rbac.PermissionRequestLogRead, rbac.PermissionAuditRead,
			rbac.PermissionAuditExport, rbac.PermissionPolicyManage,
		}
		if err := s.rbac.CreateRole(ctx, rbac.Role{ID: "platform-admin", Name: "Platform administrator", Description: "Compose bootstrap administrator"}, permissions); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := s.rbac.GrantPermission(ctx, "platform-admin", rbac.PermissionRequestLogRead); err != nil {
		return err
	}
	return s.rbac.BindRole(ctx, "platform-admin", rbac.Principal{Type: rbac.PrincipalPlatformAdmin, ID: s.cfg.AdminUsername})
}

func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveHTTP) }

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	s.setCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path == "/healthz" || r.URL.Path == "/api/health" {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "maas-api"})
		return
	}
	if tenantID, memberID, ok := parseAdminMemberPath(r.URL.Path); ok {
		s.handleMembers(w, r, tenantID, memberID, true)
		return
	}
	if tenantID, keyID, action, ok := parseTenantKeyPath(r.URL.Path); ok {
		s.handleTenantKeys(w, r, tenantID, keyID, action)
		return
	}
	if keyID, action, ok := parsePortalKeyPath(r.URL.Path); ok {
		permission := rbac.PermissionKeyManage
		if action == "reveal" {
			permission = rbac.PermissionKeyReveal
		}
		claims, authorized := s.authorize(w, r, permission, r.Method != http.MethodGet)
		if authorized {
			s.handleTenantKeysAuthorized(w, r, claims, claims.Principal.TenantID, keyID, action)
		}
		return
	}
	if memberID, ok := parsePortalMemberPath(r.URL.Path); ok {
		claims, authorized := s.authorize(w, r, rbac.PermissionMemberManage, r.Method != http.MethodGet)
		if authorized {
			s.handleMembersAuthorized(w, r, claims, claims.Principal.TenantID, memberID, false)
		}
		return
	}
	if requestID, ok := parsePortalRequestLogPath(r.URL.Path); ok {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		s.handlePortalRequestLogDetail(w, r, requestID)
		return
	}
	if usageID, ok := parsePortalUsageDetailPath(r.URL.Path); ok {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		s.handlePortalUsageDetail(w, r, usageID)
		return
	}
	switch r.URL.Path {
	case "/api/auth/login":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		s.handleLogin(w, r)
	case "/api/portal/auth/login":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		s.handlePortalLogin(w, r)
	case "/api/auth/me":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		s.handleMe(w, r)
	case "/api/auth/logout", "/api/portal/auth/logout":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		s.handleLogout(w, r)
	case "/api/portal/me":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		s.handlePortalMe(w, r)
	case "/api/portal/usage":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		s.handlePortalUsage(w, r)
	case "/api/portal/request-logs":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		s.handlePortalRequestLogs(w, r)
	case "/api/portal/audit":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		s.handlePortalAudit(w, r)
	case "/api/portal/plan":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		s.handlePortalPlan(w, r)
	case "/api/admin/summary":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		s.handleSummary(w, r)
	case "/api/admin/tenants":
		s.handleTenants(w, r)
	case "/api/admin/audit":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		s.handleAudit(w, r)
	case "/api/admin/plans":
		s.handlePlans(w, r)
	case "/api/admin/skus":
		s.handleSKUs(w, r)
	case "/api/admin/models":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		s.handleModels(w, r)
	default:
		if tenantID, ok := parseAdminPlanPath(r.URL.Path); ok {
			s.handleTenantPlan(w, r, tenantID)
			return
		}
		if tenantID, ok := parseAdminTenantPath(r.URL.Path); ok {
			s.handleTenantLifecycle(w, r, tenantID)
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func parseAdminTenantPath(path string) (tenant.ID, bool) {
	const prefix = "/api/admin/tenants/"
	if !strings.HasPrefix(path, prefix) {
		return "", false
	}
	value, err := url.PathUnescape(strings.TrimPrefix(path, prefix))
	return tenant.ID(value), err == nil && value != "" && !strings.Contains(value, "/")
}

func (s *Server) handleTenantLifecycle(w http.ResponseWriter, r *http.Request, tenantID tenant.ID) {
	if r.Method == http.MethodGet {
		if _, ok := s.authorize(w, r, rbac.PermissionTenantRead, false); !ok {
			return
		}
		row, err := s.tenants.Get(r.Context(), tenantID)
		if errors.Is(err, controlplane.ErrTenantNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "tenant not found"})
			return
		}
		if err != nil {
			writeDBError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, row)
		return
	}
	if r.Method != http.MethodPatch {
		methodNotAllowed(w)
		return
	}
	claims, ok := s.authorize(w, r, rbac.PermissionTenantManage, true)
	if !ok {
		return
	}
	var in struct {
		Slug   *string              `json:"slug"`
		Name   *string              `json:"name"`
		Status *controlplane.Status `json:"status"`
	}
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid tenant payload"})
		return
	}
	profileMutation := in.Slug != nil || in.Name != nil
	statusMutation := in.Status != nil
	if !profileMutation && !statusMutation {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "slug, name or status is required"})
		return
	}
	if profileMutation && statusMutation {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "profile and status must be updated separately"})
		return
	}
	if statusMutation && (!in.Status.Valid() || *in.Status == controlplane.StatusTrial) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "a valid non-trial status is required"})
		return
	}
	var before, row *controlplane.Tenant
	err := s.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
		var err error
		before, err = s.tenants.GetTx(tx, tenantID)
		if err != nil {
			return err
		}
		action := "tenant.update"
		if statusMutation {
			row, err = s.tenants.TransitionTx(tx, tenantID, *in.Status)
			action = "tenant.status.change"
		} else {
			slug, name := before.Slug, before.Name
			if in.Slug != nil {
				slug = *in.Slug
			}
			if in.Name != nil {
				name = *in.Name
			}
			row, err = s.tenants.UpdateProfileTx(tx, tenantID, slug, name)
		}
		if err != nil {
			return err
		}
		_, err = s.audit.AppendTx(tx, audit.Input{Principal: claims.Principal, TenantID: tenantID, Action: action, ResourceType: "tenant", ResourceID: string(tenantID), Before: before, After: row, SourceIP: r.RemoteAddr, RequestID: r.Header.Get("X-Request-ID")})
		return err
	})
	if errors.Is(err, controlplane.ErrTenantNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "tenant not found"})
		return
	}
	if errors.Is(err, controlplane.ErrTenantSlugRequired) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant login identifier is required"})
		return
	}
	if errors.Is(err, controlplane.ErrTenantSlugConflict) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "tenant login identifier is already in use"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) handleTenantKeys(w http.ResponseWriter, r *http.Request, tenantID tenant.ID, keyID, action string) {
	mutation := r.Method != http.MethodGet
	claims, ok := s.authorize(w, r, rbac.PermissionKeyManage, mutation)
	if !ok {
		return
	}
	s.handleTenantKeysAuthorized(w, r, claims, tenantID, keyID, action)
}

func (s *Server) handleTenantKeysAuthorized(w http.ResponseWriter, r *http.Request, claims rbac.SessionClaims, tenantID tenant.ID, keyID, action string) {
	if tenantID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant id is required"})
		return
	}

	switch {
	case r.Method == http.MethodGet && keyID == "" && action == "":
		rows, err := s.keys.List(r.Context(), tenantID, 200)
		if err != nil {
			writeDBError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, rows)

	case r.Method == http.MethodPost && keyID == "" && action == "":
		var in struct {
			Name string `json:"name"`
		}
		if err := decodeJSON(r, &in); err != nil || strings.TrimSpace(in.Name) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
			return
		}
		var result *virtualkey.Mutation
		err := s.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
			var err error
			result, err = s.keys.CreateTx(tx, tenantID, in.Name)
			if err != nil {
				return err
			}
			_, err = s.audit.AppendTx(tx, audit.Input{
				Principal: claims.Principal, TenantID: tenantID, Action: "virtual_key.create", ResourceType: "virtual_key",
				ResourceID: result.Key.ID, After: result.Key, SourceIP: r.RemoteAddr,
				RequestID: r.Header.Get("X-Request-ID"),
			})
			return err
		})
		if err != nil {
			s.writeKeyError(w, err)
			return
		}
		s.notifyChange(r.Context(), result.Change)
		writeJSON(w, http.StatusCreated, map[string]any{"key": result.Key, "secret": result.Secret})

	case r.Method == http.MethodDelete && keyID != "" && action == "":
		var result *virtualkey.Mutation
		err := s.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
			var err error
			result, err = s.keys.RevokeTx(tx, tenantID, keyID)
			if err != nil {
				return err
			}
			if result.Change == nil {
				return nil
			}
			_, err = s.audit.AppendTx(tx, audit.Input{
				Principal: claims.Principal, TenantID: tenantID, Action: "virtual_key.revoke", ResourceType: "virtual_key",
				ResourceID: result.Key.ID, Before: result.Before, After: result.Key,
				SourceIP: r.RemoteAddr, RequestID: r.Header.Get("X-Request-ID"),
			})
			return err
		})
		if err != nil {
			s.writeKeyError(w, err)
			return
		}
		s.notifyChange(r.Context(), result.Change)
		writeJSON(w, http.StatusAccepted, result.Key)

	case r.Method == http.MethodPost && keyID != "" && action == "retry":
		var result *virtualkey.Mutation
		err := s.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
			var err error
			result, err = s.keys.RetryTx(tx, tenantID, keyID)
			if err != nil {
				return err
			}
			_, err = s.audit.AppendTx(tx, audit.Input{
				Principal: claims.Principal, TenantID: tenantID, Action: "virtual_key.retry", ResourceType: "virtual_key",
				ResourceID: result.Key.ID, Before: result.Before, After: result.Key,
				SourceIP: r.RemoteAddr, RequestID: r.Header.Get("X-Request-ID"),
			})
			return err
		})
		if err != nil {
			s.writeKeyError(w, err)
			return
		}
		s.notifyChange(r.Context(), result.Change)
		writeJSON(w, http.StatusAccepted, result.Key)

	case r.Method == http.MethodPost && keyID != "" && action == "reveal":
		if claims.Principal.Type != rbac.PrincipalTenantUser {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "permission denied"})
			return
		}
		var row *virtualkey.Key
		var secret string
		err := s.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
			var err error
			row, secret, err = s.keys.RevealTx(tx, tenantID, keyID)
			if err != nil {
				return err
			}
			_, err = s.audit.AppendTx(tx, audit.Input{
				Principal: claims.Principal, TenantID: tenantID, Action: "virtual_key.reveal", ResourceType: "virtual_key",
				ResourceID: row.ID, After: map[string]any{"secret_fingerprint": row.SecretFingerprint, "status": row.Status},
				SourceIP: r.RemoteAddr, RequestID: r.Header.Get("X-Request-ID"),
			})
			return err
		})
		if err != nil {
			s.writeKeyError(w, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		writeJSON(w, http.StatusOK, map[string]string{"secret": secret})

	default:
		methodNotAllowed(w)
	}
}

func parseTenantKeyPath(path string) (tenant.ID, string, string, bool) {
	const prefix = "/api/admin/tenants/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	if len(parts) < 2 || len(parts) > 4 || parts[1] != "keys" || parts[0] == "" {
		return "", "", "", false
	}
	tenantValue, err := url.PathUnescape(parts[0])
	if err != nil || strings.Contains(tenantValue, "/") {
		return "", "", "", false
	}
	if len(parts) == 2 {
		return tenant.ID(tenantValue), "", "", true
	}
	keyID, err := url.PathUnescape(parts[2])
	if err != nil || keyID == "" || strings.Contains(keyID, "/") {
		return "", "", "", false
	}
	if len(parts) == 3 {
		return tenant.ID(tenantValue), keyID, "", true
	}
	if parts[3] != "retry" {
		return "", "", "", false
	}
	return tenant.ID(tenantValue), keyID, "retry", true
}

func (s *Server) notifyChange(ctx context.Context, change *configbus.Change) {
	if change == nil || s.notifier == nil {
		return
	}
	if err := s.notifier.Publish(ctx, *change); err != nil {
		log.Printf("config change %d committed; Redis notification failed: %v", change.ID, err)
		return
	}
	if err := s.bus.MarkPublished(ctx, change.ID); err != nil {
		log.Printf("mark config change %d published: %v", change.ID, err)
	}
}

func (s *Server) writeKeyError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, virtualkey.ErrKeyNotFound), errors.Is(err, controlplane.ErrTenantNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "tenant or key not found"})
	case errors.Is(err, virtualkey.ErrInvalidTransition):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, virtualkey.ErrKeyUnavailable):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "key is no longer available"})
	case errors.Is(err, virtualkey.ErrNameRequired), errors.Is(err, virtualkey.ErrTenantRequired):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		writeDBError(w, err)
	}
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var in struct{ Username, Password string }
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid login payload"})
		return
	}
	if !constantTimeEqual(in.Username, s.cfg.AdminUsername) || !constantTimeEqual(in.Password, s.cfg.AdminPassword) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}
	access, csrf, claims, err := s.sessions.Create(r.Context(), rbac.Principal{Type: rbac.PrincipalPlatformAdmin, ID: s.cfg.AdminUsername}, s.cfg.SessionLifetime)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "session creation failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"access_token": access, "csrf_token": csrf, "expires_at": claims.ExpiresAt, "principal_type": claims.Principal.Type})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	claims, ok := s.session(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": claims.SessionID, "principal": claims.Principal, "expires_at": claims.ExpiresAt})
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r, rbac.PermissionTenantRead, false); !ok {
		return
	}
	var tenants, audits, usage int64
	if err := s.db.Model(&controlplane.Tenant{}).Count(&tenants).Error; err != nil {
		writeDBError(w, err)
		return
	}
	if err := s.db.Model(&audit.Entry{}).Count(&audits).Error; err != nil {
		writeDBError(w, err)
		return
	}
	if err := s.db.Model(&billing.UsageEvent{}).Count(&usage).Error; err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenants": tenants, "audit_events": audits, "usage_events": usage, "redis": s.redis != nil})
}

func (s *Server) handleTenants(w http.ResponseWriter, r *http.Request) {
	permission := rbac.PermissionTenantRead
	if r.Method != http.MethodGet {
		permission = rbac.PermissionTenantManage
	}
	if _, ok := s.authorize(w, r, permission, r.Method != http.MethodGet); !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := s.tenants.List(r.Context(), 200)
		if err != nil {
			writeDBError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, rows)
	case http.MethodPost:
		var in struct{ ID, Slug, Name string }
		if err := decodeJSON(r, &in); err != nil || strings.TrimSpace(in.ID) == "" || strings.TrimSpace(in.Slug) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id and slug are required"})
			return
		}
		claims, _ := s.session(r)
		var row *controlplane.Tenant
		err := s.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
			var err error
			row, err = s.tenants.CreateTx(tx, tenant.ID(in.ID), in.Slug, in.Name)
			if err != nil {
				return err
			}
			if err := s.members.EnsureTenantRolesTx(tx, row.ID); err != nil {
				return err
			}
			if err := s.billing.SetTenantPlanTx(tx, row.ID, defaultPlanID, time.Now().UTC()); err != nil {
				return err
			}
			_, err = s.audit.AppendTx(tx, audit.Input{Principal: claims.Principal, TenantID: row.ID, Action: "tenant.create", ResourceType: "tenant", ResourceID: in.ID, After: row, SourceIP: r.RemoteAddr, RequestID: r.Header.Get("X-Request-ID")})
			return err
		})
		if err != nil {
			writeDBError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, row)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r, rbac.PermissionAuditRead, false); !ok {
		return
	}
	rows, err := s.audit.ListPlatform(r.Context(), 200)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request, permission rbac.Permission, mutation bool) (rbac.SessionClaims, bool) {
	claims, ok := s.session(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return rbac.SessionClaims{}, false
	}
	if strings.HasPrefix(r.URL.Path, "/api/admin/") && claims.Principal.Type != rbac.PrincipalPlatformAdmin ||
		strings.HasPrefix(r.URL.Path, "/api/portal/") && claims.Principal.Type != rbac.PrincipalTenantUser {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "permission denied"})
		return rbac.SessionClaims{}, false
	}
	ctx, err := rbac.WithPrincipal(r.Context(), claims.Principal)
	if err != nil || s.rbac.Authorize(ctx, claims.Principal, permission) != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "permission denied"})
		return rbac.SessionClaims{}, false
	}
	if mutation && !claims.VerifyCSRF(r.Header.Get("X-CSRF-Token")) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "csrf validation failed"})
		return rbac.SessionClaims{}, false
	}
	return claims, true
}

func (s *Server) session(r *http.Request) (rbac.SessionClaims, bool) {
	token := bearerToken(r)
	if token == "" {
		return rbac.SessionClaims{}, false
	}
	claims, err := s.sessions.Authenticate(r.Context(), token, time.Now().UTC())
	if err != nil {
		return rbac.SessionClaims{}, false
	}
	if claims.Principal.Type == rbac.PrincipalTenantUser {
		var count int64
		if err := s.db.WithContext(r.Context()).Model(&member.Member{}).Where("id = ? AND tenant_id = ? AND status = ?", claims.Principal.ID, claims.Principal.TenantID, member.StatusActive).Count(&count).Error; err != nil || count != 1 {
			return rbac.SessionClaims{}, false
		}
	}
	return claims, true
}

func (s *Server) setCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", s.cfg.CORSOrigin)
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-CSRF-Token, X-Request-ID")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	w.Header().Set("Vary", "Origin")
}

func decodeJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(dst)
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeDBError(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "database operation failed", "detail": err.Error()})
}
func methodNotAllowed(w http.ResponseWriter) {
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
}
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func bearerToken(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
}
