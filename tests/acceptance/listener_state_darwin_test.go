//go:build darwin

package acceptance

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Darwin answers ENOPROTOOPT for getsockopt SO_ACCEPTCONN. The TCP option
// TCP_CONNECTION_INFO gives struct tcp_connection_info, whose first byte is
// tcpi_state (<netinet/tcp.h>). A listening socket has the state TCPS_LISTEN
// (<netinet/tcp_fsm.h>). This is the read of cmd/pm-server/listening_darwin.go,
// written again so that the harness does not use the product code.
const (
	darwinTCPConnectionInfo = 0x106
	darwinTCPStateListen    = 1
)

// socketListens reads the listen state of the TCP socket fd. It makes no
// connection.
func socketListens(fd int) (bool, error) {
	var info [256]byte
	size := uint32(len(info))
	_, _, errno := syscall.Syscall6(syscall.SYS_GETSOCKOPT, uintptr(fd), syscall.IPPROTO_TCP, darwinTCPConnectionInfo,
		uintptr(unsafe.Pointer(&info[0])), uintptr(unsafe.Pointer(&size)), 0)
	if errno != 0 || size == 0 {
		return false, fmt.Errorf("getsockopt TCP_CONNECTION_INFO: %v", errno)
	}
	return info[0] == darwinTCPStateListen, nil
}
