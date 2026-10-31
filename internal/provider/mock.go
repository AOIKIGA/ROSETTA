// Package provider contains model adapters and an offline mock for learning.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"rosetta/internal/llm"
	"rosetta/internal/task"
	"rosetta/internal/tool"
)

type Mock struct {
	Delay time.Duration
}

func (Mock) Name() string { return "mock" }

// /time and /recall exercise tools and supplied history offline. Ordinary messages
// keep the original mock echo behavior. No conversation state lives on Mock.
func (m Mock) Stream(ctx context.Context, req llm.Request, emit func(string) error) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	if len(req.Messages) == 0 {
		return llm.Response{}, &llm.Error{Code: "provider_invalid_request"}
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role == "user" {
		prompt := strings.TrimSpace(last.Content)
		name := ""
		var args any
		switch {
		case prompt == "/time":
			name, args = "get_time", map[string]string{"timezone": "Asia/Shanghai"}
		case prompt == "/ls" || strings.HasPrefix(prompt, "/ls "):
			path := strings.TrimSpace(strings.TrimPrefix(prompt, "/ls"))
			if path == "" {
				path = "."
			}
			name, args = "list_files", map[string]string{"path": path}
		case strings.HasPrefix(prompt, "/read "):
			name, args = "read_files", map[string]any{"paths": []string{strings.TrimSpace(strings.TrimPrefix(prompt, "/read "))}}
		case strings.HasPrefix(prompt, "/delegate "):
			for _, spec := range req.Tools {
				if spec.Name == "subagent" {
					name = "subagent"
				}
			}
			args = map[string]string{"prompt": strings.TrimSpace(strings.TrimPrefix(prompt, "/delegate "))}
		}
		if name != "" {
			encoded, _ := json.Marshal(args)
			return llm.Response{Message: llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "mock_" + name + "_1", Name: name, Arguments: string(encoded)}}}}, nil
		}
	}
	answer := "[模拟模型：未调用 AI] 已收到：" + last.Content
	if last.Role == "user" && strings.TrimSpace(last.Content) == "/recall" {
		answer = "[模拟模型：未调用 AI] 当前上下文没有更早的用户消息。"
		for i := len(req.Messages) - 2; i >= 0; i-- {
			if req.Messages[i].Role == "user" {
				previous := []rune(req.Messages[i].Content)
				preview := string(previous[:min(len(previous), 256)])
				if len(previous) > 256 {
					preview += "…"
				}
				answer = "[模拟模型：未调用 AI] 上一条用户消息：" + preview
				break
			}
		}
	}
	if last.Role == "tool" {
		var result tool.Result
		if err := json.Unmarshal([]byte(last.Content), &result); err != nil {
			return llm.Response{}, &llm.Error{Code: "provider_invalid_request"}
		}
		if result.IsError {
			answer = "[模拟模型：未调用 AI] 工具执行失败：" + result.Code
		} else {
			name := ""
			for _, message := range req.Messages {
				for _, call := range message.ToolCalls {
					if call.ID == last.ToolCallID {
						name = call.Name
					}
				}
			}
			switch name {
			case "get_time":
				var clock struct{ Time, Timezone string }
				if err := json.Unmarshal([]byte(result.Content), &clock); err != nil {
					return llm.Response{}, &llm.Error{Code: "provider_invalid_request"}
				}
				answer = fmt.Sprintf("[模拟模型：未调用 AI；已执行真实本地工具] %s 当前时间：%s", clock.Timezone, clock.Time)
			case "subagent":
				var snapshot task.Snapshot
				if err := json.Unmarshal([]byte(result.Content), &snapshot); err != nil {
					return llm.Response{}, &llm.Error{Code: "provider_invalid_request"}
				}
				answer = "[模拟模型：子 Agent 已完成；未调用 AI] task_id=" + snapshot.ID + "\n" + snapshot.Output
			default:
				answer = "[模拟模型：已执行真实本地文件工具；未调用 AI]\n" + result.Content
			}
			preview := []rune(answer)
			if len(preview) > 600 {
				answer = string(preview[:600]) + "…[mock 展示已截断]"
			}
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
