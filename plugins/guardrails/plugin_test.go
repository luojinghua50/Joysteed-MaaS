package guardrails_test

import (
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/plugins/guardrails"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/stretchr/testify/require"
)

func TestGuardrailsBlocksInputAndOutput(t *testing.T) {
	p := guardrails.New(guardrails.Config{CheckInput: guardrails.KeywordChecker("blocked"), CheckOutput: guardrails.KeywordChecker("unsafe")})
	resp, err := p.HTTPTransportPreHook(nil, &schemas.HTTPRequest{Body: []byte(`{"prompt":"blocked"}`)})
	require.NoError(t, err)
	require.Equal(t, 422, resp.StatusCode)
	require.Error(t, p.HTTPTransportPostHook(nil, nil, &schemas.HTTPResponse{Body: []byte(`{"text":"unsafe"}`)}))
	_, sc, err := p.PreLLMHook(nil, &schemas.BifrostRequest{})
	require.NoError(t, err)
	require.Nil(t, sc)
}
