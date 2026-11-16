//go:build linux

package schema

import "fmt"

// vetCanary has a go vet printf finding on purpose: %d gets a string. It
// shows that the `vet` check of the CI `checks` job fails. The file builds
// only on Linux, so the macOS git hooks do not stop the canary push. Issue
// #307. This branch is never merged.
func vetCanary() string {
	return fmt.Sprintf("%d", "canary")
}
