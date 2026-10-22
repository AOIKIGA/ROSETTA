package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"

	"rosetta/internal/llm"
)

type callDelta struct {
	Index    *int        `json:"index"`
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function apiFunction `json:"function"`
}

// Tool arguments may arrive interleaved across many SSE frames. Only a complete
// response is returned to the runtime, so incomplete streams cannot run tools.
func readDeepSeekStream(ctx context.Context, body io.Reader, emit func(string) error) (llm.Response, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var data, text strings.Builder
	calls := make(map[int]*llm.ToolCall)
	finish := ""
	responseBytes := 0
	invalid := func() error { return &PublicError{Code: "provider_invalid_event"} }
	consume := func(payload string) (bool, error) {
		if payload == "[DONE]" {
			if finish == "" {
				return false, &PublicError{Code: "provider_incomplete_response"}
			}
			if finish == "stop" && (len(calls) != 0 || strings.TrimSpace(text.String()) == "") || finish == "tool_calls" && len(calls) == 0 {
				return false, &PublicError{Code: "provider_incomplete_response"}
			}
			ids := make(map[string]bool)
			for i := 0; i < len(calls); i++ {
				call := calls[i]
				if call == nil || strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Name) == "" || ids[call.ID] {
					return false, invalid()
				}
				ids[call.ID] = true
			}
			return true, nil
		}
		var chunk struct {
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Content   string      `json:"content"`
					ToolCalls []callDelta `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return false, invalid()
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			return false, &PublicError{Code: "provider_stream_error"}
		}
		if len(chunk.Choices) > 1 {
			return false, invalid()
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 || finish != "" && (choice.Delta.Content != "" || len(choice.Delta.ToolCalls) != 0 || choice.FinishReason != nil) {
				return false, invalid()
			}
			responseBytes += len(choice.Delta.Content)
			if responseBytes > llm.MaxResponseBytes {
				return false, &PublicError{Code: "provider_response_limit"}
			}
			if choice.Delta.Content != "" {
				text.WriteString(choice.Delta.Content)
				if err := emit(choice.Delta.Content); err != nil {
					return false, err
				}
			}
			for _, delta := range choice.Delta.ToolCalls {
				if delta.Index == nil || *delta.Index < 0 || *delta.Index >= llm.MaxToolCalls || delta.Type != "" && delta.Type != "function" {
					return false, invalid()
				}
				call := calls[*delta.Index]
				if call == nil {
					call = &llm.ToolCall{}
					calls[*delta.Index] = call
				}
				if delta.ID != "" {
					if call.ID != "" && call.ID != delta.ID {
						return false, invalid()
					}
					call.ID = delta.ID
				}
				if delta.Function.Name != "" {
					if call.Name != "" && call.Name != delta.Function.Name {
						return false, invalid()
					}
					call.Name = delta.Function.Name
				}
				responseBytes += len(delta.ID) + len(delta.Function.Name) + len(delta.Function.Arguments)
				if responseBytes > llm.MaxResponseBytes || len(call.ID) > 128 || len(call.Name) > 128 || len(call.Arguments)+len(delta.Function.Arguments) > 16<<10 {
					return false, &PublicError{Code: "provider_response_limit"}
				}
				call.Arguments += delta.Function.Arguments
			}
			if choice.FinishReason != nil {
				switch *choice.FinishReason {
				case "stop", "tool_calls":
					finish = *choice.FinishReason
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
			return llm.Response{}, err
		}
		line := scanner.Text()
		if line == "" {
			if data.Len() == 0 {
				continue
			}
			done, err := consume(strings.TrimSuffix(data.String(), "\n"))
			data.Reset()
			if err != nil {
				return llm.Response{}, err
			}
			if done {
				if err := ctx.Err(); err != nil {
					return llm.Response{}, err
				}
				message := llm.Message{Role: "assistant", Content: text.String()}
				for i := 0; i < len(calls); i++ {
					message.ToolCalls = append(message.ToolCalls, *calls[i])
				}
				return llm.Response{Message: message}, nil
			}
		} else if line == "data" || strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(strings.TrimPrefix(line, "data"), ":")
			value = strings.TrimPrefix(value, " ")
			if data.Len()+len(value)+1 > 1<<20 {
				return llm.Response{}, &PublicError{Code: "provider_event_too_large"}
			}
			data.WriteString(value)
			data.WriteByte('\n')
		}
	}
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	if scanner.Err() != nil {
		return llm.Response{}, &PublicError{Code: "provider_stream_error"}
	}
	return llm.Response{}, &PublicError{Code: "provider_incomplete_response"}
}
