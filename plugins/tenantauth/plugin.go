// Package tenantauth implements the M1 HTTP data-plane authentication hook.
package tenantauth

import (
	"context"
	"errors"
	"strings"

	"github.com/luojinghua50/Joysteed-MaaS/internal/tenant"
	auth "github.com/luojinghua50/Joysteed-MaaS/internal/tenantauth"
	"github.com/maximhq/bifrost/core/schemas"
	upstream "github.com/maximhq/bifrost/plugins/governance"
	"github.com/valyala/fasthttp"
)

type Resolver interface {
	ResolveVirtualKey(context.Context, string) (tenant.ID, error)
}

type Plugin struct {
	resolver Resolver
}

// Re-export resolver outcomes so transport wiring does not need to depend on
// the internal lookup package when it wants to classify an error.
var (
	ErrInvalidVirtualKey = auth.ErrInvalidVirtualKey
	ErrUnattributedKey   = auth.ErrUnattributedKey
	ErrTenantCannotServe = auth.ErrTenantCannotServe
)

type verifiedKey struct{}
type identity struct {
	tenant tenant.ID
	value  string
}

var _ schemas.HTTPTransportPlugin = (*Plugin)(nil)

func New(resolver Resolver) (*Plugin, error) {
	if resolver == nil {
		return nil, errors.New("tenantauth: resolver is required")
	}
	return &Plugin{resolver: resolver}, nil
}

func (*Plugin) GetName() string { return "maas-tenantauth" }
func (*Plugin) Cleanup() error  { return nil }

func (p *Plugin) HTTPTransportPreAuthHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest) (*schemas.HTTPResponse, error) {
	// Dashboard, health and metrics routes have their own session/auth boundary;
	// a data-plane VK must not be required before those middlewares run.
	if req != nil && nonDataPlanePath(req.Path) {
		return nil, nil
	}
	value, valid := credential(req)
	if ctx == nil || !valid {
		return denied(401, "invalid_virtual_key"), nil
	}
	id, err := p.resolver.ResolveVirtualKey(ctx, value)
	switch {
	case errors.Is(err, auth.ErrInvalidVirtualKey):
		return denied(401, "invalid_virtual_key"), nil
	case errors.Is(err, auth.ErrTenantCannotServe), errors.Is(err, auth.ErrUnattributedKey):
		return denied(403, "tenant_unavailable"), nil
	case err != nil:
		ctx.Log(schemas.LogLevelError, "tenant authentication backend unavailable")
		return denied(503, "authentication_unavailable"), nil
	}
	if err := tenant.SetResolvedTenant(ctx, id); err != nil {
		return denied(403, "tenant_identity_conflict"), nil
	}
	ctx.SetValue(verifiedKey{}, identity{tenant: id, value: value})
	// Governance remains authoritative for provider/model/resource permissions.
	// Set only its public credential key, never its reserved identity keys.
	ctx.SetValue(schemas.BifrostContextKeyVirtualKey, value)
	return nil, nil
}

func nonDataPlanePath(path string) bool {
	return strings.HasPrefix(path, "/api/") || path == "/api" ||
		strings.HasPrefix(path, "/oauth2/") || path == "/oauth2" ||
		path == "/health" || path == "/ready" || path == "/metrics" || path == "/"
}

func (*Plugin) HTTPTransportPreHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest) (*schemas.HTTPResponse, error) {
	if req != nil && nonDataPlanePath(req.Path) {
		return nil, nil
	}
	if ctx == nil {
		return denied(401, "invalid_virtual_key"), nil
	}
	verified, ok := ctx.Value(verifiedKey{}).(identity)
	value, valid := credential(req)
	// Catch credential/identity rewrites between the pre-auth and pre phases.
	if !ok || !valid || value != verified.value || tenant.FromContext(ctx) != verified.tenant ||
		ctx.Value(schemas.BifrostContextKeyVirtualKey) != verified.value {
		return denied(401, "invalid_virtual_key"), nil
	}
	return nil, nil
}

func (*Plugin) HTTPTransportPostHook(_ *schemas.BifrostContext, _ *schemas.HTTPRequest, _ *schemas.HTTPResponse) error {
	return nil
}

func (*Plugin) HTTPTransportStreamChunkHook(_ *schemas.BifrostContext, _ *schemas.HTTPRequest, chunk *schemas.BifrostStreamChunk) (*schemas.BifrostStreamChunk, error) {
	return chunk, nil
}

func credential(req *schemas.HTTPRequest) (string, bool) {
	if req == nil {
		return "", false
	}
	// Delegate SDK-specific credential precedence to the existing governance
	// parser. Reject ambiguous case variants instead of relying on map order.
	var request fasthttp.RequestCtx
	for _, header := range []string{"x-bf-vk", "authorization", "x-api-key", "x-goog-api-key", "api-key"} {
		var value string
		seen := false
		for key, candidate := range req.Headers {
			if strings.EqualFold(key, header) {
				if seen && candidate != value {
					return "", false
				}
				value, seen = candidate, true
			}
		}
		if seen {
			request.Request.Header.Set(header, value)
		}
	}
	value := upstream.ParseVirtualKeyFromFastHTTPRequest(&request)
	if value == nil {
		return "", false
	}
	return *value, true
}

func denied(status int, code string) *schemas.HTTPResponse {
	return &schemas.HTTPResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "application/json", "Cache-Control": "no-store"},
		Body:       []byte(`{"error":{"type":"` + code + `","message":"Tenant authentication failed"}}`),
	}
}
