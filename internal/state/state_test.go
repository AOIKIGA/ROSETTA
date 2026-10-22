package state

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testPaths(t *testing.T, name string) Paths {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "rs-state-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	p, err := Resolve(root, name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNamesPathsAndReadOnlyCheck(t *testing.T) {
	for _, name := range []string{"", "../bad", "a/b", ".", "a b", strings.Repeat("a", 33)} {
		if _, err := Resolve("/tmp/rs", name); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	if _, err := Resolve("/tmp/"+strings.Repeat("x", 100), "a"); err == nil {
		t.Fatal("accepted long socket path")
	}
	p := testPaths(t, "personal")
	if err := p.Check(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.Dir); !os.IsNotExist(err) {
		t.Fatal("read-only check created an agent")
	}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p.Run, 0755); err != nil {
		t.Fatal(err)
	}
	if err := p.Check(); err == nil {
		t.Fatal("accepted public directory")
	}
}

func TestConfigConcurrentInitializationAndPreservation(t *testing.T) {
	p := testPaths(t, "personal")
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	defaults := Config{Version: 1, Agent: p.Agent, Provider: "mock", Model: "model", MaxTokens: 512}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := p.InitConfig(defaults); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	cfg, err := p.LoadConfig(Config{})
	if err != nil || cfg != defaults {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
	cfg.Provider = "deepseek"
	if err := WriteJSON(p.Config, cfg); err != nil {
		t.Fatal(err)
	}
	if err := p.InitConfig(defaults); err != nil {
		t.Fatal(err)
	}
	actual, err := p.LoadConfig(defaults)
	if err != nil || actual != cfg {
		t.Fatalf("existing config overwritten: %+v %v", actual, err)
	}
	if err := os.WriteFile(p.Config, []byte(`{"version":1,"agent":"other"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.LoadConfig(defaults); err == nil {
		t.Fatal("accepted wrong identity")
	}
}

func TestRejectSymlinkDirectoriesAndFiles(t *testing.T) {
	p := testPaths(t, "personal")
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(p.Root, "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, p.Log); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenPrivateFile(p.Log, os.O_APPEND|os.O_WRONLY); err == nil {
		f.Close()
		t.Fatal("followed log symlink")
	}
	if err := os.Remove(p.Run); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(p.Logs, p.Run); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(); err == nil {
		t.Fatal("followed runtime directory symlink")
	}
}
