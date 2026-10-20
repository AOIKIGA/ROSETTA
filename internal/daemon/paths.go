package daemon

import "path/filepath"

func SocketPath(stateDir string) string {
	return filepath.Join(stateDir, "daemon.sock")
}
