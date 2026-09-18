// Package member owns tenant portal identities and their system-role binding.
package member

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/controlplane"
	"github.com/luojinghua50/Joysteed-MaaS/internal/rbac"
	tenant "github.com/luojinghua50/Joysteed-MaaS/internal/tenantid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Status string

const (
	StatusActive   Status = "active"
	StatusDisabled Status = "disabled"
)

type RoleName string

const (
	RoleOwner     RoleName = "owner"
	RoleAdmin     RoleName = "admin"
	RoleDeveloper RoleName = "developer"
	RoleViewer    RoleName = "viewer"
)

var (
	ErrInvalidMember = errors.New("member: invalid member")
	ErrNotFound      = errors.New("member: not found")
	ErrCredentials   = errors.New("member: invalid credentials")
	ErrInvalidRole   = errors.New("member: invalid role")
	ErrLastOwner     = errors.New("member: cannot remove the last active owner")
)

type Member struct {
	ID           string     `gorm:"column:id;primaryKey;type:text" json:"id"`
	TenantID     tenant.ID  `gorm:"column:tenant_id;type:text;not null;uniqueIndex:idx_tenant_member_email,priority:1;index" json:"tenant_id"`
	Email        string     `gorm:"column:email;type:text;not null;uniqueIndex:idx_tenant_member_email,priority:2" json:"email"`
	DisplayName  string     `gorm:"column:display_name;type:text;not null" json:"display_name"`
	PasswordHash string     `gorm:"column:password_hash;type:text;not null" json:"-"`
	Status       Status     `gorm:"column:status;type:text;not null;index" json:"status"`
	LastLoginAt  *time.Time `gorm:"column:last_login_at" json:"last_login_at,omitempty"`
	CreatedAt    time.Time  `gorm:"column:created_at;not null" json:"created_at"`
	UpdatedAt    time.Time  `gorm:"column:updated_at;not null" json:"updated_at"`
}

func (Member) TableName() string { return "tenant_members" }

type View struct {
	Member
	Role RoleName `json:"role"`
}

type CreateInput struct {
	TenantID                     tenant.ID
	Email, DisplayName, Password string
	Role                         RoleName
}

type UpdateInput struct {
	DisplayName *string
	Password    *string
	Status      *Status
	Role        *RoleName
}

type Store struct{ db *gorm.DB }

func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

func (s *Store) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("member: database is required")
	}
	if err := s.db.WithContext(ctx).AutoMigrate(&Member{}); err != nil {
		return fmt.Errorf("member: migrate: %w", err)
	}
	return nil
}

func ValidRole(role RoleName) bool {
	switch role {
	case RoleOwner, RoleAdmin, RoleDeveloper, RoleViewer:
		return true
	default:
		return false
	}
}

func RoleID(tenantID tenant.ID, role RoleName) string {
	return "tenant:" + string(tenantID) + ":" + string(role)
}

func (s *Store) EnsureTenantRoles(ctx context.Context, tenantID tenant.ID) error {
	if s == nil || s.db == nil || tenantID == "" {
		return ErrInvalidMember
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return s.EnsureTenantRolesTx(tx, tenantID)
	})
}

// EnsureTenantRolesTx seeds the fixed tenant roles in a caller-owned
// transaction, primarily for atomic tenant provisioning.
func (s *Store) EnsureTenantRolesTx(tx *gorm.DB, tenantID tenant.ID) error {
	if tx == nil || tenantID == "" {
		return ErrInvalidMember
	}
	definitions := []struct {
		name        RoleName
		description string
		permissions []rbac.Permission
	}{
		{RoleOwner, "Tenant owner", []rbac.Permission{rbac.PermissionTenantRead, rbac.PermissionTenantManage, rbac.PermissionMemberManage, rbac.PermissionKeyManage, rbac.PermissionKeyReveal, rbac.PermissionBudgetManage, rbac.PermissionBillingRead, rbac.PermissionRequestLogRead, rbac.PermissionAuditRead, rbac.PermissionAuditExport}},
		{RoleAdmin, "Tenant administrator", []rbac.Permission{rbac.PermissionTenantRead, rbac.PermissionMemberManage, rbac.PermissionKeyManage, rbac.PermissionBudgetManage, rbac.PermissionBillingRead, rbac.PermissionRequestLogRead, rbac.PermissionAuditRead}},
		{RoleDeveloper, "Tenant developer", []rbac.Permission{rbac.PermissionTenantRead, rbac.PermissionKeyManage, rbac.PermissionBillingRead, rbac.PermissionRequestLogRead, rbac.PermissionAuditRead}},
		{RoleViewer, "Tenant viewer", []rbac.Permission{rbac.PermissionTenantRead, rbac.PermissionBillingRead, rbac.PermissionAuditRead}},
	}
	now := time.Now().UTC()
	for _, definition := range definitions {
		role := rbac.Role{ID: RoleID(tenantID, definition.name), TenantID: &tenantID, Name: string(definition.name), Description: definition.description, CreatedAt: now, UpdatedAt: now}
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoUpdates: clause.AssignmentColumns([]string{"tenant_id", "name", "description", "updated_at"})}).Create(&role).Error; err != nil {
			return fmt.Errorf("member: seed role %s: %w", definition.name, err)
		}
		if err := tx.Where("role_id = ?", role.ID).Delete(&rbac.RolePermission{}).Error; err != nil {
			return err
		}
		for _, permission := range definition.permissions {
			if err := tx.Create(&rbac.RolePermission{RoleID: role.ID, Permission: permission}).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) Create(ctx context.Context, in CreateInput) (*View, error) {
	if s == nil || s.db == nil {
		return nil, ErrInvalidMember
	}
	var out *View
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		out, err = s.CreateTx(tx, in)
		return err
	})
	return out, err
}

