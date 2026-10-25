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
  rosetta start  [agent] [--state-dir DIR] [--provider mock|deepseek] [--model MODEL] [--max-tokens 512] [--workspace DIR]
  rosetta serve  [agent] [同 start 参数]   前台运行，Ctrl+C 停止
  rosetta status [agent] [--state-dir DIR]
  rosetta stop   [agent] [--state-dir DIR]
  rosetta chat   [agent] [--state-dir DIR] [--session main] [--json] -m "你好"

Agent 名称放在选项之前，允许 1–32 位 ASCII 字母/数字/_/-。
默认根目录 ~/.rosetta；每个 Agent 存在 agents/<name>/ 下。
agent.json 保存非敏感配置；显式启动参数只覆盖本次运行，不写回文件。
DeepSeek 从启动环境的 DEEPSEEK_API_KEY 读取密钥，默认 mock 不联网。
start 成功后可关闭终端；chat 的 Ctrl+C 仍会取消当前对话。
内置 get_time、list_files、read_files、阻塞式 subagent。
mock 演示：/time、/ls [目录]、/read 文件、/delegate 任务。
子 Agent 共用文件工具的工作目录，每轮父请求最多两个，不支持递归。
当前没有跨请求历史、用户命名空间、会话数据库或子任务持久化。`)
		return err
	}
	command := args[0]
	if command != "serve" && command != "start" && command != "stop" && command != "status" && command != "chat" {
		return fmt.Errorf("unknown command %q; run 'rosetta help'", command)
	}
	name := state.DefaultAgent
	flagArgs := args[1:]
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
	var message, session string
	var rawJSON bool
	var providerName, model, workspace string
	var maxTokens int
	var readyFD int
	if command == "serve" || command == "start" {
		flags.StringVar(&workspace, "workspace", "", "read-only workspace directory (default: agent workspace)")
		flags.StringVar(&providerName, "provider", "mock", "model backend: mock or deepseek")
		flags.StringVar(&model, "model", provider.DefaultDeepSeekModel, "DeepSeek model ID")
		flags.IntVar(&maxTokens, "max-tokens", 512, "maximum output tokens per request (1-4096)")
	}
	if command == "serve" {
		flags.IntVar(&readyFD, "ready-fd", 0, "internal startup pipe (reserved)")
	}
	if command == "chat" {
		flags.StringVar(&message, "m", "", "message to send")
		flags.StringVar(&session, "session", "main", "session identifier (M1: admission only, no history)")
		flags.BoolVar(&rawJSON, "json", false, "print raw event frames")
	}
	if err := flags.Parse(flagArgs); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
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
			case "workspace":
				cfg.Workspace = workspace
			case "provider":
				cfg.Provider = providerName
			case "model":
				cfg.Model = model
			case "max-tokens":
				cfg.MaxTokens = maxTokens
			}
		})
		if workspace != "" {
			cfg.Workspace, err = filepath.Abs(workspace)
			if err != nil {
				return err
			}
		} else if cfg.Workspace == "" {
			cfg.Workspace = paths.Workspace
		} else if !filepath.IsAbs(cfg.Workspace) {
			return fmt.Errorf("workspace in agent.json must be an absolute path")
		}
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
			childArgs := []string{"serve", name, "--state-dir", paths.Root, "--provider", cfg.Provider, "--model", cfg.Model, "--max-tokens", strconv.Itoa(cfg.MaxTokens), "--workspace", cfg.Workspace}
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
		files, err := tool.OpenWorkspace(cfg.Workspace)
		if err != nil {
			return err
		}
		defer files.Close()
		runtime, err := agent.NewWithSubagents(backend, tool.Time{}, tool.ReadFiles{Workspace: files}, tool.ListFiles{Workspace: files})
		if err != nil {
			return err
		}
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
	return c.Chat(ctx, protocol.ChatRequest{SessionID: session, Message: message}, func(event protocol.Event) error {
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
