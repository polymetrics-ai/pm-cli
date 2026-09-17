package server

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

// operatorSecrets uses the existing secret://authority/path bundle layout.
// Reads are descriptor-bound and confined even when a pathname is replaced.
// Opening a FIFO cannot block startup before its file type is checked.
type operatorSecrets struct {
	root *os.Root
}

func openOperatorSecrets(directory string) (*operatorSecrets, error) {
	if directory == "" {
		return nil, errors.New("operator secret directory is required")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errors.New("operator secret directory is unavailable")
	}
	return &operatorSecrets{root: root}, nil
}

func (secrets *operatorSecrets) Close() error {
	return secrets.root.Close()
}

func (secrets *operatorSecrets) read(reference, member string) ([]byte, error) {
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

func (secrets *operatorSecrets) text(reference, member string) (string, error) {
	value, err := secrets.read(reference, member)
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
