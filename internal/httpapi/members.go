package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/luojinghua50/Joysteed-MaaS/internal/audit"
	"github.com/luojinghua50/Joysteed-MaaS/internal/member"
	"github.com/luojinghua50/Joysteed-MaaS/internal/rbac"
	tenant "github.com/luojinghua50/Joysteed-MaaS/internal/tenantid"
	"gorm.io/gorm"
)

var errOwnerMutation = errors.New("tenant users cannot modify owner membership")

func parseAdminMemberPath(path string) (tenant.ID, string, bool) {
	const prefix = "/api/admin/tenants/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	if len(parts) < 2 || len(parts) > 3 || parts[1] != "members" || parts[0] == "" {
		return "", "", false
	}
	tenantValue, err := url.PathUnescape(parts[0])
	if err != nil || strings.Contains(tenantValue, "/") {
		return "", "", false
	}
	if len(parts) == 2 {
		return tenant.ID(tenantValue), "", true
	}
	memberID, err := url.PathUnescape(parts[2])
	if err != nil || memberID == "" || strings.Contains(memberID, "/") {
		return "", "", false
	}
	return tenant.ID(tenantValue), memberID, true
}

func parsePortalMemberPath(path string) (string, bool) {
	const prefix = "/api/portal/members"
	if path == prefix {
		return "", true
	}
	if !strings.HasPrefix(path, prefix+"/") {
		return "", false
	}
	value, err := url.PathUnescape(strings.TrimPrefix(path, prefix+"/"))
	return value, err == nil && value != "" && !strings.Contains(value, "/")
}

func (s *Server) handleMembers(w http.ResponseWriter, r *http.Request, tenantID tenant.ID, memberID string, platform bool) {
	claims, ok := s.authorize(w, r, rbac.PermissionMemberManage, r.Method != http.MethodGet)
	if !ok {
		return
	}
	s.handleMembersAuthorized(w, r, claims, tenantID, memberID, platform)
}

func (s *Server) handleMembersAuthorized(w http.ResponseWriter, r *http.Request, claims rbac.SessionClaims, tenantID tenant.ID, memberID string, platform bool) {
	if tenantID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tenant id is required"})
		return
	}
	switch {
	case r.Method == http.MethodGet && memberID == "":
		rows, err := s.members.List(r.Context(), tenantID)
		if err != nil {
			writeMemberError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, rows)
	case r.Method == http.MethodPost && memberID == "":
		var in struct {
			Email       string          `json:"email"`
			DisplayName string          `json:"display_name"`
			Password    string          `json:"password"`
			Role        member.RoleName `json:"role"`
		}
		if err := decodeJSON(r, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid member payload"})
			return
		}
		if !platform && in.Role == member.RoleOwner {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "tenant users cannot grant owner"})
			return
		}
		var row *member.View
		err := s.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
			var err error
			row, err = s.members.CreateTx(tx, member.CreateInput{TenantID: tenantID, Email: in.Email, DisplayName: in.DisplayName, Password: in.Password, Role: in.Role})
			if err != nil {
				return err
			}
			_, err = s.audit.AppendTx(tx, audit.Input{Principal: claims.Principal, TenantID: tenantID, Action: "member.create", ResourceType: "member", ResourceID: row.ID, After: row, SourceIP: r.RemoteAddr, RequestID: r.Header.Get("X-Request-ID")})
			return err
		})
		if err != nil {
			writeMemberError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, row)
	case (r.Method == http.MethodPatch || r.Method == http.MethodPut) && memberID != "":
		var in struct {
			DisplayName *string          `json:"display_name"`
			Password    *string          `json:"password"`
			Status      *member.Status   `json:"status"`
			Role        *member.RoleName `json:"role"`
		}
		if err := decodeJSON(r, &in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid member payload"})
			return
		}
		var current, row *member.View
		err := s.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
			var err error
			current, err = s.members.GetTx(tx, tenantID, memberID)
			if err != nil {
				return err
			}
			if !platform && (current.Role == member.RoleOwner || in.Role != nil && *in.Role == member.RoleOwner) {
				return errOwnerMutation
			}
			row, err = s.members.UpdateTx(tx, tenantID, memberID, member.UpdateInput{DisplayName: in.DisplayName, Password: in.Password, Status: in.Status, Role: in.Role})
			if err != nil {
				return err
			}
			action := "member.update"
			if in.Password != nil {
				if err := s.sessions.RevokePrincipalTx(tx, rbac.Principal{Type: rbac.PrincipalTenantUser, ID: memberID, TenantID: tenantID}); err != nil {
					return err
				}
				action = "member.password.reset"
			}
			_, err = s.audit.AppendTx(tx, audit.Input{Principal: claims.Principal, TenantID: tenantID, Action: action, ResourceType: "member", ResourceID: row.ID, Before: current, After: row, SourceIP: r.RemoteAddr, RequestID: r.Header.Get("X-Request-ID")})
			return err
		})
		if errors.Is(err, errOwnerMutation) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if err != nil {
			writeMemberError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, row)
	case r.Method == http.MethodDelete && memberID != "":
		disabled := member.StatusDisabled
		var current, row *member.View
		err := s.db.WithContext(r.Context()).Transaction(func(tx *gorm.DB) error {
			var err error
			current, err = s.members.GetTx(tx, tenantID, memberID)
			if err != nil {
				return err
			}
			if !platform && current.Role == member.RoleOwner {
				return errOwnerMutation
			}
			row, err = s.members.UpdateTx(tx, tenantID, memberID, member.UpdateInput{Status: &disabled})
			if err != nil {
				return err
			}
			_, err = s.audit.AppendTx(tx, audit.Input{Principal: claims.Principal, TenantID: tenantID, Action: "member.disable", ResourceType: "member", ResourceID: row.ID, Before: current, After: row, SourceIP: r.RemoteAddr, RequestID: r.Header.Get("X-Request-ID")})
			return err
		})
		if errors.Is(err, errOwnerMutation) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		if err != nil {
			writeMemberError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, row)
	default:
		methodNotAllowed(w)
	}
}

func writeMemberError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, member.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "member not found"})
	case errors.Is(err, member.ErrInvalidMember), errors.Is(err, member.ErrInvalidRole):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, member.ErrLastOwner):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		writeDBError(w, err)
	}
}
