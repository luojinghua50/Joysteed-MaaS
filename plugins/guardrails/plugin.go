// Package guardrails provides an explicit fail-closed content policy plugin.
// It is a policy hook, not a claim of regulatory compliance: deployments still
// need their own moderation providers, retention and incident processes.
package guardrails

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
)

const PluginName = "maas-guardrails"

var ErrBlocked = errors.New("guardrails: content blocked")

type Direction string

const (
	DirectionInput  Direction = "input"
	DirectionOutput Direction = "output"
)

type Checker func(direction Direction, body []byte) error

type Config struct {
	CheckInput  Checker
	CheckOutput Checker
}

type Plugin struct{ input, output Checker }

func New(cfg Config) *Plugin      { return &Plugin{input: cfg.CheckInput, output: cfg.CheckOutput} }
func (p *Plugin) GetName() string { return PluginName }
func (p *Plugin) Cleanup() error  { return nil }

func (p *Plugin) HTTPTransportPreAuthHook(_ *schemas.BifrostContext, _ *schemas.HTTPRequest) (*schemas.HTTPResponse, error) {
	return nil, nil
}

func (p *Plugin) HTTPTransportPreHook(_ *schemas.BifrostContext, req *schemas.HTTPRequest) (*schemas.HTTPResponse, error) {
	if p == nil || p.input == nil || req == nil {
		return nil, nil
	}
	if err := p.input(DirectionInput, req.Body); err != nil {
		return rejectResponse(err), nil
	}
	return nil, nil
}

func (p *Plugin) HTTPTransportPostHook(_ *schemas.BifrostContext, _ *schemas.HTTPRequest, resp *schemas.HTTPResponse) error {
	if p == nil || p.output == nil || resp == nil {
		return nil
	}
	if err := p.output(DirectionOutput, resp.Body); err != nil {
		return err
	}
	return nil
}

func (p *Plugin) HTTPTransportStreamChunkHook(_ *schemas.BifrostContext, _ *schemas.HTTPRequest, chunk *schemas.BifrostStreamChunk) (*schemas.BifrostStreamChunk, error) {
	if p == nil || p.output == nil || chunk == nil {
		return chunk, nil
	}
	body, err := json.Marshal(chunk)
	if err != nil {
		return nil, fmt.Errorf("guardrails: encode stream chunk: %w", err)
	}
	if err := p.output(DirectionOutput, body); err != nil {
		return nil, err
	}
	return chunk, nil
}

func (p *Plugin) PreRequestHook(_ *schemas.BifrostContext, _ *schemas.BifrostRequest) error {
	return nil
}

func (p *Plugin) PreLLMHook(_ *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	if p == nil || p.input == nil || req == nil {
		return req, nil, nil
	}
	body, err := json.Marshal(req)
	if err != nil {
		return req, nil, fmt.Errorf("guardrails: encode request: %w", err)
	}
	if err := p.input(DirectionInput, body); err != nil {
		return req, &schemas.LLMPluginShortCircuit{Error: bifrostError(err)}, nil
	}
	return req, nil, nil
}

func (p *Plugin) PostLLMHook(_ *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	if bifrostErr != nil || p == nil || p.output == nil || resp == nil {
		return resp, bifrostErr, nil
	}
	body, err := json.Marshal(resp)
	if err != nil {
		return nil, bifrostError(err), nil
	}
	if err := p.output(DirectionOutput, body); err != nil {
		return nil, bifrostError(err), nil
	}
	return resp, nil, nil
}

func rejectResponse(err error) *schemas.HTTPResponse {
	body, marshalErr := json.Marshal(map[string]any{"error": map[string]string{"type": "content_policy", "message": err.Error()}})
	if marshalErr != nil {
		body = []byte(`{"error":{"type":"content_policy","message":"content blocked"}}`)
	}
	return &schemas.HTTPResponse{StatusCode: 422, Headers: map[string]string{"Content-Type": "application/json"}, Body: body}
}

func bifrostError(err error) *schemas.BifrostError {
	status := 422
	message := err.Error()
	typ := "content_policy"
	return &schemas.BifrostError{StatusCode: &status, Error: &schemas.ErrorField{Type: &typ, Message: message}, AllowFallbacks: boolPtr(false)}
}
func boolPtr(v bool) *bool { return &v }

// KeywordChecker is a small deterministic baseline useful for local tests and
// as a last-resort policy. Production deployments should inject a real
// moderation service through Config.CheckInput/CheckOutput.
func KeywordChecker(blocked ...string) Checker {
	needles := make([][]byte, 0, len(blocked))
	for _, word := range blocked {
		if strings.TrimSpace(word) != "" {
			needles = append(needles, bytes.ToLower([]byte(word)))
		}
	}
	return func(_ Direction, body []byte) error {
		lower := bytes.ToLower(body)
		for _, needle := range needles {
			if bytes.Contains(lower, needle) {
				return fmt.Errorf("%w: matched blocked content", ErrBlocked)
			}
		}
		return nil
	}
}
