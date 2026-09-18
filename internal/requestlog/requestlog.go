// Package requestlog defines the narrow, tenant-safe projection of Bifrost
// request logs exposed through the MaaS control plane.
package requestlog

import (
	"encoding/json"
	"strings"
	"time"
)

const (
	maxStringBytes = 32 << 10
	maxArrayItems  = 100
	maxMapItems    = 100
	maxDepth       = 12
	maxTotalBytes  = 512 << 10
	maxTotalNodes  = 2000
)

type ListItem struct {
	ID            string    `json:"id"`
	Timestamp     time.Time `json:"timestamp"`
	Status        string    `json:"status"`
	Provider      string    `json:"provider"`
	Model         string    `json:"model"`
	Object        string    `json:"object"`
	LatencyMS     *float64  `json:"latency_ms,omitempty"`
	TotalTokens   int       `json:"total_tokens"`
	Cost          *float64  `json:"cost,omitempty"`
	Stream        bool      `json:"stream"`
	VirtualKeyID  string    `json:"virtual_key_id"`
	ContentHidden bool      `json:"content_hidden"`
}

type ListResponse struct {
	Logs       []ListItem `json:"logs"`
	Limit      int        `json:"limit"`
	Offset     int        `json:"offset"`
	TotalCount int64      `json:"total_count"`
}

type Detail struct {
	Summary          ListItem `json:"summary"`
	Request          any      `json:"request,omitempty"`
	Response         any      `json:"response,omitempty"`
	Routing          any      `json:"routing,omitempty"`
	Error            any      `json:"error,omitempty"`
	ContentAvailable bool     `json:"content_available"`
	ContentHidden    bool     `json:"content_hidden"`
	Redacted         bool     `json:"redacted"`
	Truncated        bool     `json:"truncated"`
}

// Source is populated by the gateway adapter. Keeping it free of Bifrost
// types prevents the MaaS API binary from compiling the gateway implementation
// merely to decode the shared JSON contract.
type Source struct {
	ID, Status, Provider, Model, Object, VirtualKeyID string
	Timestamp                                         time.Time
	LatencyMS                                         *float64
	TotalTokens                                       int
	Cost                                              *float64
	Stream, ContentHidden                             bool
	Request, Response, Routing, Error                 map[string]any
}

func ProjectList(source Source) ListItem {
	return ListItem{
		ID: source.ID, Timestamp: source.Timestamp, Status: source.Status,
		Provider: source.Provider, Model: source.Model, Object: source.Object,
		LatencyMS: source.LatencyMS, TotalTokens: source.TotalTokens, Cost: source.Cost,
		Stream: source.Stream, VirtualKeyID: source.VirtualKeyID,
		ContentHidden: source.ContentHidden,
	}
}

func ProjectDetail(source Source) Detail {
	detail := Detail{Summary: ProjectList(source), ContentHidden: source.ContentHidden}
	request := compactMap(source.Request)
	response := compactMap(source.Response)
	routing := compactMap(source.Routing)
	errorDetails := compactMap(source.Error)
	detail.ContentAvailable = !source.ContentHidden && (len(request) > 0 || len(response) > 0 || len(errorDetails) > 0)
	budget := &sanitizeBudget{remaining: maxTotalBytes, nodes: maxTotalNodes}
	if !source.ContentHidden {
		if len(request) > 0 {
			detail.Request = budget.clean(request, "", 0)
		}
		if len(response) > 0 {
			detail.Response = budget.clean(response, "", 0)
		}
	}
	if len(routing) > 0 {
		detail.Routing = budget.clean(routing, "", 0)
	}
	if !source.ContentHidden && len(errorDetails) > 0 {
		detail.Error = budget.clean(errorDetails, "", 0)
	}
	detail.Redacted = budget.redacted
	detail.Truncated = budget.truncated
	return detail
}

func Owns(virtualKeyID string, allowed map[string]struct{}) bool {
	_, ok := allowed[virtualKeyID]
	return ok
}

func compactMap(values map[string]any) map[string]any {
	out := make(map[string]any, len(values))
	for key, value := range values {
		data, err := json.Marshal(value)
		if err != nil || string(data) == "null" || string(data) == "[]" || string(data) == "{}" || string(data) == `""` {
			continue
		}
		var normalized any
		if json.Unmarshal(data, &normalized) == nil {
			out[key] = normalized
		}
	}
	return out
}

type sanitizeBudget struct {
	remaining int
	nodes     int
	redacted  bool
	truncated bool
}

func (b *sanitizeBudget) clean(value any, key string, depth int) any {
	if b.nodes <= 0 {
		b.truncated = true
		return "[TRUNCATED]"
	}
	b.nodes--
	if sensitiveKey(key) {
		b.redacted = true
		return "[REDACTED]"
	}
	if depth > maxDepth || b.remaining <= 0 {
		b.truncated = true
		return "[TRUNCATED]"
	}
	switch typed := value.(type) {
	case nil, bool, float64:
		return typed
	case string:
		limit := len(typed)
		if limit > maxStringBytes {
			limit = maxStringBytes
			b.truncated = true
		}
		if limit > b.remaining {
			limit = b.remaining
			b.truncated = true
		}
		b.remaining -= limit
		if limit < len(typed) {
			return typed[:limit] + "...[TRUNCATED]"
		}
		return typed
	case map[string]any:
		limit := len(typed)
		if limit > maxMapItems {
			limit = maxMapItems
			b.truncated = true
		}
		out := make(map[string]any, limit)
		count := 0
		for childKey, child := range typed {
			if count >= limit {
				break
			}
			if len(childKey) > b.remaining {
				b.truncated = true
				break
			}
			b.remaining -= len(childKey)
			out[childKey] = b.clean(child, childKey, depth+1)
			count++
		}
		return out
	case []any:
		limit := len(typed)
		if limit > maxArrayItems {
			limit = maxArrayItems
			b.truncated = true
		}
		out := make([]any, 0, limit)
		for i := 0; i < limit; i++ {
			out = append(out, b.clean(typed[i], key, depth+1))
		}
		return out
	default:
		data, err := json.Marshal(typed)
		if err != nil {
			b.truncated = true
			return "[UNAVAILABLE]"
		}
		var normalized any
		if json.Unmarshal(data, &normalized) != nil {
			b.truncated = true
			return "[UNAVAILABLE]"
		}
		return b.clean(normalized, key, depth)
	}
}

func sensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.NewReplacer("-", "", "_", "", " ", "").Replace(key))
	if normalized == "authorization" || normalized == "cookie" || normalized == "setcookie" ||
		normalized == "apikey" || normalized == "xapikey" || normalized == "accesskey" ||
		normalized == "secret" || normalized == "clientsecret" || normalized == "password" {
		return true
	}
	return strings.HasSuffix(normalized, "token") || strings.HasSuffix(normalized, "secret") || strings.HasSuffix(normalized, "password")
}
