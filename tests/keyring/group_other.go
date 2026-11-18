//go:build !linux && !darwin

package keyring

import "errors"

// liveGroupMembers has no observation on this system. The harness supports
// Linux and Darwin.
func liveGroupMembers(int) (int, error) {
	return 0, errors.New("no observation of a process group on this system")
}
