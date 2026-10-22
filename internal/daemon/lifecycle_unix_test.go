//go:build darwin || linux

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"rosetta/internal/agent"
	"rosetta/internal/client"
	"rosetta/internal/protocol"
	"rosetta/internal/state"
)

func TestStopCancelsActiveTurnAndCleansInstance(t *testing.T) {
	paths, err := state.Resolve(shortStateDir(t), "personal")
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	p := blockingProvider{entered: make(chan struct{}, 1)}
	d := New(agent.New(p, nil), testLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ready := make(chan protocol.Health, 1)
	done := make(chan error, 1)
	go func() { done <- d.Serve(ctx, paths, func(h protocol.Health) error { ready <- h; return nil }) }()
	var health protocol.Health
	select {
	case health = <-ready:
	case err := <-done:
		t.Fatalf("serve before ready: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	c := client.New(paths.Socket)
	defer c.Close()
	if err := c.Shutdown(ctx, "stale-instance"); err == nil {
		t.Fatal("accepted stale shutdown")
	}
	if h, err := Status(ctx, paths); err != nil || h.InstanceID != health.InstanceID || h.Status != "ready" {
		t.Fatalf("health=%+v err=%v", h, err)
	}
	chatDone := make(chan error, 1)
	go func() {
		chatDone <- c.Chat(ctx, protocol.ChatRequest{SessionID: "main", Message: "hi"}, func(protocol.Event) error { return nil })
	}()
	select {
	case <-p.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if h, err := Stop(ctx, paths); err != nil || h.Status != "stopped" {
		t.Fatalf("stop=%+v err=%v", h, err)
	}
	select {
	case err := <-chatDone:
		if err == nil {
			t.Fatal("active turn unexpectedly completed")
		}
	case <-ctx.Done():
		t.Fatal("turn did not cancel")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("serve did not exit")
	}
	for _, path := range []string{paths.Socket, paths.Instance} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("runtime file remains: %s %v", path, err)
		}
	}
	if _, err := os.Stat(paths.Lock); err != nil {
		t.Fatalf("lock inode must remain: %v", err)
	}
	if _, err := Stop(ctx, paths); err != nil {
		t.Fatalf("second stop: %v", err)
	}
}

func TestShutdownValidationAndAdmission(t *testing.T) {
	d := New(nil, testLogger())
	d.health = protocol.Health{Protocol: protocol.Version, InstanceID: "current", Status: "ready"}
	h := d.Handler()
	for _, body := range []string{`{}`, `{"instance_id":"old"}`, `{"instance_id":"current","unknown":1}`, `{"instance_id":"current"} {}`} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/shutdown", strings.NewReader(body)))
		if w.Code == http.StatusOK || d.stopping {
			t.Fatalf("accepted invalid shutdown: %s", body)
		}
	}
	for range 2 {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/shutdown", strings.NewReader(`{"instance_id":"current"}`)))
		if w.Code != http.StatusOK {
			t.Fatal(w.Body.String())
		}
		var health protocol.Health
		if err := json.Unmarshal(w.Body.Bytes(), &health); err != nil || health.Status != "stopping" {
			t.Fatalf("%s", w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat", strings.NewReader(`{"session_id":"main","message":"hi"}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("admitted during shutdown: %d", w.Code)
	}
}

func TestStatusDoesNotTrustStaleMetadata(t *testing.T) {
	paths, err := state.Resolve(shortStateDir(t), "personal")
	if err != nil {
		t.Fatal(err)
	}
	if h, err := Stop(context.Background(), paths); err != nil || h.Status != "stopped" {
		t.Fatalf("%+v %v", h, err)
	}
	if _, err := os.Stat(paths.Dir); !os.IsNotExist(err) {
		t.Fatal("stop created agent")
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteJSON(paths.Instance, protocol.Health{PID: os.Getpid(), InstanceID: "stale"}); err != nil {
		t.Fatal(err)
	}
	if h, err := Stop(context.Background(), paths); err != nil || h.Status != "stopped" {
		t.Fatalf("stale pid affected stop: %+v %v", h, err)
	}
	_, cleanup, err := listenLocal(paths.Run)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := Status(ctx, paths); err == nil {
		t.Fatal("unresponsive daemon reported as stopped/ready")
	}
}

// This helper only runs in a subprocess started by TestStartupCancellation.
func TestStartupHelper(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "block-readiness" {
		return
	}
	if err := os.WriteFile("helper.pid", []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		os.Exit(2)
	}
	time.Sleep(time.Minute)
	os.Exit(3)
}

func TestStartupCancellationKillsOnlyOwnedChild(t *testing.T) {
	paths, err := state.Resolve(shortStateDir(t), "cancel")
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pidReady := make(chan int, 1)
	go func() {
		for {
			data, err := os.ReadFile(filepath.Join(paths.Dir, "helper.pid"))
			if err == nil {
				pid, _ := strconv.Atoi(string(data))
				pidReady <- pid
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	_, _, err = Start(ctx, paths, []string{"-test.run=^TestStartupHelper$", "--", "block-readiness"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("startup cancellation: %v", err)
	}
	select {
	case pid := <-pidReady:
		if pid <= 0 {
			t.Fatal("invalid helper pid")
		}
		if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
			t.Fatalf("startup child still exists: pid=%d err=%v", pid, err)
		}
	default:
		t.Fatal("child did not reach readiness wait")
	}
}
