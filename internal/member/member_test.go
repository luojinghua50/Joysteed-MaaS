package member_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/controlplane"
	"github.com/luojinghua50/Joysteed-MaaS/internal/member"
	"github.com/luojinghua50/Joysteed-MaaS/internal/rbac"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func memberStore(t *testing.T) (*member.Store, *rbac.Store) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&controlplane.Tenant{}, &rbac.Role{}, &rbac.RolePermission{}, &rbac.RoleBinding{}, &member.Member{}))
	now := time.Now().UTC()
	require.NoError(t, db.Create(&controlplane.Tenant{ID: "tenant-a", Slug: "acme", Name: "Acme", Status: controlplane.StatusActive, StatusChangedAt: now, CreatedAt: now, UpdatedAt: now}).Error)
	store := member.NewStore(db)
	require.NoError(t, store.EnsureTenantRoles(context.Background(), "tenant-a"))
	return store, rbac.NewStore(db)
}

func TestMemberAuthenticationRolesAndLastOwner(t *testing.T) {
	store, roles := memberStore(t)
	ctx := context.Background()
	owner, err := store.Create(ctx, member.CreateInput{TenantID: "tenant-a", Email: "OWNER@EXAMPLE.COM", DisplayName: "Owner", Password: "correct-horse-battery", Role: member.RoleOwner})
	require.NoError(t, err)
	require.Equal(t, "owner@example.com", owner.Email)

	authenticated, err := store.Authenticate(ctx, "ACME", "owner@example.com", "correct-horse-battery")
	require.NoError(t, err)
	require.Equal(t, member.RoleOwner, authenticated.Role)
	require.NotNil(t, authenticated.LastLoginAt)
	_, err = store.Authenticate(ctx, "acme", "owner@example.com", "wrong-password")
	require.ErrorIs(t, err, member.ErrCredentials)

	principal := rbac.Principal{Type: rbac.PrincipalTenantUser, ID: owner.ID, TenantID: "tenant-a"}
	require.NoError(t, roles.Authorize(ctx, principal, rbac.PermissionMemberManage))
	require.NoError(t, roles.Authorize(ctx, principal, rbac.PermissionKeyManage))
	require.NoError(t, roles.Authorize(ctx, principal, rbac.PermissionKeyReveal))
	disabled := member.StatusDisabled
	_, err = store.Update(ctx, "tenant-a", owner.ID, member.UpdateInput{Status: &disabled})
	require.ErrorIs(t, err, member.ErrLastOwner)

	second, err := store.Create(ctx, member.CreateInput{TenantID: "tenant-a", Email: "second@example.com", DisplayName: "Second owner", Password: "another-secure-password", Role: member.RoleOwner})
	require.NoError(t, err)
	viewer := member.RoleViewer
	updated, err := store.Update(ctx, "tenant-a", owner.ID, member.UpdateInput{Role: &viewer})
	require.NoError(t, err)
	require.Equal(t, member.RoleViewer, updated.Role)
	require.NoError(t, roles.Authorize(ctx, rbac.Principal{Type: rbac.PrincipalTenantUser, ID: second.ID, TenantID: "tenant-a"}, rbac.PermissionMemberManage))
	require.ErrorIs(t, roles.Authorize(ctx, principal, rbac.PermissionKeyManage), rbac.ErrForbidden)
	require.ErrorIs(t, roles.Authorize(ctx, principal, rbac.PermissionKeyReveal), rbac.ErrForbidden)
}

func TestOnlyOwnerCanRevealTenantKeys(t *testing.T) {
	store, roles := memberStore(t)
	ctx := context.Background()
	for _, role := range []member.RoleName{member.RoleAdmin, member.RoleDeveloper, member.RoleViewer} {
		row, err := store.Create(ctx, member.CreateInput{TenantID: "tenant-a", Email: string(role) + "@example.com", DisplayName: string(role), Password: "long-enough-password", Role: role})
		require.NoError(t, err)
		principal := rbac.Principal{Type: rbac.PrincipalTenantUser, ID: row.ID, TenantID: "tenant-a"}
		require.ErrorIs(t, roles.Authorize(ctx, principal, rbac.PermissionKeyReveal), rbac.ErrForbidden)
	}
}

func TestRequestLogPermissionExcludesViewer(t *testing.T) {
	store, roles := memberStore(t)
	ctx := context.Background()
	for _, role := range []member.RoleName{member.RoleOwner, member.RoleAdmin, member.RoleDeveloper, member.RoleViewer} {
		row, err := store.Create(ctx, member.CreateInput{TenantID: "tenant-a", Email: string(role) + "-logs@example.com", DisplayName: string(role), Password: "long-enough-password", Role: role})
		require.NoError(t, err)
		err = roles.Authorize(ctx, rbac.Principal{Type: rbac.PrincipalTenantUser, ID: row.ID, TenantID: "tenant-a"}, rbac.PermissionRequestLogRead)
		if role == member.RoleViewer {
			require.ErrorIs(t, err, rbac.ErrForbidden)
		} else {
			require.NoError(t, err)
		}
	}
}

func TestMemberRejectsShortPasswordAndUnknownRole(t *testing.T) {
	store, _ := memberStore(t)
	_, err := store.Create(context.Background(), member.CreateInput{TenantID: "tenant-a", Email: "user@example.com", DisplayName: "User", Password: "short", Role: member.RoleAdmin})
	require.ErrorIs(t, err, member.ErrInvalidMember)
	_, err = store.Create(context.Background(), member.CreateInput{TenantID: "tenant-a", Email: "user@example.com", DisplayName: "User", Password: "long-enough-password", Role: "root"})
	require.ErrorIs(t, err, member.ErrInvalidMember)
}

func TestMemberTxRollsBackMemberAndRoleBinding(t *testing.T) {
	store, roles := memberStore(t)
	db := roles.DB()
	sentinel := errors.New("rollback")
	var memberID string
	err := db.Transaction(func(tx *gorm.DB) error {
		row, err := store.CreateTx(tx, member.CreateInput{TenantID: "tenant-a", Email: "rollback@example.com", DisplayName: "Rollback", Password: "rollback-password", Role: member.RoleAdmin})
		require.NoError(t, err)
		memberID = row.ID
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)
	_, err = store.Get(context.Background(), "tenant-a", memberID)
	require.ErrorIs(t, err, member.ErrNotFound)
	var bindings int64
	require.NoError(t, db.Model(&rbac.RoleBinding{}).Where("principal_id = ?", memberID).Count(&bindings).Error)
	require.Zero(t, bindings)
}
