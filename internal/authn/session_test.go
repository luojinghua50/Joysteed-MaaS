package authn_test

import (
	"context"
	"testing"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/authn"
	"github.com/luojinghua50/Joysteed-MaaS/internal/rbac"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestSessionPersistsWithoutPlaintextTokens(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	store := authn.NewStore(db)
	require.NoError(t, store.Migrate(context.Background()))
	principal := rbac.Principal{Type: rbac.PrincipalTenantUser, ID: "member-1", TenantID: "tenant-a"}
	access, csrf, created, err := store.Create(context.Background(), principal, time.Hour)
	require.NoError(t, err)
	require.NotEmpty(t, access)
	require.True(t, created.VerifyCSRF(csrf))

	// A new Store instance models another API replica or a process restart.
	restored, err := authn.NewStore(db).Authenticate(context.Background(), access, time.Now())
	require.NoError(t, err)
	require.Equal(t, principal, restored.Principal)
	require.True(t, restored.VerifyCSRF(csrf))
	var row authn.Session
	require.NoError(t, db.First(&row).Error)
	require.NotEqual(t, access, row.AccessTokenHash)
	require.NotEqual(t, csrf, row.CSRFTokenHash)
	require.NoError(t, store.Revoke(context.Background(), access))
	_, err = store.Authenticate(context.Background(), access, time.Now())
	require.ErrorIs(t, err, authn.ErrInvalidSession)
}

func TestRevokePrincipalTxOnlyRevokesMatchingSessions(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	store := authn.NewStore(db)
	require.NoError(t, store.Migrate(context.Background()))
	target := rbac.Principal{Type: rbac.PrincipalTenantUser, ID: "member-1", TenantID: "tenant-a"}
	other := rbac.Principal{Type: rbac.PrincipalTenantUser, ID: "member-2", TenantID: "tenant-a"}
	targetAccess1, _, _, err := store.Create(context.Background(), target, time.Hour)
	require.NoError(t, err)
	targetAccess2, _, _, err := store.Create(context.Background(), target, time.Hour)
	require.NoError(t, err)
	otherAccess, _, _, err := store.Create(context.Background(), other, time.Hour)
	require.NoError(t, err)

	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return store.RevokePrincipalTx(tx, target)
	}))
	_, err = store.Authenticate(context.Background(), targetAccess1, time.Now())
	require.ErrorIs(t, err, authn.ErrInvalidSession)
	_, err = store.Authenticate(context.Background(), targetAccess2, time.Now())
	require.ErrorIs(t, err, authn.ErrInvalidSession)
	claims, err := store.Authenticate(context.Background(), otherAccess, time.Now())
	require.NoError(t, err)
	require.Equal(t, other, claims.Principal)
}
