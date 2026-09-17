package credentials

import (
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const maximumOperatorSecretBytes = 64 << 10

// OperatorSecrets confines descriptor-bound secret:// bundle reads to one
// already-open operator directory. Opening a FIFO cannot block startup before
// its file type is checked.
type OperatorSecrets struct {
	root *os.Root
}

// OpenOperatorSecrets opens and retains the exact directory identity used by
// all subsequent reads. Replacing its pathname cannot replace the opened root.
func OpenOperatorSecrets(directory string) (*OperatorSecrets, error) {
	if directory == "" {
		return nil, errors.New("operator secret directory is required")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errors.New("operator secret directory is unavailable")
	}
	return &OperatorSecrets{root: root}, nil
}

// Close releases the retained directory identity.
func (secrets *OperatorSecrets) Close() error {
	if secrets == nil || secrets.root == nil {
		return nil
	}
	return secrets.root.Close()
}

// Read returns the exact bounded bytes in member of the referenced bundle.
// Ownership of the returned bytes transfers to the caller.
func (secrets *OperatorSecrets) Read(reference, member string) ([]byte, error) {
	if secrets == nil || secrets.root == nil {
		return nil, errors.New("operator secret directory is unavailable")
	}
	parsed, err := url.Parse(reference)
	if err != nil || parsed.Scheme != "secret" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("operator secret reference is invalid")
	}
	if member == "" || member == "." || member == ".." || strings.ContainsAny(member, "/\\\x00") {
		return nil, errors.New("operator secret member is invalid")
	}
	relative := filepath.FromSlash(parsed.Host + "/" + strings.TrimPrefix(parsed.Path, "/"))
	bundle, err := secrets.root.OpenRoot(relative)
	if err != nil {
		return nil, errors.New("operator secret bundle is unavailable")
	}
	defer bundle.Close()
	file, err := bundle.OpenFile(member, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("operator secret member is unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > maximumOperatorSecretBytes {
		return nil, errors.New("operator secret member must be a bounded regular file")
	}
	value, err := io.ReadAll(io.LimitReader(file, maximumOperatorSecretBytes+1))
	if err != nil || len(value) == 0 || len(value) > maximumOperatorSecretBytes {
		clear(value)
		return nil, errors.New("operator secret read failed")
	}
	return value, nil
}

// Text reads one secret member as a nonempty single-line value.
func (secrets *OperatorSecrets) Text(reference, member string) (string, error) {
	value, err := secrets.Read(reference, member)
	if err != nil {
		return "", err
	}
	defer clear(value)
	text := strings.TrimSpace(string(value))
	if text == "" || strings.ContainsAny(text, "\x00\r\n") {
		return "", errors.New("operator secret text is invalid")
	}
	return text, nil
}

func (*OperatorSecrets) String() string { return "operator secret bundle [REDACTED]" }

func (secrets *OperatorSecrets) GoString() string { return secrets.String() }
