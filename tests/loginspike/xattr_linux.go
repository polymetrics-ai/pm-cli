//go:build linux

package main

import "syscall"

// hasFileCapability reports whether path carries a security.capability
// extended attribute.
func hasFileCapability(path string) bool {
	buffer := make([]byte, 64)
	_, err := syscall.Getxattr(path, "security.capability", buffer)
	return err == nil
}
