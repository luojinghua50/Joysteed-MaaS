package requestlog_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/luojinghua50/Joysteed-MaaS/internal/requestlog"
	"github.com/stretchr/testify/require"
)

func TestProjectDetailRedactsNestedSecretsAndTruncates(t *testing.T) {
	entry := requestlog.Source{
		ID: "request-a", VirtualKeyID: "vk-a", Status: "success",
		Request: map[string]any{"parameters": map[string]any{
			"authorization": "Bearer secret-value",
			"nested": map[string]any{
				"api_key": "provider-key", "cookie": "session=value",
				"harmless": strings.Repeat("x", 40<<10),
			},
		}},
	}
	detail := requestlog.ProjectDetail(entry)
	require.True(t, detail.ContentAvailable)
	require.True(t, detail.Redacted)
	require.True(t, detail.Truncated)
	encoded, err := json.Marshal(detail)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "secret-value")
	require.NotContains(t, string(encoded), "provider-key")
	require.NotContains(t, string(encoded), "session=value")
	require.Contains(t, string(encoded), "[REDACTED]")
}

func TestProjectDetailHonorsContentHiddenAndOwnership(t *testing.T) {
	entry := requestlog.Source{ID: "request-a", VirtualKeyID: "vk-a", ContentHidden: true, Request: map[string]any{"parameters": map[string]any{"value": "must-not-appear"}}}
	detail := requestlog.ProjectDetail(entry)
	require.False(t, detail.ContentAvailable)
	require.Nil(t, detail.Request)
	require.Nil(t, detail.Response)
	require.True(t, requestlog.Owns(entry.VirtualKeyID, map[string]struct{}{"vk-a": {}}))
	require.False(t, requestlog.Owns(entry.VirtualKeyID, map[string]struct{}{"vk-b": {}}))
}

func TestProjectDetailCapsObjectBreadth(t *testing.T) {
	parameters := make(map[string]any, 250)
	for index := 0; index < 250; index++ {
		parameters[string(rune(index+1000))] = index
	}
	detail := requestlog.ProjectDetail(requestlog.Source{Request: map[string]any{"parameters": parameters}})
	require.True(t, detail.Truncated)
	request := detail.Request.(map[string]any)
	require.LessOrEqual(t, len(request["parameters"].(map[string]any)), 100)
}
