package stack

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/config"
)

func TestSettledService(t *testing.T) {
	running := serviceContainer{running: true, configHash: "hash-a"}
	for _, test := range []struct {
		name       string
		expected   string
		containers []serviceContainer
		want       bool
	}{
		{"running with the expected hash", "hash-a", []serviceContainer{running}, true},
		{"changed configuration", "hash-b", []serviceContainer{running}, false},
		{"stopped container", "hash-a", []serviceContainer{{running: false, configHash: "hash-a"}}, false},
		{"no container", "hash-a", nil, false},
		{"two containers", "hash-a", []serviceContainer{running, running}, false},
		{"no hash label", "hash-a", []serviceContainer{{running: true}}, false},
		{"no expected hash", "", []serviceContainer{{running: true}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := settledService(test.expected, test.containers); got != test.want {
				t.Fatalf("settledService = %t, want %t", got, test.want)
			}
		})
	}
}

// TestStartServicesKeepsSettledContainers runs startServices against fake
// python3 (docker compose through tools/bootstrap.py) and docker programs on
// PATH. The fakes record each compose up call. A service whose running
// container has the expected config hash must go through up --no-recreate.
// Every other service must go through the normal up.
func TestStartServicesKeepsSettledContainers(t *testing.T) {
	for _, test := range []struct {
		name       string
		wait       bool
		services   []string
		hashes     map[string]string
		containers map[string][]string // service -> "running hash" of each container
		want       []string
	}{
		{
			name:       "a settled service keeps its container",
			wait:       true,
			services:   []string{"postgres"},
			hashes:     map[string]string{"postgres": "hash-postgres"},
			containers: map[string][]string{"postgres": {"true hash-postgres"}},
			want:       []string{"up -d --wait --no-recreate postgres"},
		},
		{
			name:       "a changed configuration goes through the normal up",
			wait:       true,
			services:   []string{"postgres"},
			hashes:     map[string]string{"postgres": "hash-new"},
			containers: map[string][]string{"postgres": {"true hash-old"}},
			want:       []string{"up -d --wait postgres"},
		},
		{
			name:       "a stopped container goes through the normal up",
			services:   []string{"temporal"},
			hashes:     map[string]string{"temporal": "hash-temporal"},
			containers: map[string][]string{"temporal": {"false hash-temporal"}},
			want:       []string{"up -d temporal"},
		},
		{
			name:     "a group is split per service",
			services: []string{"seaweed-s3-a", "seaweed-s3-b", "caddy"},
			hashes: map[string]string{
				"seaweed-s3-a": "hash-a", "seaweed-s3-b": "hash-b-new", "caddy": "hash-caddy",
			},
			containers: map[string][]string{
				"seaweed-s3-a": {"true hash-a"},
				"seaweed-s3-b": {"true hash-b-old"},
			},
			want: []string{"up -d seaweed-s3-b caddy", "up -d --no-recreate seaweed-s3-a"},
		},
		{
			name:       "two containers of one service go through the normal up",
			services:   []string{"seaweed-master"},
			hashes:     map[string]string{"seaweed-master": "hash-master"},
			containers: map[string][]string{"seaweed-master": {"true hash-master", "true hash-master"}},
			want:       []string{"up -d seaweed-master"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := installFakeEngine(t, test.hashes, test.containers)
			rt := &runtime{root: fake, project: "pm-no-recreate-test", stdout: io.Discard, stderr: io.Discard}
			if err := rt.startServices(context.Background(), noRecreateTestConfiguration(t), test.wait, test.services...); err != nil {
				t.Fatalf("startServices: %v", err)
			}
			if got := recordedUpCalls(t, fake); !slices.Equal(got, test.want) {
				t.Fatalf("compose up calls = %q, want %q", got, test.want)
			}
		})
	}
}

