package warehouse

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BoundScratch is one private, disposable per-attempt directory for the
// restricted child. It is owner-only, never shared with another attempt, and
// removed when the attempt completes or is refused. The child is pointed at it
// through TMPDIR and HOME, so its engine scratch and any spill stay inside it;
// no shared writable database file is ever used.
type BoundScratch struct {
	path string
}

// NewBoundScratch creates one owner-only scratch directory below the configured
// root. The root must already exist as an owner-only directory that is not a
// symlink: the supervisor never creates or widens a shared directory.
func NewBoundScratch(root string) (*BoundScratch, error) {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, errors.New("the scratch root must be one absolute, cleaned path")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect the scratch root: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("the scratch root must not be a symlink")
	}
	if !info.IsDir() {
		return nil, errors.New("the scratch root must be a directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("the scratch root must be owner-only, found mode %#o", info.Mode().Perm())
	}
	path, err := os.MkdirTemp(root, "pm-query-")
	if err != nil {
		return nil, fmt.Errorf("create the per-attempt scratch directory: %w", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		_ = os.RemoveAll(path)
		return nil, fmt.Errorf("restrict the per-attempt scratch directory: %w", err)
	}
	created, err := os.Lstat(path)
	if err != nil {
		_ = os.RemoveAll(path)
		return nil, fmt.Errorf("inspect the per-attempt scratch directory: %w", err)
	}
	if !created.IsDir() || created.Mode().Perm()&0o077 != 0 {
		_ = os.RemoveAll(path)
		return nil, fmt.Errorf("the per-attempt scratch directory is not owner-only, found mode %#o", created.Mode().Perm())
	}
	return &BoundScratch{path: path}, nil
}

// Path reports the per-attempt directory. It is only valid until Close.
func (scratch *BoundScratch) Path() string {
	if scratch == nil {
		return ""
	}
	return scratch.path
}

// String is a redacted description: a scratch path is local execution state and
// never durable evidence.
func (scratch *BoundScratch) String() string { return "bound analytical scratch [REDACTED]" }

// GoString keeps the redaction on the %#v path.
func (scratch *BoundScratch) GoString() string { return scratch.String() }

// Close removes the per-attempt directory, including any engine spill the child
// left behind. It is idempotent and reports the first removal failure.
func (scratch *BoundScratch) Close() error {
	if scratch == nil || scratch.path == "" {
		return nil
	}
	path := scratch.path
	scratch.path = ""
	if !strings.HasPrefix(filepath.Base(path), "pm-query-") {
		return errors.New("refusing to remove a directory this supervisor did not create")
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove the per-attempt scratch directory: %w", err)
	}
	return nil
}