// CreateTx creates a member and its role binding inside a caller-owned
// transaction so control-plane handlers can commit the audit event atomically.
func (s *Store) CreateTx(tx *gorm.DB, in CreateInput) (*View, error) {
	if tx == nil || in.TenantID == "" || !ValidRole(in.Role) {
		return nil, ErrInvalidMember
	}
	email, err := normalizeEmail(in.Email)
	if err != nil || strings.TrimSpace(in.DisplayName) == "" {
		return nil, ErrInvalidMember
	}
	hash, err := hashPassword(in.Password)
	if err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	row := Member{ID: id, TenantID: in.TenantID, Email: email, DisplayName: strings.TrimSpace(in.DisplayName), PasswordHash: hash, Status: StatusActive, CreatedAt: now, UpdatedAt: now}
	var count int64
	if err := tx.Model(&controlplane.Tenant{}).Where("id = ?", in.TenantID).Count(&count).Error; err != nil || count != 1 {
		if err != nil {
			return nil, err
		}
		return nil, controlplane.ErrTenantNotFound
	}
	if err := tx.Create(&row).Error; err != nil {
		return nil, fmt.Errorf("member: create: %w", err)
	}
	if err := replaceRole(tx, &row, in.Role); err != nil {
		return nil, err
	}
	return &View{Member: row, Role: in.Role}, nil
}

func (s *Store) List(ctx context.Context, tenantID tenant.ID) ([]View, error) {
	if s == nil || s.db == nil || tenantID == "" {
		return nil, ErrInvalidMember
	}
	var members []Member
	if err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Order("created_at ASC").Find(&members).Error; err != nil {
		return nil, fmt.Errorf("member: list: %w", err)
	}
	views := make([]View, 0, len(members))
	for _, row := range members {
		role, err := memberRole(s.db.WithContext(ctx), row)
		if err != nil {
			return nil, err
		}
		views = append(views, View{Member: row, Role: role})
	}
	return views, nil
}

func (s *Store) Get(ctx context.Context, tenantID tenant.ID, memberID string) (*View, error) {
	if s == nil || s.db == nil {
		return nil, ErrInvalidMember
	}
	return get(s.db.WithContext(ctx), tenantID, memberID, false)
}

// GetTx reads and locks a member inside a caller-owned transaction.
func (s *Store) GetTx(tx *gorm.DB, tenantID tenant.ID, memberID string) (*View, error) {
	if tx == nil {
		return nil, ErrInvalidMember
	}
	return get(tx, tenantID, memberID, true)
}

func get(db *gorm.DB, tenantID tenant.ID, memberID string, lock bool) (*View, error) {
	var row Member
	query := db.Where("tenant_id = ? AND id = ?", tenantID, strings.TrimSpace(memberID))
	if lock {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	err := query.Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	role, err := memberRole(db, row)
	if err != nil {
		return nil, err
	}
	return &View{Member: row, Role: role}, nil
}

func (s *Store) Update(ctx context.Context, tenantID tenant.ID, memberID string, in UpdateInput) (*View, error) {
	if s == nil || s.db == nil {
		return nil, ErrInvalidMember
	}
	var out *View
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		out, err = s.UpdateTx(tx, tenantID, memberID, in)
		return err
	})
	return out, err
}

// UpdateTx updates a member and role binding inside a caller-owned transaction.
func (s *Store) UpdateTx(tx *gorm.DB, tenantID tenant.ID, memberID string, in UpdateInput) (*View, error) {
	if tx == nil || tenantID == "" || strings.TrimSpace(memberID) == "" {
		return nil, ErrInvalidMember
	}
	var row Member
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("tenant_id = ? AND id = ?", tenantID, memberID).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	currentRole, err := memberRole(tx, row)
	if err != nil {
		return nil, err
	}
	nextRole := currentRole
	if in.Role != nil {
		if !ValidRole(*in.Role) {
			return nil, ErrInvalidRole
		}
		nextRole = *in.Role
	}
	nextStatus := row.Status
	if in.Status != nil {
		if *in.Status != StatusActive && *in.Status != StatusDisabled {
			return nil, ErrInvalidMember
		}
		nextStatus = *in.Status
	}
	if currentRole == RoleOwner && (nextRole != RoleOwner || nextStatus != StatusActive) {
		var owners int64
		err := tx.Table(Member{}.TableName()+" m").Joins("JOIN rbac_role_bindings b ON b.principal_id = m.id AND b.principal_type = ?", rbac.PrincipalTenantUser).
			Where("m.tenant_id = ? AND m.status = ? AND b.role_id = ? AND m.id <> ?", tenantID, StatusActive, RoleID(tenantID, RoleOwner), row.ID).Count(&owners).Error
		if err != nil {
			return nil, err
		}
		if owners == 0 {
			return nil, ErrLastOwner
		}
	}
	updates := map[string]any{"updated_at": time.Now().UTC(), "status": nextStatus}
	if in.DisplayName != nil {
		name := strings.TrimSpace(*in.DisplayName)
		if name == "" {
			return nil, ErrInvalidMember
		}
		updates["display_name"] = name
	}
	if in.Password != nil {
		hash, err := hashPassword(*in.Password)
		if err != nil {
			return nil, err
		}
		updates["password_hash"] = hash
	}
	if err := tx.Model(&row).Updates(updates).Error; err != nil {
		return nil, err
	}
	if nextRole != currentRole {
		if err := replaceRole(tx, &row, nextRole); err != nil {
			return nil, err
		}
	}
	if err := tx.Where("id = ?", row.ID).Take(&row).Error; err != nil {
		return nil, err
	}
	return &View{Member: row, Role: nextRole}, nil
}

