//go:build darwin || linux

package daemon

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"rosetta/internal/state"
	"syscall"
	"time"
)

func listenLocal(stateDir string) (net.Listener, func(), error) {
	if len(SocketPath(stateDir)) >= 104 {
		return nil, nil, fmt.Errorf("socket path too long; use a shorter --state-dir")
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, nil, err
	}
	info, err := os.Lstat(stateDir)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, nil, fmt.Errorf("state directory must be a private directory (0700): %s", stateDir)
	}
	lock, err := state.OpenPrivateFile(filepath.Join(stateDir, "daemon.lock"), os.O_CREATE|os.O_RDWR)
	if err != nil {
		return nil, nil, err
	}
	// Status briefly probes this same lock. Retry transient contention before
	// deciding another daemon owns it; the lock file's inode is never replaced.
	for attempt := 0; ; attempt++ {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			if attempt < 10 {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			_ = lock.Close()
			return nil, nil, ErrAlreadyRunning
		}
		_ = lock.Close()
		return nil, nil, fmt.Errorf("cannot acquire daemon lock: %w", err)
	}
	unlock := func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}
	// The lifetime lock makes stale-socket cleanup safe against another instance.
	// Never remove the lock file: replacing its inode would create two locks.
	path := SocketPath(stateDir)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			unlock()
			return nil, nil, fmt.Errorf("refusing to remove non-socket file: %s", path)
		}
		if err := os.Remove(path); err != nil {
			unlock()
			return nil, nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		unlock()
		return nil, nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		unlock()
		return nil, nil, err
	}
	cleanup := func() {
		_ = listener.Close() // UnixListener also removes its socket file.
		unlock()
	}
	if err := os.Chmod(path, 0600); err != nil {
		cleanup()
		return nil, nil, err
	}
	return listener, cleanup, nil
}
