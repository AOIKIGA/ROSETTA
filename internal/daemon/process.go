package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"rosetta/internal/client"
	"rosetta/internal/protocol"
	"rosetta/internal/state"
)

var ErrAlreadyRunning = errors.New("daemon lock is held")

// Startup is sent once over the private parent/child pipe, not over stdout.
type Startup struct {
	Health         protocol.Health `json:"health"`
	Error          string          `json:"error,omitempty"`
	AlreadyRunning bool            `json:"already_running,omitempty"`
}

func Status(ctx context.Context, paths state.Paths) (protocol.Health, error) {
	if err := paths.Check(); err != nil {
		return protocol.Health{}, err
	}
	held, err := lockHeld(paths.Lock)
	if err != nil {
		return protocol.Health{}, err
	}
	if !held {
		return protocol.Health{Status: "stopped", Agent: paths.Agent, Protocol: protocol.Version}, nil
	}
	c := client.New(paths.Socket)
	defer c.Close()
	health, err := c.Health(ctx)
	if err != nil {
		return health, fmt.Errorf("agent %s holds its lock but is unavailable (starting, stopping or unresponsive): %w", paths.Agent, err)
	}
	if health.Agent != paths.Agent || health.InstanceID == "" || health.PID <= 0 {
		return health, fmt.Errorf("daemon identity mismatch")
	}
	return health, nil
}

// Start waits for readiness and verifies it through the socket. The child's
// lifetime is independent of ctx after this function returns successfully.
func Start(parent context.Context, paths state.Paths, serveArgs []string) (protocol.Health, bool, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	if err := paths.Check(); err != nil {
		return protocol.Health{}, false, err
	}
	held, err := lockHeld(paths.Lock)
	if err != nil {
		return protocol.Health{}, false, err
	}
	if held {
		health, err := waitReady(ctx, paths)
		return health, true, err
	}
	executable, err := os.Executable()
	if err != nil {
		return protocol.Health{}, false, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return protocol.Health{}, false, err
	}
	defer reader.Close()
	defer writer.Close()
	log, err := state.OpenPrivateFile(paths.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND)
	if err != nil {
		return protocol.Health{}, false, err
	}
	defer log.Close()
	null, err := os.Open(os.DevNull)
	if err != nil {
		return protocol.Health{}, false, err
	}
	defer null.Close()
	args := append(append([]string(nil), serveArgs...), "--ready-fd=3")
	cmd := exec.Command(executable, args...) // Deliberately NOT CommandContext.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, log, log
	cmd.ExtraFiles = []*os.File{writer}
	cmd.Dir = paths.Dir
	if err := detach(cmd); err != nil {
		return protocol.Health{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return protocol.Health{}, false, err
	}
	if err := cmd.Start(); err != nil {
		return protocol.Health{}, false, err
	}
	_ = writer.Close() // EOF now reflects the child, not our own pipe descriptor.
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	handedOff := false
	defer func() {
		if !handedOff {
			// Only kill the child we created, never a PID read from a file.
			_ = cmd.Process.Kill()
			select {
			case <-exited:
			case <-time.After(time.Second):
			}
		}
	}()
	type reply struct {
		startup Startup
		err     error
	}
	replies := make(chan reply, 1)
	go func() {
		var startup Startup
		err := json.NewDecoder(io.LimitReader(reader, 16<<10)).Decode(&startup)
		replies <- reply{startup, err}
	}()
	select {
	case <-ctx.Done():
		return protocol.Health{}, false, fmt.Errorf("startup did not complete; see %s: %w", paths.Log, ctx.Err())
	case result := <-replies:
		if result.err != nil {
			return protocol.Health{}, false, fmt.Errorf("daemon exited before readiness; see %s: %w", paths.Log, result.err)
		}
		if result.startup.AlreadyRunning {
			health, err := waitReady(ctx, paths)
			return health, true, err
		}
		if result.startup.Error != "" {
			return protocol.Health{}, false, fmt.Errorf("daemon startup: %s (log: %s)", result.startup.Error, paths.Log)
		}
		health, err := waitReady(ctx, paths)
		if err != nil {
			return health, false, err
		}
		if health.InstanceID != result.startup.Health.InstanceID || health.PID != cmd.Process.Pid {
			return health, false, fmt.Errorf("daemon changed during startup")
		}
		if err := ctx.Err(); err != nil {
			return health, false, err
		}
		handedOff = true
		return health, false, nil
	}
}

func waitReady(ctx context.Context, paths state.Paths) (protocol.Health, error) {
	var lastErr error
	for {
		health, err := Status(ctx, paths)
		if err == nil && health.Status == "ready" {
			return health, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return protocol.Health{}, fmt.Errorf("agent did not become ready (last status error: %v): %w", lastErr, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Stop acknowledges the observed instance, then waits for its lock to be released.
// A replacement instance is never stopped on behalf of an earlier stop request.
func Stop(parent context.Context, paths state.Paths) (protocol.Health, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	health, err := Status(ctx, paths)
	if err != nil || health.Status == "stopped" {
		return health, err
	}
	c := client.New(paths.Socket)
	defer c.Close()
	if err := c.Shutdown(ctx, health.InstanceID); err != nil {
		// Another stop may have completed between health and shutdown.
		if current, statusErr := Status(ctx, paths); statusErr == nil && current.Status == "stopped" {
			return current, nil
		}
		return health, err
	}
	for {
		current, err := Status(ctx, paths)
		if err == nil {
			if current.Status == "stopped" {
				return current, nil
			}
			if current.InstanceID != health.InstanceID {
				return current, fmt.Errorf("original instance stopped, but a new instance is already running")
			}
		}
		select {
		case <-ctx.Done():
			return health, fmt.Errorf("could not confirm daemon stopped; no PID was killed: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}