func (s *Store) Authenticate(ctx context.Context, tenantSlug, email, password string) (*View, error) {
	normalized, err := normalizeEmail(email)
	if err != nil || strings.TrimSpace(tenantSlug) == "" || password == "" {
		return nil, ErrCredentials
	}
	var row Member
	err = s.db.WithContext(ctx).Table(Member{}.TableName()+" m").Select("m.*").
		Joins("JOIN tenants t ON t.id = m.tenant_id").
		Where("LOWER(t.slug) = ? AND m.email = ? AND m.status = ?", strings.ToLower(strings.TrimSpace(tenantSlug)), normalized, StatusActive).Take(&row).Error
	if err != nil || bcrypt.CompareHashAndPassword([]byte(row.PasswordHash), []byte(password)) != nil {
		return nil, ErrCredentials
	}
	now := time.Now().UTC()
	if err := s.db.WithContext(ctx).Model(&Member{}).Where("id = ?", row.ID).Updates(map[string]any{"last_login_at": now, "updated_at": now}).Error; err != nil {
		return nil, err
	}
	row.LastLoginAt = &now
	role, err := memberRole(s.db.WithContext(ctx), row)
	if err != nil {
		return nil, ErrCredentials
	}
	return &View{Member: row, Role: role}, nil
}

func replaceRole(tx *gorm.DB, row *Member, role RoleName) error {
	if tx == nil || row == nil || !ValidRole(role) {
		return ErrInvalidRole
	}
	ids := []string{RoleID(row.TenantID, RoleOwner), RoleID(row.TenantID, RoleAdmin), RoleID(row.TenantID, RoleDeveloper), RoleID(row.TenantID, RoleViewer)}
	if err := tx.Where("principal_type = ? AND principal_id = ? AND tenant_id = ? AND role_id IN ?", rbac.PrincipalTenantUser, row.ID, row.TenantID, ids).Delete(&rbac.RoleBinding{}).Error; err != nil {
		return err
	}
	var count int64
	roleID := RoleID(row.TenantID, role)
	if err := tx.Model(&rbac.Role{}).Where("id = ? AND tenant_id = ?", roleID, row.TenantID).Count(&count).Error; err != nil || count != 1 {
		if err != nil {
			return err
		}
		return ErrInvalidRole
	}
	return tx.Create(&rbac.RoleBinding{RoleID: roleID, PrincipalType: rbac.PrincipalTenantUser, PrincipalID: row.ID, TenantID: &row.TenantID, CreatedAt: time.Now().UTC()}).Error
}

func memberRole(db *gorm.DB, row Member) (RoleName, error) {
	var binding rbac.RoleBinding
	err := db.Where("principal_type = ? AND principal_id = ? AND tenant_id = ? AND role_id IN ?", rbac.PrincipalTenantUser, row.ID, row.TenantID,
		[]string{RoleID(row.TenantID, RoleOwner), RoleID(row.TenantID, RoleAdmin), RoleID(row.TenantID, RoleDeveloper), RoleID(row.TenantID, RoleViewer)}).Take(&binding).Error
	if err != nil {
		return "", fmt.Errorf("member: role lookup: %w", err)
	}
	parts := strings.Split(binding.RoleID, ":")
	if len(parts) == 0 || !ValidRole(RoleName(parts[len(parts)-1])) {
		return "", ErrInvalidRole
	}
	return RoleName(parts[len(parts)-1]), nil
}

func normalizeEmail(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	parsed, err := mail.ParseAddress(value)
	if err != nil || strings.ToLower(parsed.Address) != value || len(value) > 254 {
		return "", ErrInvalidMember
	}
	return value, nil
}

func hashPassword(value string) (string, error) {
	if len(value) < 12 || len(value) > 1024 {
		return "", fmt.Errorf("%w: password must be 12-1024 characters", ErrInvalidMember)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(value), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("member: hash password: %w", err)
	}
	return string(hash), nil
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "mem_" + hex.EncodeToString(b), nil
}
