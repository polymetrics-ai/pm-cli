package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
)

// reporter prints one line for each check. Every line passes through the
// secret registry first, so a registered value never reaches the job log. The
// reporter also keeps a copy of everything it printed, for the final leak scan.
type reporter struct {
	mu        sync.Mutex
	secrets   [][]byte
	printed   bytes.Buffer
	pass      int
	fail      int
	scrubHits int
}

var report = &reporter{}

// secret registers a value that must never be printed. It also registers the
// encodings that a leak is likely to use.
func (r *reporter) secret(value string) {
	if len(value) < 6 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, form := range secretForms([]byte(value)) {
		r.secrets = append(r.secrets, form)
	}
}

func secretForms(value []byte) [][]byte {
	return [][]byte{
		append([]byte(nil), value...),
		[]byte(base64.StdEncoding.EncodeToString(value)),
		[]byte(strings.TrimRight(base64.StdEncoding.EncodeToString(value), "=")),
		[]byte(base64.RawURLEncoding.EncodeToString(value)),
		[]byte(url.QueryEscape(string(value))),
	}
}

func (r *reporter) scrub(line string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []byte(line)
	for _, form := range r.secrets {
		if len(form) >= 6 && bytes.Contains(out, form) {
			r.scrubHits++
			out = bytes.ReplaceAll(out, form, []byte("[REDACTED]"))
		}
	}
	return string(out)
}

func (r *reporter) emit(kind, id, format string, args ...any) {
	line := fmt.Sprintf("LOGIN-SPIKE %s %s %s", kind, id, fmt.Sprintf(format, args...))
	line = strings.ReplaceAll(line, "\n", " ")
	line = r.scrub(line)
	r.mu.Lock()
	r.printed.WriteString(line)
	r.printed.WriteByte('\n')
	switch kind {
	case "PASS":
		r.pass++
	case "FAIL":
		r.fail++
	}
	r.mu.Unlock()
	fmt.Fprintln(os.Stdout, line)
}

func (r *reporter) PASS(id, format string, args ...any) { r.emit("PASS", id, format, args...) }
func (r *reporter) FAIL(id, format string, args ...any) { r.emit("FAIL", id, format, args...) }
func (r *reporter) INFO(id, format string, args ...any) { r.emit("INFO", id, format, args...) }

// check prints PASS when ok is true and FAIL otherwise.
func (r *reporter) check(id string, ok bool, format string, args ...any) bool {
	if ok {
		r.PASS(id, format, args...)
	} else {
		r.FAIL(id, format, args...)
	}
	return ok
}

// tail prints at most max lines of a diagnostic file, after redaction. Use it
// only on a failure path.
func (r *reporter) tail(id, path string, max int) {
	contents, err := os.ReadFile(path)
	if err != nil {
		r.INFO(id, "diagnostic file unavailable")
		return
	}
	lines := strings.Split(strings.TrimRight(string(contents), "\n"), "\n")
	if len(lines) > max {
		lines = lines[len(lines)-max:]
	}
	for _, line := range lines {
		if len(line) > 400 {
			line = line[:400] + "..."
		}
		r.INFO(id, "| %s", line)
	}
}

func (r *reporter) printedCopy() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.printed.Bytes()...)
}

func shortDigest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])[:16]
}

// containsAny reports whether data holds the value in one of its likely
// encodings.
func containsAny(data, value []byte) bool {
	for _, form := range secretForms(value) {
		if len(form) >= 6 && bytes.Contains(data, form) {
			return true
		}
	}
	return false
}