// TestStartServicesBoundsTheHashCommand runs startServices against a fake
// compose config --hash call that never ends. A child of the fake holds the
// output pipe of the call. The call must end at its own deadline when the
// caller set none, and at a cancellation of the caller. Each case expects an
// error, no compose up call and no process of the call that holds its output
// pipe (no exec.ErrWaitDelay). Each wait has one time limit. No sleep, no
// retry.
func TestStartServicesBoundsTheHashCommand(t *testing.T) {
	services := map[string]string{"postgres": "hash-postgres"}
	containers := map[string][]string{"postgres": {"true hash-postgres"}}

	t.Run("a stalled hash call ends at its deadline", func(t *testing.T) {
		fake := installFakeEngine(t, services, containers)
		makeStallFIFO(t, fake, "stall")
		previous := composeHashTimeout
		composeHashTimeout = 200 * time.Millisecond
		t.Cleanup(func() { composeHashTimeout = previous })
		rt := &runtime{root: fake, project: "pm-no-recreate-test", stdout: io.Discard, stderr: io.Discard}

		err := awaitStartServices(t, rt, context.Background())
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, exec.ErrWaitDelay) {
			t.Fatalf("startServices with a stalled hash call = %v; want the deadline and no held output pipe", err)
		}
		assertNoComposeCall(t, fake)
	})

	t.Run("a cancelled caller ends the hash call", func(t *testing.T) {
		fake := installFakeEngine(t, services, containers)
		makeStallFIFO(t, fake, "stall")
		started := makeStallFIFO(t, fake, "started")
		rt := &runtime{root: fake, project: "pm-no-recreate-test", stdout: io.Discard, stderr: io.Discard}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		results := make(chan error, 1)
		go func() { results <- rt.startServices(ctx, noRecreateTestConfiguration(t), true, "postgres") }()
		awaitFIFOLine(t, started)
		cancel()
		var err error
		select {
		case err = <-results:
		case <-time.After(hashTestLimit):
			t.Fatalf("startServices did not return within %s after the cancellation", hashTestLimit)
		}
		if !errors.Is(err, context.Canceled) || errors.Is(err, exec.ErrWaitDelay) {
			t.Fatalf("startServices after a cancellation = %v; want the cancellation and no held output pipe", err)
		}
		assertNoComposeCall(t, fake)
	})
}

// hashTestLimit is the one time limit of each wait in the hash call checks.
const hashTestLimit = 20 * time.Second

// awaitStartServices runs startServices once and waits once for its result.
func awaitStartServices(t *testing.T, rt *runtime, ctx context.Context) error {
	t.Helper()
	results := make(chan error, 1)
	go func() { results <- rt.startServices(ctx, noRecreateTestConfiguration(t), true, "postgres") }()
	select {
	case err := <-results:
		return err
	case <-time.After(hashTestLimit):
		t.Fatalf("startServices did not return within %s", hashTestLimit)
	}
	return nil
}

// makeStallFIFO makes a named pipe in the fake engine directory. The cleanup
// opens it once for writing without a wait, so a fake process that still
// reads it ends when the test ends.
func makeStallFIFO(t *testing.T, directory, name string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if writer, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = writer.Close()
		}
	})
	return path
}

// awaitFIFOLine waits once, with one limit, for one line on the named pipe.
func awaitFIFOLine(t *testing.T, path string) {
	t.Helper()
	lines := make(chan error, 1)
	go func() {
		reader, err := os.Open(path)
		if err != nil {
			lines <- err
			return
		}
		defer reader.Close()
		_, err = bufio.NewReader(reader).ReadString('\n')
		lines <- err
	}()
	select {
	case err := <-lines:
		if err != nil {
			t.Fatalf("read the start line of the fake hash call: %v", err)
		}
	case <-time.After(hashTestLimit):
		t.Fatalf("the fake hash call did not start within %s", hashTestLimit)
	}
}

// assertNoComposeCall checks that the fake recorded no compose call other
// than the hash call.
func assertNoComposeCall(t *testing.T, directory string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(directory, "compose.log")); !errors.Is(err, os.ErrNotExist) {
		contents, _ := os.ReadFile(filepath.Join(directory, "compose.log"))
		t.Fatalf("compose calls after a failed hash call = %q (stat error %v); want none", contents, err)
	}
}

