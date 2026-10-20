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
	"syscall"
	"time"

	"rosetta/internal/agent"
	"rosetta/internal/client"
	"rosetta/internal/daemon"
	"rosetta/internal/protocol"
	"rosetta/internal/provider"
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

func run(ctx context.Context, args []string, out, errOut io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(out, `ROSETTA — Daemon + Runtime 单次问答

用法：
  rosetta serve  [--state-dir DIR] [--provider mock|deepseek] [--model MODEL] [--max-tokens 512]
  rosetta status [--state-dir DIR]          检查服务是否就绪
  rosetta chat   [--state-dir DIR] [--session main] [--json] -m "你好"

DeepSeek 模式从 DEEPSEEK_API_KEY 环境变量读取密钥。默认 mock 不联网。
当前没有历史记忆、会话持久化或文件/命令工具。默认目录：~/.rosetta`)
		return err
	}
	command := args[0]
	if command != "serve" && command != "status" && command != "chat" {
		return fmt.Errorf("unknown command %q; run 'rosetta help'", command)
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
	var providerName, model string
	var maxTokens int
	if command == "serve" {
		flags.StringVar(&providerName, "provider", "mock", "model backend: mock or deepseek")
		flags.StringVar(&model, "model", provider.DefaultDeepSeekModel, "DeepSeek model ID")
		flags.IntVar(&maxTokens, "max-tokens", 512, "maximum output tokens per request (1-4096)")
	}
	if command == "chat" {
		flags.StringVar(&message, "m", "", "message to send")
		flags.StringVar(&session, "session", "main", "session identifier (M1: admission only, no history)")
		flags.BoolVar(&rawJSON, "json", false, "print raw event frames")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	if *stateDir == "" {
		return fmt.Errorf("state-dir must not be empty")
	}
	absDir, err := filepath.Abs(*stateDir)
	if err != nil {
		return err
	}
	if command == "serve" {
		logger := slog.New(slog.NewTextHandler(errOut, nil))
		var backend agent.TextProvider
		switch providerName {
		case "mock":
			backend = provider.Mock{Delay: 60 * time.Millisecond}
		case "deepseek":
			backend, err = provider.NewDeepSeek(os.Getenv("DEEPSEEK_API_KEY"), model, maxTokens)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown provider %q (choose mock or deepseek)", providerName)
		}
		runtime := agent.New(backend)
		return daemon.New(runtime, logger).Serve(ctx, absDir)
	}
	c := client.New(daemon.SocketPath(absDir))
	defer c.Close()
	if command == "status" {
		health, err := c.Health(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(health)
	}
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
		}
		return nil
	})
}
