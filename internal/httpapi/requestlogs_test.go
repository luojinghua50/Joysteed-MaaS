package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/luojinghua50/Joysteed-MaaS/internal/controlplane"
	"github.com/luojinghua50/Joysteed-MaaS/internal/member"
	"github.com/luojinghua50/Joysteed-MaaS/internal/rbac"
	tenant "github.com/luojinghua50/Joysteed-MaaS/internal/tenantid"
	"github.com/luojinghua50/Joysteed-MaaS/internal/virtualkey"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func requestLogTestServer(t *testing.T, gatewayClient *http.Client) *Server {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:http_request_logs_"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	server, err := New(db, nil, Config{
		AdminUsername: "admin", AdminPassword: "password", SessionLifetime: time.Hour,
		KeyEncryptionKey:   "0123456789abcdef0123456789abcdef",
		GatewayInternalURL: "http://gateway.test", GatewayInternalToken: "internal-token", GatewayHTTPClient: gatewayClient,
	})
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, db.AutoMigrate(&controlplane.Tenant{}))
	require.NoError(t, server.rbac.Migrate(ctx))
	require.NoError(t, server.members.Migrate(ctx))
	require.NoError(t, server.sessions.Migrate(ctx))
	require.NoError(t, server.keys.Migrate(ctx))
	return server
}

func addRequestLogTenant(t *testing.T, server *Server, id, slug, keyID string, role member.RoleName) string {
	t.Helper()
	ctx := context.Background()
	_, err := server.tenants.Create(ctx, tenantID(id), slug, slug)
	require.NoError(t, err)
	require.NoError(t, server.members.EnsureTenantRoles(ctx, tenantID(id)))
	user, err := server.members.Create(ctx, member.CreateInput{TenantID: tenantID(id), Email: slug + "@example.com", DisplayName: slug, Password: "long-enough-password", Role: role})
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, server.db.Create(&virtualkey.Key{ID: keyID, TenantID: tenantID(id), Name: "test", SecretCiphertext: "ciphertext", SecretFingerprint: "fingerprint", Status: virtualkey.StatusActive, DesiredState: virtualkey.DesiredActive, DesiredGeneration: 1, AppliedGeneration: 1, CreatedAt: now, UpdatedAt: now}).Error)
	access, _, _, err := server.sessions.Create(ctx, rbac.Principal{Type: rbac.PrincipalTenantUser, ID: user.ID, TenantID: tenantID(id)}, time.Hour)
	require.NoError(t, err)
	return access
}

func tenantID(value string) tenant.ID { return tenant.ID(value) }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func gatewayClient(fn roundTripFunc) *http.Client { return &http.Client{Transport: fn} }

func TestPortalRequestLogDetailCannotSubmitAnotherTenantKey(t *testing.T) {
	var received url.Values
	client := gatewayClient(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "internal-token", r.Header.Get("X-MaaS-Internal-Token"))
		received = r.URL.Query()
		return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":"request log not found"}`))}, nil
	})
	server := requestLogTestServer(t, client)
	access := addRequestLogTenant(t, server, "tenant-a", "acme", "vk-a", member.RoleDeveloper)
	_ = addRequestLogTenant(t, server, "tenant-b", "other", "vk-b", member.RoleDeveloper)

	req := httptest.NewRequest(http.MethodGet, "/api/portal/request-logs/request-b?virtual_key_id=vk-b", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)
	require.Equal(t, http.StatusNotFound, recorder.Code)
	require.Equal(t, []string{"vk-a"}, received["virtual_key_id"])
	require.False(t, slices.Contains(received["virtual_key_id"], "vk-b"))
}

func TestPortalRequestLogsRejectViewerBeforeCallingGateway(t *testing.T) {
	calls := 0
	client := gatewayClient(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"logs":[]}`))}, nil
	})
	server := requestLogTestServer(t, client)
	access := addRequestLogTenant(t, server, "tenant-viewer", "viewer", "vk-viewer", member.RoleViewer)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/request-logs", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, req)
	require.Equal(t, http.StatusForbidden, recorder.Code)
	require.Zero(t, calls)
}
