package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"rosetta/internal/agent"
	"rosetta/internal/client"
	"rosetta/internal/daemon"
	"rosetta/internal/identity"
	"rosetta/internal/llm"
	"rosetta/internal/protocol"
	"rosetta/internal/provider"
	"rosetta/internal/state"
	"rosetta/internal/tool"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		if ctx.Err() != nil {
			fmt.Fprintln(os.Stderr, "已取消。")
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out, errOut io.Writer) (runErr error) {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(out, `ROSETTA — 具名 Agent / 后台 Daemon / ReAct 工具循环

用法（省略 agent 时使用 personal）：
  rosetta start  [agent] [--state-dir DIR] [--provider mock|deepseek] [--model MODEL] [--max-tokens 512]
  rosetta serve  [agent] [同 start 参数]   前台运行，Ctrl+C 停止
  rosetta status [agent] [--state-dir DIR]
  rosetta stop   [agent] [--state-dir DIR]
  rosetta chat   [agent] [--state-dir DIR] [--user defaultuser] [--session main] [--json] -m "你好"

历史查询：
  rosetta sessions list [agent] [--user defaultuser] [--limit 20] [--after CURSOR] [--json]
  rosetta sessions show [agent] [--user defaultuser] [--session main] [--limit 20] [--after CURSOR] [--json]

Agent 名称放在选项之前，允许 1–32 位 ASCII 字母/数字/_/-。
默认根目录 ~/.rosetta；每个 Agent 存在 agents/<name>/ 下。
agent.json 保存非敏感配置；显式启动参数只覆盖本次运行，不写回文件。
DeepSeek 从启动环境的 DEEPSEEK_API_KEY 读取密钥，默认 mock 不联网。
start 成功后可关闭终端；chat 的 Ctrl+C 仍会取消当前对话。
内置 get_time；mock 下发送 -m /time 可离线观察两轮工具调用。
SQLite 保存各用户/会话的消息与运行状态；后续请求加载近期完整的成功对话。
mock 下发送 -m /recall 可查看当前上下文中的上一条用户消息。`)
		return err
	}
	command := args[0]
	commandArgs := args[1:]
	if command == "sessions" {
		if len(commandArgs) == 0 || (commandArgs[0] != "list" && commandArgs[0] != "show") {
			return fmt.Errorf("use 'rosetta sessions list|show [agent] --user USER --session SESSION'")
		}
		command = "sessions-" + commandArgs[0]
		commandArgs = commandArgs[1:]
	}
	if command != "serve" && command != "start" && command != "stop" && command != "status" && command != "chat" && command != "sessions-list" && command != "sessions-show" {
		return fmt.Errorf("unknown command %q; run 'rosetta help'", command)
	}
	name := state.DefaultAgent
	flagArgs := commandArgs
	if len(flagArgs) > 0 && !strings.HasPrefix(flagArgs[0], "-") {
		name, flagArgs = flagArgs[0], flagArgs[1:]
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(errOut)
	stateDir := flags.String("state-dir", filepath.Join(homeDir, ".rosetta"), "private state directory")
	var message, session, user, after string
	var limit int
	var rawJSON bool
	var providerName, model string
	var maxTokens int
	var readyFD int
	if command == "serve" || command == "start" {
		flags.StringVar(&providerName, "provider", "mock", "model backend: mock or deepseek")
		flags.StringVar(&model, "model", provider.DefaultDeepSeekModel, "DeepSeek model ID")
		flags.IntVar(&maxTokens, "max-tokens", 512, "maximum output tokens per request (1-4096)")
	}
	if command == "serve" {
		flags.IntVar(&readyFD, "ready-fd", 0, "internal startup pipe (reserved)")
	}
	if command == "chat" || command == "sessions-list" || command == "sessions-show" {
		flags.StringVar(&user, "user", identity.DefaultUser, "local user namespace")
		flags.StringVar(&session, "session", identity.DefaultSession, "session name within this user")
		flags.BoolVar(&rawJSON, "json", false, "print structured JSON")
	}
	if command == "sessions-list" || command == "sessions-show" {
		flags.StringVar(&after, "after", "", "next-page cursor from the previous response")
		flags.IntVar(&limit, "limit", 20, "maximum records per page (1-100)")
	}
	if command == "chat" {
		flags.StringVar(&message, "m", "", "message to send")
	}
	if err := flags.Parse(flagArgs); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	if command == "chat" || command == "sessions-list" || command == "sessions-show" {
		scope, err := identity.Resolve(user, session)
		if err != nil {
			return err
		}
		user, session = scope.User, scope.Session
	}
	if command == "sessions-list" || command == "sessions-show" {
		if limit < 1 || limit > 100 {
			return fmt.Errorf("limit must be 1-100")
		}
	}
	var readyPipe *os.File
	if readyFD != 0 {
		if readyFD != 3 {
			return fmt.Errorf("invalid internal readiness descriptor")
		}
		readyPipe = os.NewFile(uintptr(readyFD), "readiness")
		if readyPipe == nil {
			return fmt.Errorf("missing readiness pipe")
		}
		defer func() {
			if readyPipe != nil {
				if runErr != nil {
					_ = json.NewEncoder(readyPipe).Encode(daemon.Startup{Error: runErr.Error(), AlreadyRunning: errors.Is(runErr, daemon.ErrAlreadyRunning)})
				}
				_ = readyPipe.Close()
			}
		}()
	}
	paths, err := state.Resolve(*stateDir, name)
	if err != nil {
		return err
	}
	if err := paths.Check(); err != nil {
		return err
	}
	if command == "start" || command == "serve" {
		defaults := state.Config{Version: 1, Agent: name, Provider: "mock", Model: provider.DefaultDeepSeekModel, MaxTokens: 512}
		cfg, err := paths.LoadConfig(defaults)
		if err != nil {
			return err
		}
		flags.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "provider":
				cfg.Provider = providerName
			case "model":
				cfg.Model = model
			case "max-tokens":
				cfg.MaxTokens = maxTokens
			}
		})
		if cfg.Provider != "mock" && cfg.Provider != "deepseek" {
			return fmt.Errorf("provider must be mock or deepseek")
		}
		if strings.TrimSpace(cfg.Model) == "" || len(cfg.Model) > 128 {
			return fmt.Errorf("model must be nonblank and at most 128 bytes")
		}
		if cfg.MaxTokens < 1 || cfg.MaxTokens > 4096 {
			return fmt.Errorf("max-tokens must be 1-4096")
		}
		if command == "start" {
			if err := paths.Ensure(); err != nil {
				return err
			}
			if err := paths.InitConfig(defaults); err != nil {
				return err
			}
			childArgs := []string{"serve", name, "--state-dir", paths.Root, "--provider", cfg.Provider, "--model", cfg.Model, "--max-tokens", strconv.Itoa(cfg.MaxTokens)}
			health, existing, err := daemon.Start(ctx, paths, childArgs)
			if err != nil {
				return err
			}
			action := "已启动"
			if existing {
				action = "已在运行（本次启动参数未应用）"
			}
			_, err = fmt.Fprintf(out, "%s %s (pid=%d, instance=%s)\n日志：%s\n", name, action, health.PID, health.InstanceID, paths.Log)
			return err
		}
		logger := slog.New(slog.NewTextHandler(errOut, nil))
		var backend llm.Provider
		switch cfg.Provider {
		case "mock":
			backend = provider.Mock{Delay: 60 * time.Millisecond}
		case "deepseek":
			backend, err = provider.NewDeepSeek(os.Getenv("DEEPSEEK_API_KEY"), cfg.Model, cfg.MaxTokens)
			if err != nil {
				return err
			}
		}
		if err := paths.Ensure(); err != nil {
			return err
		}
		if err := paths.InitConfig(defaults); err != nil {
			return err
		}
		tools, err := tool.NewRegistry(tool.Time{})
		if err != nil {
			return err
		}
		runtime := agent.New(backend, tools)
		var ready func(protocol.Health) error
		if readyPipe != nil {
			ready = func(health protocol.Health) error {
				err := json.NewEncoder(readyPipe).Encode(daemon.Startup{Health: health})
				_ = readyPipe.Close()
				readyPipe = nil
				return err
			}
		}
		return daemon.New(runtime, logger).Serve(ctx, paths, ready)
	}
	if command == "status" || command == "stop" {
		var health protocol.Health
		if command == "stop" {
			health, err = daemon.Stop(ctx, paths)
		} else {
			health, err = daemon.Status(ctx, paths)
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(health)
	}
	health, err := daemon.Status(ctx, paths)
	if err != nil {
		return err
	}
	if health.Status != "ready" {
		return fmt.Errorf("agent %s is %s; run 'rosetta start %s' first", name, health.Status, name)
	}
	c := client.New(paths.Socket)
	defer c.Close()
	encoder := json.NewEncoder(out)
	if command == "sessions-list" {
		page, err := c.Sessions(ctx, user, after, limit)
		if err != nil {
			return err
		}
		if rawJSON {
			return encoder.Encode(page)
		}
		if len(page.Sessions) == 0 {
			_, err = fmt.Fprintf(out, "用户 %s 暂无会话。\n", user)
			return err
		}
		for _, item := range page.Sessions {
			if _, err = fmt.Fprintf(out, "%s  user=%s  turns=%d  id=%s\n", item.Name, item.User, item.Turns, item.ID); err != nil {
				return err
			}
		}
		if page.NextAfter != "" {
			_, err = fmt.Fprintf(out, "下一页：--after %s\n", page.NextAfter)
		}
		return err
	}
	if command == "sessions-show" {
		cursor := int64(0)
		if after != "" {
			cursor, err = strconv.ParseInt(after, 10, 64)
			if err != nil || cursor < 0 {
				return fmt.Errorf("history cursor must be a nonnegative integer")
			}
		}
		page, err := c.History(ctx, user, session, cursor, limit)
		if err != nil {
			return err
		}
		if rawJSON {
			return encoder.Encode(page)
		}
		if _, err = fmt.Fprintf(out, "用户 %s / 会话 %s (id=%s)\n", page.Session.User, page.Session.Name, page.Session.ID); err != nil {
			return err
		}
		for _, item := range page.Messages {
			if _, err = fmt.Fprintf(out, "\n[%s · %s · turn %s]\n%s\n", item.Role, item.Turn.Status, item.Turn.ID, item.Content); err != nil {
				return err
			}
			for _, call := range item.ToolCalls {
				if _, err = fmt.Fprintf(out, "工具调用 %s (%s): %s\n", call.Name, call.ID, call.Arguments); err != nil {
					return err
				}
			}
			if item.ToolCallID != "" {
				if _, err = fmt.Fprintf(out, "对应工具调用：%s\n", item.ToolCallID); err != nil {
					return err
				}
			}
			if item.Turn.Error != "" {
				if _, err = fmt.Fprintf(out, "结束原因：%s\n", item.Turn.Error); err != nil {
					return err
				}
			}
		}
		if page.NextAfter != 0 {
			_, err = fmt.Fprintf(out, "\n下一页：--after %d\n", page.NextAfter)
		}
		return err
	}
	return c.Chat(ctx, protocol.ChatRequest{User: user, Session: session, Message: message}, func(event protocol.Event) error {
		if rawJSON {
			return encoder.Encode(event)
		}
		switch event.Type {
		case protocol.TextDelta:
			_, err := fmt.Fprint(out, event.Text)
			return err
		case protocol.Completed:
			_, err := fmt.Fprintln(out)
			return err
		case protocol.RoundStarted:
			_, err := fmt.Fprintf(errOut, "[第 %d 轮模型调用]\n", event.Round)
			return err
		case protocol.ToolStarted:
			_, err := fmt.Fprintf(errOut, "[执行工具 %s]\n", event.ToolName)
			return err
		case protocol.ToolFinished:
			_, err := fmt.Fprintf(errOut, "[工具 %s：%s (%d ms)]\n", event.ToolName, event.Status, event.DurationMS)
			return err
		}
		return nil
	})
}
