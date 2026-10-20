//go:build !darwin && !linux

package daemon

import (
	"fmt"
	"net"
)

func listenLocal(string) (net.Listener, func(), error) {
	return nil, nil, fmt.Errorf("M1 supports macOS and Linux only")
}
