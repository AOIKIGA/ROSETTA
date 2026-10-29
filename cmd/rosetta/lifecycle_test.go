//go:build darwin || linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"rosetta/internal/protocol"
	"rosetta/internal/store"
)

// Exercise the actual binary: process detachment cannot be verified by calling run.
func TestDetachedLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and launches the CLI")
	}
	dir, err := os.MkdirTemp("/tmp", "rs-process-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	binary := filepath.Join(dir, "rosetta")
	build := exec.Command("go", "build", "-race", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	root := filepath.Join(dir, "state")
	invoke := func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, binary, append(args, "--state-dir", root)...).CombinedOutput()
	}
	must := func(args ...string) []byte {
		t.Helper()
		output, err := invoke(args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, output)
		}
		return output
	}
	health := func(name string) protocol.Health {
		t.Helper()
		var h protocol.Health
		if err := json.Unmarshal(must("status", name), &h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	for _, name := range []string{"personal", "work", "race", "broken"} {
		t.Cleanup(func() {
			output, err := invoke("stop", name)
			if err != nil {
				t.Errorf("cleanup %s: %v %s", name, err, output)
			}
		})
	}
	// The launcher has its own session, as a shell would. It must exit promptly
	// without its child keeping the output pipe or terminal process group alive.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	starter := exec.CommandContext(ctx, binary, "start", "--state-dir", root)
	starter.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if output, err := starter.CombinedOutput(); err != nil {
		t.Fatalf("start: %v %s", err, output)
	}
	first := health("personal")
	if first.Status != "ready" || first.Agent != "personal" {
		t.Fatalf("%+v", first)
	}
	pgid, err := syscall.Getpgid(first.PID)
	if err != nil || pgid != first.PID || pgid == starter.Process.Pid {
		t.Fatalf("daemon did not detach: pgid=%d health=%+v err=%v", pgid, first, err)
	}
	for _, sig := range []syscall.Signal{syscall.SIGHUP, syscall.SIGINT} {
		if err := syscall.Kill(-starter.Process.Pid, sig); err != nil && err != syscall.ESRCH {
			t.Fatal(err)
		}
	}
	if current := health("personal"); current.InstanceID != first.InstanceID {
		t.Fatal("terminal signal changed daemon")
	}
	if output := string(must("start", "personal")); !strings.Contains(output, "已在运行") {
		t.Fatal(output)
	}
	if current := health("personal"); current.InstanceID != first.InstanceID {
		t.Fatal("duplicate start replaced daemon")
	}
	must("start", "work")
	work := health("work")
	if work.PID == first.PID || work.InstanceID == first.InstanceID {
		t.Fatal("agents share process")
	}
	if output := string(must("chat", "personal", "-m", "/time", "--json")); !strings.Contains(output, `"type":"tool.finished"`) || !strings.Contains(output, `"type":"turn.completed"`) {
		t.Fatal(output)
	}
	chatCtx, cancelChat := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelChat()
	chat := exec.CommandContext(chatCtx, binary, "chat", "personal", "--state-dir", root, "--json", "-m", strings.Repeat("hello", 1000))
	chatOutput, err := chat.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := chat.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(chatOutput).ReadString('\n')
	if err != nil || !strings.Contains(line, `"type":"turn.started"`) {
		t.Fatalf("chat start: %q %v", line, err)
	}
	if err := chat.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := chat.Wait(); err == nil || chat.ProcessState.ExitCode() != 130 {
		t.Fatalf("Ctrl+C did not cancel chat: %v", err)
	}
	if current := health("personal"); current.InstanceID != first.InstanceID {
		t.Fatal("chat cancellation stopped daemon")
	}
	must("chat", "personal", "-m", "/time") // Reuse the canceled session.
	// Named users/sessions have separate durable histories within an agent.
	must("chat", "personal", "--user", "alice", "--session", "main", "-m", "alice-secret")
	must("chat", "personal", "--user", "bob", "--session", "main", "-m", "bob-secret")
	must("chat", "personal", "--user", "alice", "--session", "learning", "-m", "lesson")
	var aliceHistory store.HistoryPage
	if err := json.Unmarshal(must("sessions", "show", "personal", "--user", "alice", "--session", "main", "--json"), &aliceHistory); err != nil {
		t.Fatal(err)
	}
	if len(aliceHistory.Messages) != 2 || aliceHistory.Messages[0].Content != "alice-secret" || aliceHistory.Messages[1].Turn.Status != "completed" {
		t.Fatalf("%+v", aliceHistory)
	}
	var aliceSessions store.SessionPage
	if err := json.Unmarshal(must("sessions", "list", "personal", "--user", "alice", "--json"), &aliceSessions); err != nil || len(aliceSessions.Sessions) != 2 {
		t.Fatalf("%+v %v", aliceSessions, err)
	}
	var defaultHistory store.HistoryPage
	if err := json.Unmarshal(must("sessions", "show", "personal", "--json"), &defaultHistory); err != nil {
		t.Fatal(err)
	}
	if defaultHistory.Session.User != "defaultuser" || defaultHistory.Session.Name != "main" {
		t.Fatalf("%+v", defaultHistory.Session)
	}
	var workSessions store.SessionPage
	if err := json.Unmarshal(must("sessions", "list", "work", "--user", "alice", "--json"), &workSessions); err != nil || len(workSessions.Sessions) != 0 {
		t.Fatalf("cross-agent history: %+v %v", workSessions, err)
	}
	// A competing foreground serve must fail without changing the live instance.
	if output, err := invoke("serve", "personal"); err == nil || !strings.Contains(string(output), "lock") {
		t.Fatalf("duplicate serve: %v %s", err, output)
	}
	// Concurrent starts converge on one ready instance.
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if output, err := invoke("start", "race"); err != nil {
				t.Errorf("concurrent start: %v %s", err, output)
			}
		})
	}
	wg.Wait()
	if h := health("race"); h.Status != "ready" {
		t.Fatalf("%+v", h)
	}
	// A failed child reports its startup error, without claiming ready or leaking a daemon.
	ctxFail, cancelFail := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelFail()
	failed := exec.CommandContext(ctxFail, binary, "start", "broken", "--state-dir", root, "--provider", "deepseek")
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "DEEPSEEK_API_KEY=") {
			failed.Env = append(failed.Env, entry)
		}
	}
	failed.Env = append(failed.Env, "DEEPSEEK_API_KEY=")
	if output, err := failed.CombinedOutput(); err == nil || !strings.Contains(string(output), "DEEPSEEK_API_KEY") {
		t.Fatalf("missing key: %v %s", err, output)
	}
	if h := health("broken"); h.Status != "stopped" {
		t.Fatalf("failed child left daemon: %+v", h)
	}
	// Persistent config is reused; explicit runtime overrides must not rewrite it.
	configPath := filepath.Join(root, "agents", "personal", "agent.json")
	configBefore, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	must("stop", "personal")
	if h := health("work"); h.InstanceID != work.InstanceID {
		t.Fatal("stopped unrelated agent")
	}
	for _, leaf := range []string{"daemon.sock", "instance.json"} {
		if _, err := os.Stat(filepath.Join(root, "agents", "personal", "run", leaf)); !os.IsNotExist(err) {
			t.Fatalf("runtime file remains: %s %v", leaf, err)
		}
	}
	must("start", "personal", "--max-tokens", "64")
	second := health("personal")
	var restartedHistory store.HistoryPage
	if err := json.Unmarshal(must("sessions", "show", "personal", "--user", "alice", "--json"), &restartedHistory); err != nil {
		t.Fatal(err)
	}
	if restartedHistory.Session.ID != aliceHistory.Session.ID || len(restartedHistory.Messages) != 2 {
		t.Fatal("restart lost session history")
	}
	// The restarted daemon reconstructs model context from its SQLite database.
	for _, tc := range []struct{ agent, user, session, want string }{
		{"personal", "alice", "main", "上一条用户消息：alice-secret"},
		{"personal", "bob", "main", "上一条用户消息：bob-secret"},
		{"personal", "alice", "learning", "上一条用户消息：lesson"},
		{"work", "alice", "main", "当前上下文没有更早的用户消息"},
	} {
		output := string(must("chat", tc.agent, "--user", tc.user, "--session", tc.session, "-m", "/recall"))
		if !strings.Contains(output, tc.want) {
			t.Fatalf("context isolation/restart for %+v: %s", tc, output)
		}
	}
	if second.InstanceID == first.InstanceID {
		t.Fatal("restart reused instance ID")
	}
	configAfter, err := os.ReadFile(configPath)
	if err != nil || string(configAfter) != string(configBefore) {
		t.Fatal("startup override changed persisted config")
	}
	// A crash leaves stale metadata/socket; lock ownership allows safe recovery.
	if err := syscall.Kill(second.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		output, err := invoke("status", "personal")
		if err == nil && strings.Contains(string(output), `"status":"stopped"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("crashed instance retained lock: %v %s", err, output)
		}
		time.Sleep(20 * time.Millisecond)
	}
	must("start", "personal")
	if h := health("personal"); h.InstanceID == second.InstanceID {
		t.Fatal("crash recovery reused instance")
	}
	// Configuration and instance diagnostics remain private.
	for _, path := range []string{configPath, filepath.Join(root, "agents", "personal", "run", "instance.json")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal(fmt.Sprintf("private file %s: %v %v", path, info, err))
		}
	}
}
