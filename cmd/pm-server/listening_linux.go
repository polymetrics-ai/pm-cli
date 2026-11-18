//go:build linux

package main

import "syscall"

// ipprotoMPTCP is IPPROTO_MPTCP of <linux/in.h>. The syscall package defines
// it only for loong64. Go's net.Listen makes an MPTCP socket for "tcp" when
// the kernel has MPTCP turned on (net/dial.go, defaultMPTCPEnabledListen), and
// pm-server does the same when it binds server.listen itself. An MPTCP
// listener also accepts plain TCP clients. So rule 3 accepts it as TCP.
const ipprotoMPTCP = 0x106

// tcpListens tells if the socket fd uses TCP, plain or MPTCP, and listens for
// connections.
func tcpListens(fd int) bool {
	protocol, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_PROTOCOL)
	if err != nil || (protocol != syscall.IPPROTO_TCP && protocol != ipprotoMPTCP) {
		return false
	}
	listening, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_ACCEPTCONN)
	return err == nil && listening != 0
}
