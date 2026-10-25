//go:build !darwin && !linux

package tool

import (
	"fmt"
	"os"
)

func openWorkspaceFile(*os.Root, string) (*os.File, error) {
	return nil, fmt.Errorf("workspace tools support macOS and Linux only")
}
