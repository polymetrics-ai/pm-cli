package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
)

func TestControlsPresenceAndPrecedence(t *testing.T) {
	directory := t.TempDir()
	profilesPath := filepath.Join(directory, "profiles.json")
	profileOutput := pmapi.OutputJSON
	profileWait := true
	profileTimeout := 20 * time.Second
	profile := pmapi.ServerProfile{
		Name:           "ada",
		ServerURL:      "https://pm.example.test",
		InstanceID:     "instance-v19",
		SessionHandle:  "session-v19",
		Output:         &profileOutput,
		Wait:           &profileWait,
		RequestTimeout: &profileTimeout,
	}
	if err := pmapi.AddServerProfile(profilesPath, profile); err != nil {
		t.Fatalf("write profile: %v", err)
	}

	environment := map[string]string{
		"PM_SERVER_PROFILE":  "ada",
		"PM_OUTPUT":          "text",
		"PM_WAIT":            "true",
		"PM_REQUEST_TIMEOUT": "10s",
	}
	lookup := func(name string) (string, bool) { value, ok := environment[name]; return value, ok }
	flags := &rootFlags{}
	flags.json.name = "json"
	flags.wait.name = "wait"
	if err := flags.json.Set("false"); err != nil {
		t.Fatalf("set explicit false json: %v", err)
	}
	if err := flags.wait.Set("false"); err != nil {
		t.Fatalf("set explicit false wait: %v", err)
	}
	runtime := &commandRuntime{options: Options{ProfilesPath: profilesPath, LookupEnv: lookup}, flags: flags}
	resolved, err := runtime.resolveConfig(false)
	if err != nil {
		t.Fatalf("resolve controls: %v", err)
	}
	if resolved.output != pmapi.OutputText || resolved.wait || resolved.requestTimeout != 10*time.Second {
		t.Fatalf("resolved controls = output %q wait %t timeout %s", resolved.output, resolved.wait, resolved.requestTimeout)
	}

	settingsOnly := &commandRuntime{options: Options{ProfilesPath: profilesPath, LookupEnv: func(name string) (string, bool) {
		if name == "PM_SERVER_PROFILE" {
			return "ada", true
		}
		return "", false
	}}, flags: &rootFlags{}}
	resolved, err = settingsOnly.resolveConfig(false)
	if err != nil {
		t.Fatalf("resolve unset flags: %v", err)
	}
	if resolved.output != pmapi.OutputJSON || !resolved.wait || resolved.requestTimeout != 20*time.Second {
		t.Fatalf("unset flags overwrote profile defaults: %#v", resolved)
	}

	var duplicate singleString
	duplicate.name = "output"
	if err := duplicate.Set("json"); err != nil {
		t.Fatalf("first control: %v", err)
	}
	if err := duplicate.Set("text"); err == nil {
		t.Fatal("duplicate control was accepted")
	}
	var empty singleString
	empty.name = "workspace"
	if err := empty.Set(""); err == nil {
		t.Fatal("empty selector was accepted")
	}
	var wrongType singleBool
	wrongType.name = "wait"
	if err := wrongType.Set("1"); err == nil {
		t.Fatal("wrong-type boolean was accepted")
	}

	conflicting := map[string]string{"PM_SERVER_PROFILE": "ada", "PM_OUTPUT": "text", "PM_JSON": "true"}
	conflictRuntime := &commandRuntime{options: Options{ProfilesPath: profilesPath, LookupEnv: func(name string) (string, bool) {
		value, ok := conflicting[name]
		return value, ok
	}}, flags: &rootFlags{}}
	if _, err := conflictRuntime.resolveConfig(false); err == nil {
		t.Fatal("conflicting PM_OUTPUT and PM_JSON were accepted")
	}

	maliciousPath := filepath.Join(directory, "malicious.json")
	malicious := []byte(`{"schema_version":"pm.client-profiles/v1","profiles":{"ada":{"server_url":"https://pm.example.test","instance_id":"instance-v19","session_handle":"session-v19","provider_token":"must-not-load"}}}`)
	if err := os.WriteFile(maliciousPath, malicious, 0o600); err != nil {
		t.Fatalf("write malicious profile: %v", err)
	}
	if _, err := pmapi.LoadServerProfile(maliciousPath, "ada"); err == nil {
		t.Fatal("provider credential field was accepted from settings")
	}

	var stdout, stderr bytes.Buffer
	lookups := 0
	code := Execute(t.Context(), Options{
		Args: []string{"version"}, Stdout: &stdout, Stderr: &stderr, ProfilesPath: filepath.Join(directory, "missing.json"),
		LookupEnv: func(string) (string, bool) { lookups++; return "", false }, Version: "v19-test",
	})
	if code != 0 || stdout.String() != "v19-test\n" || lookups != 0 {
		t.Fatalf("offline version = code %d stdout %q env lookups %d stderr %q", code, stdout.String(), lookups, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Execute(t.Context(), Options{Args: []string{"version", "--unknown"}, Stdout: &stdout, Stderr: &stderr})
	if code != 2 {
		t.Fatalf("unknown control exit = %d, want 2", code)
	}
	if code := pmapi.ExitCodeForProblem(pmapi.DecodedProblem{Value: pmapi.Problem{Status: 412}}); code != 4 {
		t.Fatalf("revision conflict exit = %d, want 4", code)
	}
	if code := pmapi.ExitCodeForProblem(pmapi.DecodedProblem{Value: pmapi.Problem{Status: 503}}); code != 5 {
		t.Fatalf("dependency refusal exit = %d, want 5", code)
	}
}
