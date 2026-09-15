// Package rbac implements the control-plane authorization boundary.
//
// Resource authorization in the Bifrost data plane remains owned by
// framework/grant. This package only answers the management-plane question:
// whether a principal may perform an administrative action.
package rbac

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tenant "github.com/luojinghua50/Joysteed-MaaS/internal/tenantid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type PrincipalType string

const (
	PrincipalPlatformAdmin PrincipalType = "platform_admin"
	PrincipalTenantUser    PrincipalType = "tenant_user"
)

var (
	ErrUnauthenticated  = errors.New("rbac: unauthenticated principal")
	ErrForbidden        = errors.New("rbac: permission denied")
	ErrTenantMismatch   = errors.New("rbac: principal tenant mismatch")
	ErrInvalidPrincipal = errors.New("rbac: invalid principal")
)

// Principal is the only authorization identity accepted by this package. A
// tenant user cannot name another tenant in a request and platform admins are
// explicitly distinguished from tenant users.
type Principal struct {
	Type     PrincipalType
	ID       string
	TenantID tenant.ID
}

func (p Principal) Valid() bool {
	if strings.TrimSpace(p.ID) == "" {
		return false
	}
	switch p.Type {
	case PrincipalPlatformAdmin:
		return p.TenantID == ""
	case PrincipalTenantUser:
		return p.TenantID != ""
	default:
		return false
	}
}

type principalKey struct{}

func WithPrincipal(ctx context.Context, p Principal) (context.Context, error) {
	if !p.Valid() {
		return nil, ErrInvalidPrincipal
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, principalKey{}, p), nil
}

func PrincipalFromContext(ctx context.Context) (Principal, bool) {
	if ctx == nil {
		return Principal{}, false
	}
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok && p.Valid()
}

// Permission strings are deliberately hierarchical. A role containing
// "tenant.read" grants that exact permission; "tenant.*" grants all actions
// below the prefix. Wildcard matching is implemented here rather than in SQL so
// the same semantics apply to cached and database-backed checks.
type Permission string

const (
	PermissionTenantRead    Permission = "tenant.read"
	PermissionTenantManage  Permission = "tenant.manage"
	PermissionMemberManage  Permission = "member.manage"
	PermissionKeyManage     Permission = "key.manage"
	PermissionBudgetManage  Permission = "budget.manage"
	PermissionBillingRead   Permission = "billing.read"
	PermissionBillingManage Permission = "billing.manage"
	PermissionAuditRead     Permission = "audit.read"
	PermissionAuditExport   Permission = "audit.export"
	PermissionPolicyManage  Permission = "policy.manage"
)

type Role struct {
	ID          string     `gorm:"column:id;primaryKey;type:text"`
	TenantID    *tenant.ID `gorm:"column:tenant_id;type:text;index"`
	Name        string     `gorm:"column:name;type:text;not null"`
	Description string     `gorm:"column:description;type:text;not null"`
	CreatedAt   time.Time  `gorm:"column:created_at;not null"`
	UpdatedAt   time.Time  `gorm:"column:updated_at;not null"`
}

func (Role) TableName() string { return "rbac_roles" }

type RolePermission struct {
	RoleID     string     `gorm:"column:role_id;primaryKey;type:text"`
	Permission Permission `gorm:"column:permission;primaryKey;type:text"`
}

func (RolePermission) TableName() string { return "rbac_role_permissions" }

type RoleBinding struct {
	RoleID        string        `gorm:"column:role_id;primaryKey;type:text"`
	PrincipalType PrincipalType `gorm:"column:principal_type;primaryKey;type:text"`
	PrincipalID   string        `gorm:"column:principal_id;primaryKey;type:text"`
	TenantID      *tenant.ID    `gorm:"column:tenant_id;type:text;index"`
	CreatedAt     time.Time     `gorm:"column:created_at;not null"`
}

func (RoleBinding) TableName() string { return "rbac_role_bindings" }

type Store struct{ db *gorm.DB }

func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

func (s *Store) DB() *gorm.DB {
	if s == nil {
		return nil
	}
	return s.db
}

func (s *Store) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("rbac: database is nil")
	}
	if err := s.db.WithContext(ctx).AutoMigrate(&Role{}, &RolePermission{}, &RoleBinding{}); err != nil {
		return fmt.Errorf("rbac: migrate: %w", err)
	}
	return nil
}