// installFakeEngine writes fake python3 and docker programs and puts them
// first on PATH. It returns the directory that holds their data and log.
func installFakeEngine(t *testing.T, hashes map[string]string, containers map[string][]string) string {
	t.Helper()
	directory := t.TempDir()
	bin := filepath.Join(directory, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	var hashLines strings.Builder
	for service, hash := range hashes {
		hashLines.WriteString(service + " " + hash + "\n")
	}
	writeTestFile(t, filepath.Join(directory, "hashes"), hashLines.String(), 0o600)
	for service, states := range containers {
		var ids strings.Builder
		for index, state := range states {
			id := service + "-container-" + string(rune('a'+index))
			ids.WriteString(id + "\n")
			writeTestFile(t, filepath.Join(directory, "inspect-"+id), state+"\n", 0o600)
		}
		writeTestFile(t, filepath.Join(directory, "ps-"+service), ids.String(), 0o600)
	}
	writeTestFile(t, filepath.Join(bin, "python3"), `#!/bin/sh
for argument in "$@"; do
  if [ "$argument" = "--hash" ]; then
    if [ -p "$FAKE_ENGINE_DIR/stall" ]; then
      # A child holds the output pipe of this call, as a helper process of a
      # real Compose call can. Both wait for a writer that never comes.
      sh -c 'read -r line < "$FAKE_ENGINE_DIR/stall"' &
      if [ -p "$FAKE_ENGINE_DIR/started" ]; then
        printf 'started\n' > "$FAKE_ENGINE_DIR/started"
      fi
      read -r line < "$FAKE_ENGINE_DIR/stall"
      exit 0
    fi
    cat "$FAKE_ENGINE_DIR/hashes"
    exit 0
  fi
done
printf '%s\n' "$*" >> "$FAKE_ENGINE_DIR/compose.log"
`, 0o700)
	writeTestFile(t, filepath.Join(bin, "docker"), `#!/bin/sh
case "$1" in
  info)
    printf '%s\n' '["name=seccomp,profile=builtin"]'
    ;;
  ps)
    for argument in "$@"; do
      case "$argument" in
        label=com.docker.compose.service=*)
          service=${argument#label=com.docker.compose.service=}
          ;;
      esac
    done
    if [ -f "$FAKE_ENGINE_DIR/ps-$service" ]; then
      cat "$FAKE_ENGINE_DIR/ps-$service"
    fi
    ;;
  inspect)
    for argument in "$@"; do
      id=$argument
    done
    cat "$FAKE_ENGINE_DIR/inspect-$id"
    ;;
  *)
    exit 64
    ;;
esac
`, 0o700)
	t.Setenv("FAKE_ENGINE_DIR", directory)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
	return directory
}

// recordedUpCalls returns each recorded compose call from its up argument.
func recordedUpCalls(t *testing.T, directory string) []string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(directory, "compose.log"))
	if err != nil {
		t.Fatalf("read the recorded compose calls: %v", err)
	}
	var calls []string
	for _, line := range strings.Split(strings.TrimSpace(string(contents)), "\n") {
		index := strings.Index(line, " up ")
		if index < 0 {
			t.Fatalf("recorded compose call has no up argument: %q", line)
		}
		calls = append(calls, line[index+1:])
	}
	return calls
}

func writeTestFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

// noRecreateTestConfiguration returns a valid configuration in which every
// dependency is bundled. The values follow deploy/server.example.json.
func noRecreateTestConfiguration(t *testing.T) config.Configuration {
	t.Helper()
	loaded, err := config.Load(strings.NewReader(noRecreateTestConfigurationDocument))
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := config.Validate(loaded, config.Prelaunch{})
	if err != nil {
		t.Fatal(err)
	}
	return configuration
}

const noRecreateTestConfigurationDocument = `{
  "schema_version": "1",
  "deployment_id": "pm-development",
  "public_origin": "https://pm.example.invalid",
  "postgres": {
    "ownership": "bundled",
    "database": "pm_app",
    "endpoint_ref": "secret://operator/pm-development-postgres-dsn"
  },
  "temporal": {
    "ownership": "bundled",
    "endpoint": "temporal:7233",
    "namespace": "pm-development",
    "client_identity_ref": "secret://operator/pm-development-temporal-mtls",
    "retention": "720h",
    "task_queues": ["pm-control", "pm-read", "pm-effect", "pm-binary-query"]
  },
  "artifacts": {
    "ownership": "bundled",
    "driver": "s3",
    "endpoint": "https://objects.internal.invalid",
    "bucket": "pm-development-artifacts",
    "credential_ref": "secret://operator/pm-development-object-access",
    "attempt_key_policy": "unique_immutable"
  },
  "identity": {
    "issuer": "https://identity.example.invalid",
    "client_id": "pm-server",
    "client_secret_ref": "secret://operator/pm-development-oidc"
  },
  "encryption": {
    "wrapping_key_ref": "secret://operator/pm-development-wrap"
  },
  "admission": {
    "profile": "development"
  },
  "limits": {
    "workspace_active_runs": 4,
    "workflow_payload_bytes": 65536,
    "workflow_fanout": 32,
    "run_lifetime": "24h",
    "event_buffer": 128,
    "query_children": 1,
    "query_child_memory_bytes": 4294967296,
    "query_child_deadline": "5m",
    "request_bytes": 1048576,
    "metadata_deadline": "5s"
  }
}`
