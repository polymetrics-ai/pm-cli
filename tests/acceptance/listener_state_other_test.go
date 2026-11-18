//go:build !darwin

package acceptance

import (
	"fmt"
	"syscall"
)

// socketListens reads the listen state of the socket fd with getsockopt
// SO_ACCEPTCONN. It makes no connection.
func socketListens(fd int) (bool, error) {
	accepting, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_ACCEPTCONN)
	if err != nil {
		return false, fmt.Errorf("getsockopt SO_ACCEPTCONN: %w", err)
	}
	return accepting != 0, nil
}
