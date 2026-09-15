// Package httpapi exposes the runnable MaaS control-plane API used by the
// Compose deployment. It intentionally keeps the data-plane Bifrost process
// separate: this server owns tenants, sessions, RBAC, billing metadata and
// the administration surface.
package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/audit"
	"github.com/luojinghua50/Joysteed-MaaS/internal/billing"
	"github.com/luojinghua50/Joysteed-MaaS/internal/controlplane"
	"github.com/luojinghua50/Joysteed-MaaS/internal/rbac"
	tenant "github.com/luojinghua50/Joysteed-MaaS/internal/tenantid"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type Config struct {
	AdminUsername   string
	AdminPassword   string
	SessionLifetime time.Duration
	CORSOrigin      string
}

type Server struct {
	db      *gorm.DB
	tenants *controlplane.Store
	rbac    *rbac.Store
	audit   *audit.Store
	billing *billing.Store
	redis   redis.UniversalClient
	cfg     Config

	mu       sync.RWMutex
	sessions map[string]rbac.SessionClaims
}

func New(db *gorm.DB, redisClient redis.UniversalClient, cfg Config) *Server {
	if cfg.AdminUsername == "" {
		cfg.AdminUsername = "admin"
	}
	if cfg.SessionLifetime <= 0 {
		cfg.SessionLifetime = 12 * time.Hour
	}
	if cfg.CORSOrigin == "" {
		cfg.CORSOrigin = "http://localhost:3000"
	}
	return &Server{
		db: db, tenants: controlplane.NewStore(db), rbac: rbac.NewStore(db),
		audit: audit.NewStore(db), billing: billing.NewStore(db), redis: redisClient,
		cfg: cfg, sessions: make(map[string]rbac.SessionClaims),
	}
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
	return s.seedAdmin(ctx)
}

func (s *Server) seedAdmin(ctx context.Context) error {
	var role rbac.Role
	err := s.db.WithContext(ctx).Where("id = ?", "platform-admin").First(&role).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		permissions := []rbac.Permission{
			rbac.PermissionTenantRead, rbac.PermissionTenantManage,
			rbac.PermissionMemberManage, rbac.PermissionKeyManage,
			rbac.PermissionBudgetManage, rbac.PermissionBillingRead,
			rbac.PermissionBillingManage, rbac.PermissionAuditRead,
			rbac.PermissionAuditExport, rbac.PermissionPolicyManage,
		}
		if err := s.rbac.CreateRole(ctx, rbac.Role{ID: "platform-admin", Name: "Platform administrator", Description: "Compose bootstrap administrator"}, permissions); err != nil {
			return err
		}
	} else if err != nil {
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
	switch r.URL.Path {
	case "/api/auth/login":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		s.handleLogin(w, r)
	case "/api/auth/me":
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		s.handleMe(w, r)
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
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
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
	claims, csrf, err := rbac.NewSessionClaims(randomID(), rbac.Principal{Type: rbac.PrincipalPlatformAdmin, ID: s.cfg.AdminUsername}, s.cfg.SessionLifetime)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "session creation failed"})
		return
	}
	access, err := randomToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "session creation failed"})
		return
	}
	s.mu.Lock()
	s.sessions[access] = claims
	s.mu.Unlock()
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
			_, err = s.audit.AppendTx(tx, audit.Input{Principal: claims.Principal, Action: "tenant.create", ResourceType: "tenant", ResourceID: in.ID, After: row, SourceIP: r.RemoteAddr, RequestID: r.Header.Get("X-Request-ID")})
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
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(auth, "Bearer ") {
		return rbac.SessionClaims{}, false
	}
	token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	s.mu.RLock()
	claims, ok := s.sessions[token]
	s.mu.RUnlock()
	if !ok || !claims.Valid(time.Now().UTC()) {
		return rbac.SessionClaims{}, false
	}
	return claims, true
}

func (s *Server) setCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", s.cfg.CORSOrigin)
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-CSRF-Token, X-Request-ID")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
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
func randomID() string { token, _ := randomToken(); return token }
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
