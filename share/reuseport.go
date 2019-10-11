//+build !windows

package chshare

import (
	"runtime"
	"syscall"
)

func reusePort(network, address string, conn syscall.RawConn) error {
	return conn.Control(func(descriptor uintptr) {
		switch runtime.GOOS {
		case "darwin":
			syscall.SetsockoptInt(int(descriptor), syscall.SOL_SOCKET, 0x200 /* syscall.SO_REUSEPORT */, 1)
		case "linux":
			syscall.SetsockoptInt(int(descriptor), syscall.SOL_SOCKET, 0x0F, 1)
		}
	})
}
