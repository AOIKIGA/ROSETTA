//go:build darwin || linux

package tool

import (
	"os"
	"syscall"
)

func openWorkspaceFile(root *os.Root, path string) (*os.File, error) {
	return root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
