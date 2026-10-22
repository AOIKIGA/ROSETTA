//go:build !darwin && !linux

package daemon

import (
	"fmt"
	"os/exec"
)

func detach(*exec.Cmd) error { return fmt.Errorf("daemon processes support macOS and Linux only") }
func lockHeld(string) (bool, error) {
	return false, fmt.Errorf("daemon processes support macOS and Linux only")
}
