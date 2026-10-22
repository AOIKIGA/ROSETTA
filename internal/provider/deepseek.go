package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"rosetta/internal/llm"
)

const DefaultDeepSeekModel = "deepseek-flash"

// DeepSeek owns only the upstream API details. It does not know about sessions,
// local sockets or the daemon's event protocol.
type DeepSeek struct {
	apiKey, model, endpoint string
	maxTokens               int
	client                  *http.Client
}

// PublicError carries a safe category, never a raw upstream response or key.
type PublicError = llm.Error

func NewDeepSeek(apiKey, model string, maxTokens int) (*DeepSeek, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" || strings.ContainsAny(apiKey, "\r\n") {
		return nil, fmt.Errorf("set DEEPSEEK_API_KEY before starting the daemon")
	}
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("model must not be blank")
	}
	// This is a deliberately small learning-version limit, not an API limit.
	if maxTokens < 1 || maxTokens > 4096 {
		return nil, fmt.Errorf("max-tokens must be between 1 and 4096")
	}
	return &DeepSeek{
		apiKey: apiKey, model: model, maxTokens: maxTokens,
		endpoint: "https://api.deepseek.com/chat/completions",
		client: &http.Client{
			Timeout: 110 * time.Second,
			// Never forward credentials to a redirected endpoint.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (*DeepSeek) Name() string { return "deepseek" }

func (p *DeepSeek) Stream(ctx context.Context, input llm.Request, emit func(string) error) (llm.Response, error) {
	messages := make([]apiMessage, 0, len(input.Messages))
	for _, message := range input.Messages {
		wire := apiMessage{Role: message.Role, Content: message.Content, ToolCallID: message.ToolCallID}
		for _, call := range message.ToolCalls {
			wire.ToolCalls = append(wire.ToolCalls, apiToolCall{ID: call.ID, Type: "function", Function: apiFunction{Name: call.Name, Arguments: call.Arguments}})
		}
		messages = append(messages, wire)
	}
	tools := make([]apiToolSpec, 0, len(input.Tools))
	for _, spec := range input.Tools {
		tools = append(tools, apiToolSpec{Type: "function", Function: apiFunctionSpec{Name: spec.Name, Description: spec.Description, Parameters: spec.Parameters}})
	}
	body, err := json.Marshal(map[string]any{
		"model":    p.model,
		"messages": messages,
		"tools":    tools,
		"stream":   true, "max_tokens": p.maxTokens,
		"thinking": map[string]string{"type": "disabled"},
	})
	if err != nil {
		return llm.Response{}, &PublicError{Code: "provider_request_error"}
	}
	if len(body) > llm.MaxInputBytes {
		return llm.Response{}, &PublicError{Code: "input_limit"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return llm.Response{}, &PublicError{Code: "provider_request_error"}
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return llm.Response{}, ctx.Err()
		}
		return llm.Response{}, &PublicError{Code: "provider_connection_error"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Do not return the body: it can echo credentials or user input.
		return llm.Response{}, &PublicError{Code: fmt.Sprintf("provider_http_%d", resp.StatusCode)}
	}
	return readDeepSeekStream(ctx, resp.Body, emit)
}

// These wire types stay inside the adapter; the runtime uses llm types.
type apiMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content"`
	ToolCalls  []apiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type apiFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type apiToolCall struct {
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function apiFunction `json:"function"`
}

type apiToolSpec struct {
	Type     string          `json:"type"`
	Function apiFunctionSpec `json:"function"`
}

type apiFunctionSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}
