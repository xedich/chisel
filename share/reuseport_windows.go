//+build windows

package chshare

import (
	"syscall"
	"golang.org/x/sys/windows"
)

func reusePort(network, address string, c syscall.RawConn) (err error) {
	return c.Control(func(fd uintptr) {
		err = windows.SetsockoptInt(windows.Handle(fd), windows.SOL_SOCKET, windows.SO_REUSEADDR, 1)
	})
}
