//go:build linux

package main

import "syscall"

// tcpListens tells if the socket fd uses TCP and listens for connections.
func tcpListens(fd int) bool {
	protocol, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_PROTOCOL)
	if err != nil || protocol != syscall.IPPROTO_TCP {
		return false
	}
	listening, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_ACCEPTCONN)
	return err == nil && listening != 0
}
