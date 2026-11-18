//go:build darwin

package main

import (
	"syscall"
	"unsafe"
)

// Darwin answers ENOPROTOOPT for getsockopt SO_ACCEPTCONN and has no
// SO_PROTOCOL. The TCP option TCP_CONNECTION_INFO gives struct
// tcp_connection_info, whose first byte is tcpi_state (<netinet/tcp.h>). Only
// a TCP socket answers the option. A socket that listens has the state
// TCPS_LISTEN (<netinet/tcp_fsm.h>).
const (
	tcpConnectionInfo = 0x106
	tcpStateListen    = 1
)

// tcpListens tells if the socket fd uses TCP and listens for connections.
func tcpListens(fd int) bool {
	var info [256]byte
	size := uint32(len(info))
	_, _, errno := syscall.Syscall6(syscall.SYS_GETSOCKOPT, uintptr(fd), syscall.IPPROTO_TCP, tcpConnectionInfo, uintptr(unsafe.Pointer(&info[0])), uintptr(unsafe.Pointer(&size)), 0)
	return errno == 0 && size > 0 && info[0] == tcpStateListen
}
