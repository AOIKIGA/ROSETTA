package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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
type PublicError struct{ Code string }

func (e *PublicError) Error() string      { return e.Code }
func (e *PublicError) PublicCode() string { return e.Code }

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

func (p *DeepSeek) Stream(ctx context.Context, message string, emit func(string) error) error {
	body, err := json.Marshal(map[string]any{
		"model":    p.model,
		"messages": []map[string]string{{"role": "user", "content": message}},
		"stream":   true, "max_tokens": p.maxTokens,
		"thinking": map[string]string{"type": "disabled"},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return &PublicError{Code: "provider_request_error"}
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &PublicError{Code: "provider_connection_error"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Do not return the body: it can echo credentials or user input.
		return &PublicError{Code: fmt.Sprintf("provider_http_%d", resp.StatusCode)}
	}
	return readDeepSeekStream(ctx, resp.Body, emit)
}

func readDeepSeekStream(ctx context.Context, body io.Reader, emit func(string) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var data strings.Builder
	finished, hasText := false, false
	// SSE can contain comments, blank lines and multiple data lines per event.
	consume := func(payload string) (bool, error) {
		if payload == "[DONE]" {
			if !finished || !hasText {
				return false, &PublicError{Code: "provider_incomplete_response"}
			}
			return true, nil
		}
		var chunk struct {
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return false, &PublicError{Code: "provider_invalid_event"}
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			return false, &PublicError{Code: "provider_stream_error"}
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				if finished {
					return false, &PublicError{Code: "provider_invalid_event"}
				}
				hasText = true
				if err := emit(choice.Delta.Content); err != nil {
					return false, err
				}
			}
			if choice.FinishReason != nil {
				switch *choice.FinishReason {
				case "stop":
					finished = true
				case "length":
					return false, &PublicError{Code: "provider_output_limit"}
				default:
					return false, &PublicError{Code: "provider_unsupported_finish"}
				}
			}
		}
		return false, nil
	}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Text()
		if line == "" {
			if data.Len() == 0 {
				continue
			}
			done, err := consume(strings.TrimSuffix(data.String(), "\n"))
			data.Reset()
			if done || err != nil {
				return err
			}
		} else if line == "data" || strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(strings.TrimPrefix(line, "data"), ":")
			value = strings.TrimPrefix(value, " ")
			if data.Len()+len(value)+1 > 1<<20 {
				return &PublicError{Code: "provider_event_too_large"}
			}
			data.WriteString(value)
			data.WriteByte('\n')
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if scanner.Err() != nil {
		return &PublicError{Code: "provider_stream_error"}
	}
	// EOF alone is not proof of a completed generation.
	return &PublicError{Code: "provider_incomplete_response"}
}
