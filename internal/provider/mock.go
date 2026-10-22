// Package provider contains model adapters and an offline mock for learning.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"rosetta/internal/llm"
	"rosetta/internal/tool"
)

type Mock struct {
	Delay time.Duration
}

func (Mock) Name() string { return "mock" }

// The /time trigger makes the tool loop reproducible offline. Ordinary messages
// keep the original mock echo behavior. No conversation state lives on Mock.
func (m Mock) Stream(ctx context.Context, req llm.Request, emit func(string) error) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	if len(req.Messages) == 0 {
		return llm.Response{}, &llm.Error{Code: "provider_invalid_request"}
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role == "user" && strings.TrimSpace(last.Content) == "/time" {
		return llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "mock_time_1", Name: "get_time", Arguments: `{"timezone":"Asia/Shanghai"}`},
		}}}, nil
	}
	answer := "[模拟模型：未调用 AI] 已收到：" + last.Content
	if last.Role == "tool" {
		var result tool.Result
		if err := json.Unmarshal([]byte(last.Content), &result); err != nil {
			return llm.Response{}, &llm.Error{Code: "provider_invalid_request"}
		}
		if result.IsError {
			answer = "[模拟模型：未调用 AI] 工具执行失败：" + result.Code
		} else {
			var clock struct{ Time, Timezone string }
			if err := json.Unmarshal([]byte(result.Content), &clock); err != nil {
				return llm.Response{}, &llm.Error{Code: "provider_invalid_request"}
			}
			answer = fmt.Sprintf("[模拟模型：未调用 AI；已执行真实本地工具] %s 当前时间：%s", clock.Timezone, clock.Time)
		}
	}
	text := []rune(answer)
	for len(text) > 0 {
		if err := ctx.Err(); err != nil {
			return llm.Response{}, err
		}
		if m.Delay > 0 {
			timer := time.NewTimer(m.Delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return llm.Response{}, ctx.Err()
			case <-timer.C:
			}
		}
		n := min(4, len(text))
		if err := emit(string(text[:n])); err != nil {
			return llm.Response{}, err
		}
		text = text[n:]
	}
	return llm.Response{Message: llm.Message{Role: "assistant", Content: answer}}, nil
}
