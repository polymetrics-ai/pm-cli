package acceptance

import (
	"fmt"
	"strings"
	"testing"
)

// TestReadJSONOutputRefusesTrailingSyntax is a Go developer check, not
// acceptance proof. It checks readJSONOutput, the helper of the 284c steps
// that reads the one JSON document of a pm command with --output json (Codex
// red review of RED 2, finding F2). The helper must refuse each byte after the
// document other than white space, also a closing bracket or brace. Its error
// must give the byte count of the stdout only, never the stdout.
func TestReadJSONOutputRefusesTrailingSyntax(t *testing.T) {
	const refusal = "holds more than the one JSON document that the step reads"
	for _, stdout := range []string{
		`{"a":1}]`,
		`{"a":1}}`,
		`{"a":1} {"b":2}`,
		`{"a":1} x`,
	} {
		t.Run(fmt.Sprintf("refuses %q", stdout), func(t *testing.T) {
			var target struct {
				A int `json:"a"`
			}
			err := readJSONOutput("pm check", stdout, &target)
			if err == nil {
				t.Fatalf("readJSONOutput accepted %q, want the refusal %q", stdout, refusal)
			}
			message := err.Error()
			if !strings.Contains(message, refusal) {
				t.Fatalf("readJSONOutput refused %q with %q, want the refusal %q", stdout, message, refusal)
			}
			if want := fmt.Sprintf("(%d bytes, not shown)", len(stdout)); !strings.Contains(message, want) {
				t.Fatalf("the refusal %q does not give the byte count %q", message, want)
			}
			if strings.Contains(message, stdout) || strings.Contains(message, `"a"`) {
				t.Fatalf("the refusal %q shows the stdout", message)
			}
		})
	}
	for _, stdout := range []string{`{"a":1}`, "{\"a\":1}\n"} {
		t.Run(fmt.Sprintf("accepts %q", stdout), func(t *testing.T) {
			var target struct {
				A int `json:"a"`
			}
			if err := readJSONOutput("pm check", stdout, &target); err != nil {
				t.Fatalf("readJSONOutput refused %q: %v", stdout, err)
			}
			if target.A != 1 {
				t.Fatalf("readJSONOutput read a = %d from %q, want 1", target.A, stdout)
			}
		})
	}
}
