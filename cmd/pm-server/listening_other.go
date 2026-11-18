//go:build !linux && !darwin

package main

// tcpListens has no proven check on this system, so --listen-fd refuses each
// descriptor. PM supports Linux and Darwin.
func tcpListens(int) bool {
	return false
}