func (s *Store) CreateRole(ctx context.Context, role Role, permissions []Permission) error {
	if s == nil || s.db == nil {
		return errors.New("rbac: database is nil")
	}
	if strings.TrimSpace(role.ID) == "" || strings.TrimSpace(role.Name) == "" {
		return errors.New("rbac: role id and name are required")
	}
	if role.TenantID != nil && strings.TrimSpace(string(*role.TenantID)) == "" {
		return ErrTenantMismatch
	}
	now := time.Now().UTC()
	role.CreatedAt, role.UpdatedAt = now, now
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&role).Error; err != nil {
			return fmt.Errorf("rbac: create role: %w", err)
		}
		seen := make(map[Permission]struct{}, len(permissions))
		for _, permission := range permissions {
			if strings.TrimSpace(string(permission)) == "" {
				return errors.New("rbac: permission is required")
			}
			if _, ok := seen[permission]; ok {
				continue
			}
			seen[permission] = struct{}{}
			if err := tx.Create(&RolePermission{RoleID: role.ID, Permission: permission}).Error; err != nil {
				return fmt.Errorf("rbac: create role permission: %w", err)
			}
		}
		return nil
	})
}

func (s *Store) BindRole(ctx context.Context, roleID string, principal Principal) error {
	if s == nil || s.db == nil {
		return errors.New("rbac: database is nil")
	}
	if strings.TrimSpace(roleID) == "" || !principal.Valid() {
		return ErrInvalidPrincipal
	}
	var role Role
	if err := s.db.WithContext(ctx).Where("id = ?", roleID).First(&role).Error; err != nil {
		return fmt.Errorf("rbac: role lookup: %w", err)
	}
	if principal.Type == PrincipalTenantUser {
		if role.TenantID == nil || *role.TenantID != principal.TenantID {
			return ErrTenantMismatch
		}
	} else if role.TenantID != nil {
		return ErrTenantMismatch
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&RoleBinding{
		RoleID: roleID, PrincipalType: principal.Type, PrincipalID: principal.ID,
		TenantID: nullableTenant(principal.TenantID), CreatedAt: time.Now().UTC(),
	}).Error
}

func (s *Store) UnbindRole(ctx context.Context, roleID string, principal Principal) error {
	if s == nil || s.db == nil {
		return errors.New("rbac: database is nil")
	}
	if !principal.Valid() {
		return ErrInvalidPrincipal
	}
	q := s.db.WithContext(ctx).Where("role_id = ? AND principal_type = ? AND principal_id = ?", roleID, principal.Type, principal.ID)
	if principal.Type == PrincipalTenantUser {
		q = q.Where("tenant_id = ?", string(principal.TenantID))
	} else {
		q = q.Where("tenant_id IS NULL")
	}
	if err := q.Delete(&RoleBinding{}).Error; err != nil {
		return fmt.Errorf("rbac: unbind role: %w", err)
	}
	return nil
}

func nullableTenant(id tenant.ID) *tenant.ID {
	if id == "" {
		return nil
	}
	return &id
}

func permissionMatches(granted, wanted Permission) bool {
	if granted == wanted {
		return true
	}
	if strings.HasSuffix(string(granted), ".*") {
		prefix := strings.TrimSuffix(string(granted), "*")
		return strings.HasPrefix(string(wanted), prefix)
	}
	return false
}

func (s *Store) Permissions(ctx context.Context, principal Principal) ([]Permission, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("rbac: database is nil")
	}
	if !principal.Valid() {
		return nil, ErrInvalidPrincipal
	}
	var rows []RolePermission
	q := s.db.WithContext(ctx).Table(RolePermission{}.TableName()).
		Joins("JOIN rbac_role_bindings b ON b.role_id = rbac_role_permissions.role_id").
		Joins("JOIN rbac_roles r ON r.id = rbac_role_permissions.role_id").
		Where("b.principal_type = ? AND b.principal_id = ?", principal.Type, principal.ID)
	if principal.Type == PrincipalTenantUser {
		q = q.Where("b.tenant_id = ? AND r.tenant_id = ?", string(principal.TenantID), string(principal.TenantID))
	} else {
		q = q.Where("b.tenant_id IS NULL AND r.tenant_id IS NULL")
	}
	err := q.Distinct("rbac_role_permissions.permission").Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("rbac: list permissions: %w", err)
	}
	out := make([]Permission, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Permission)
	}
	return out, nil
}

func (s *Store) Authorize(ctx context.Context, principal Principal, wanted Permission) error {
	if !principal.Valid() {
		return ErrUnauthenticated
	}
	permissions, err := s.Permissions(ctx, principal)
	if err != nil {
		return err
	}
	for _, granted := range permissions {
		if permissionMatches(granted, wanted) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrForbidden, wanted)
}

func (s *Store) Require(ctx context.Context, wanted Permission) error {
	p, ok := PrincipalFromContext(ctx)
	if !ok {
		return ErrUnauthenticated
	}
	return s.Authorize(ctx, p, wanted)
}
