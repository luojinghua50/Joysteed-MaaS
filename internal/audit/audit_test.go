package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/audit"
	"github.com/luojinghua50/Joysteed-MaaS/internal/rbac"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func auditStore(t *testing.T) *audit.Store {
	db, err := gorm.Open(sqlite.Open("file:audit_"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	s := audit.NewStore(db)
	require.NoError(t, s.Migrate(context.Background()))
	return s
}

func TestAuditRedactsAndRollsBack(t *testing.T) {
	s := auditStore(t)
	db := s.DB()
	p := rbac.Principal{Type: rbac.PrincipalTenantUser, ID: "u", TenantID: "t"}
	e, err := s.Append(context.Background(), audit.Input{Principal: p, Action: "key.create", ResourceType: "key", ResourceID: "k", After: map[string]any{"api_key": "secret-value", "nested": []any{map[string]any{"token": "tok"}}, "name": "safe", "monkey": "ordinary"}})
	require.NoError(t, err)
	require.NotContains(t, e.AfterJSON, "secret-value")
	require.NotContains(t, e.AfterJSON, "\"token\":\"tok\"")
	require.Contains(t, e.AfterJSON, "[REDACTED]")
	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(e.AfterJSON), &decoded))
	require.Equal(t, "safe", decoded["name"])
	require.Equal(t, "ordinary", decoded["monkey"])
	err = db.Transaction(func(tx *gorm.DB) error {
		_, err := s.AppendTx(tx, audit.Input{Principal: p, Action: "will.rollback", ResourceType: "x", ResourceID: "x"})
		require.NoError(t, err)
		return errors.New("rollback")
	})
	require.Error(t, err)
	rows, err := s.ListTenant(context.Background(), "t", 100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
}
