//go:build !linux

package main

// hasFileCapability is Linux-only; the spike runs only in the Linux CI image.
func hasFileCapability(string) bool { return false }
