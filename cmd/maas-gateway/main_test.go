package main

import (
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/fasthttp/router"
	bifrostserver "github.com/maximhq/bifrost/transports/bifrost-http/server"
	"github.com/stretchr/testify/require"
	"github.com/valyala/fasthttp"
)

func TestRuntimeKVConfigUsesDedicatedOverrides(t *testing.T) {
	t.Setenv("MAAS_REDIS_ADDR", "shared:6379")
	t.Setenv("MAAS_REDIS_PASSWORD", "shared-password")
	t.Setenv("MAAS_KV_REDIS_ADDR", "cluster:6379")
	t.Setenv("MAAS_KV_REDIS_PASSWORD", "kv-password")
	t.Setenv("MAAS_KV_REDIS_DB", "3")
	t.Setenv("MAAS_KV_REDIS_USE_TLS", "true")
	t.Setenv("MAAS_KV_REDIS_CLUSTER_MODE", "false")
	t.Setenv("MAAS_KV_REDIS_OP_TIMEOUT", "750ms")
	t.Setenv("MAAS_KV_REDIS_KEY_PREFIX", "maas:test:")

	config := runtimeKVConfig()
	require.Equal(t, "cluster:6379", config.Addr.GetValue())
	require.Equal(t, "kv-password", config.Password.GetValue())
	require.Equal(t, 3, config.DB.CoerceInt(0))
	require.True(t, config.UseTLS.CoerceBool(false))
	require.False(t, config.ClusterMode.CoerceBool(true))
	require.Equal(t, 750*time.Millisecond, time.Duration(config.OpTimeout))
	require.Equal(t, "maas:test:", config.KeyPrefix)
}

func TestRuntimeKVConfigFallsBackToSharedRedis(t *testing.T) {
	t.Setenv("MAAS_REDIS_ADDR", "shared:6379")
	t.Setenv("MAAS_REDIS_PASSWORD", "shared-password")
	unsetEnvForTest(t, "MAAS_KV_REDIS_ADDR")
	unsetEnvForTest(t, "MAAS_KV_REDIS_PASSWORD")

	config := runtimeKVConfig()
	require.Equal(t, "shared:6379", config.Addr.GetValue())
	require.Equal(t, "shared-password", config.Password.GetValue())
}

func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	value, existed := os.LookupEnv(key)
	require.NoError(t, os.Unsetenv(key))
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(key, value)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func TestInternalRequestLogsRequireTokenAndVirtualKeyScope(t *testing.T) {
	server := &bifrostserver.BifrostHTTPServer{Router: router.New()}
	registerInternalRequestLogs(server, "internal-token")

	invoke := func(uri, token string) int {
		var ctx fasthttp.RequestCtx
		ctx.Request.Header.SetMethod(http.MethodGet)
		ctx.Request.SetRequestURI(uri)
		if token != "" {
			ctx.Request.Header.Set("X-MaaS-Internal-Token", token)
		}
		server.Router.Handler(&ctx)
		return ctx.Response.StatusCode()
	}

	require.Equal(t, fasthttp.StatusUnauthorized, invoke("/maas/internal/logs", ""))
	require.Equal(t, fasthttp.StatusUnauthorized, invoke("/maas/internal/logs", "wrong-token"))
	require.Equal(t, fasthttp.StatusBadRequest, invoke("/maas/internal/logs", "internal-token"))
	require.Equal(t, fasthttp.StatusBadRequest, invoke("/maas/internal/logs/request-1", "internal-token"))
}
