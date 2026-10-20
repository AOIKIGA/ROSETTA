//go:build darwin || linux

package daemon

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

func shortStateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rs-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestListenerLockAndRestart(t *testing.T) {
	dir := shortStateDir(t)
	_, cleanup, err := listenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, secondCleanup, err := listenLocal(dir)
	if err == nil {
		secondCleanup()
		cleanup()
		t.Fatal("second daemon acquired same state directory")
	}
	cleanup()
	if _, err := os.Stat(SocketPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("socket remains: %v", err)
	}
	_, cleanup, err = listenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
}

func TestStaleSocketRecoveryAndRegularFileProtection(t *testing.T) {
	dir := shortStateDir(t)
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: SocketPath(dir), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	_ = stale.Close()
	_, cleanup, err := listenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	path := filepath.Join(dir, "daemon.sock")
	if err := os.WriteFile(path, []byte("keep me"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, cleanup, err := listenLocal(dir); err == nil {
		cleanup()
		t.Fatal("overwrote regular file")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "keep me" {
		t.Fatal("regular file was changed")
	}
}
