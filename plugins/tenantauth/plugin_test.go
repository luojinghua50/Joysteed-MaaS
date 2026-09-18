package tenantauth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	"github.com/luojinghua50/Joysteed-MaaS/plugins/tenantauth"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

type resolver struct {
	id  tenant.ID
	err error
	got string
}

func (r *resolver) ResolveVirtualKey(_ context.Context, value string) (tenant.ID, error) {
	r.got = value
	return r.id, r.err
}

func req(headers map[string]string) *schemas.HTTPRequest {
	return &schemas.HTTPRequest{Headers: headers}
}

func TestPreAuthResolvesTenantAndPreHookKeepsIdentity(t *testing.T) {
	r := &resolver{id: "tenant-a"}
	p, err := tenantauth.New(r)
	require.NoError(t, err)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	_, err = p.HTTPTransportPreAuthHook(ctx, req(map[string]string{"Authorization": "Bearer sk-bf-secret"}))
	require.NoError(t, err)
	require.Equal(t, "sk-bf-secret", r.got)
	require.Equal(t, tenant.ID("tenant-a"), tenant.FromContext(ctx))

	// Match the real transport boundary: pre-auth user values are copied to
	// fasthttp, then the pre-hook receives a newly constructed BifrostContext.
	// BifrostContext intentionally refuses to read through that recyclable
	// parent, so the plugin must establish identity again in the second phase.
	var fastCtx fasthttp.RequestCtx
	var fastReq fasthttp.Request
	fastCtx.Init(&fastReq, nil, nil)
	for key, value := range ctx.GetUserValues() {
		fastCtx.SetUserValue(key, value)
	}
	preHookCtx := schemas.NewBifrostContext(&fastCtx, schemas.NoDeadline)
	require.Empty(t, tenant.FromContext(preHookCtx))
	_, err = p.HTTPTransportPreHook(preHookCtx, req(map[string]string{"Authorization": "Bearer sk-bf-secret"}))
	require.NoError(t, err)
	require.Equal(t, tenant.ID("tenant-a"), tenant.FromContext(preHookCtx))
	require.Equal(t, "sk-bf-secret", preHookCtx.Value(schemas.BifrostContextKeyVirtualKey))
}

func TestPreAuthRejectsMissingAndAmbiguousCredentials(t *testing.T) {
	r := &resolver{id: "tenant-a"}
	p, err := tenantauth.New(r)
	require.NoError(t, err)
	for _, headers := range []map[string]string{
		nil,
		{"Authorization": "Basic abc"},
		{"Authorization": "Bearer sk-bf-a", "authorization": "Bearer sk-bf-b"},
	} {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		resp, err := p.HTTPTransportPreAuthHook(ctx, req(headers))
		require.NoError(t, err)
		require.Equal(t, 401, resp.StatusCode)
	}
	require.Empty(t, r.got, "resolver must not receive an ambiguous credential")
}

func TestPreAuthMapsResolverFailuresWithoutLeakingDetails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"invalid", tenantauth.ErrInvalidVirtualKey, 401, "invalid_virtual_key"},
		{"unavailable", tenantauth.ErrTenantCannotServe, 403, "tenant_unavailable"},
		{"backend", errors.New("password=secret"), 503, "authentication_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := tenantauth.New(&resolver{err: tc.err})
			require.NoError(t, err)
			ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
			resp, err := p.HTTPTransportPreAuthHook(ctx, req(map[string]string{"x-bf-vk": "sk-bf-secret"}))
			require.NoError(t, err)
			require.Equal(t, tc.status, resp.StatusCode)
			require.Contains(t, string(resp.Body), tc.code)
			require.NotContains(t, string(resp.Body), "secret")
		})
	}
}

func TestPreHookRevalidatesCredential(t *testing.T) {
	r := &resolver{err: tenantauth.ErrInvalidVirtualKey}
	p, err := tenantauth.New(r)
	require.NoError(t, err)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	resp, err := p.HTTPTransportPreHook(ctx, req(map[string]string{"x-bf-vk": "sk-bf-revoked"}))
	require.NoError(t, err)
	require.Equal(t, 401, resp.StatusCode)
	require.Equal(t, "sk-bf-revoked", r.got)
}

func TestPreAuthLeavesSessionAndHealthRoutesToTheirOwnAuth(t *testing.T) {
	p, err := tenantauth.New(&resolver{id: "tenant-a"})
	require.NoError(t, err)
	for _, path := range []string{"/api/providers", "/api", "/oauth2/authorize", "/oauth2", "/health", "/ready", "/metrics", "/"} {
		ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
		resp, err := p.HTTPTransportPreAuthHook(ctx, &schemas.HTTPRequest{Path: path})
		require.NoError(t, err)
		require.Nil(t, resp, path)
		resp, err = p.HTTPTransportPreHook(ctx, &schemas.HTTPRequest{Path: path})
		require.NoError(t, err)
		require.Nil(t, resp, path)
	}
}
