package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunRefusesExistingOrSymlinkedRootsWithoutClobbering(t *testing.T) {
	repositoryRoot := realTemporaryDirectory(t)
	writeExampleConfiguration(t, repositoryRoot)

	t.Run("existing root", func(t *testing.T) {
		parent := realTemporaryDirectory(t)
		root := filepath.Join(parent, "fixture")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatalf("create existing fixture root: %v", err)
		}
		sentinel := filepath.Join(root, "sentinel")
		if err := os.WriteFile(sentinel, []byte("unchanged\n"), 0o600); err != nil {
			t.Fatalf("write fixture sentinel: %v", err)
		}

		var stdout, stderr bytes.Buffer
		status := run([]string{
			"--root", root,
			"--project", "pm-fixture-test",
			"--repo-root", repositoryRoot,
		}, &stdout, &stderr)
		if status == 0 {
			t.Fatal("fixture generation accepted an existing root")
		}
		if stdout.Len() != 0 {
			t.Fatal("fixture generation wrote an environment for an existing root")
		}
		contents, err := os.ReadFile(sentinel)
		if err != nil || string(contents) != "unchanged\n" {
			t.Fatalf("existing fixture contents changed: %v", err)
		}
		for _, name := range []string{"environment.json", "server.json", "secrets"} {
			if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
				t.Fatalf("existing fixture root gained %q", name)
			}
		}
	})

	t.Run("symlinked ancestor", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation is not generally available to unprivileged Windows tests")
		}
		targetParent := realTemporaryDirectory(t)
		linkParent := realTemporaryDirectory(t)
		linked := filepath.Join(linkParent, "linked")
		if err := os.Symlink(targetParent, linked); err != nil {
			t.Fatalf("create symlinked fixture ancestor: %v", err)
		}
		root := filepath.Join(linked, "fixture")

		var stdout, stderr bytes.Buffer
		status := run([]string{
			"--root", root,
			"--project", "pm-fixture-test",
			"--repo-root", repositoryRoot,
		}, &stdout, &stderr)
		if status == 0 {
			t.Fatal("fixture generation accepted a symlinked ancestor")
		}
		if stdout.Len() != 0 {
			t.Fatal("fixture generation wrote an environment through a symlinked ancestor")
		}
		if _, err := os.Lstat(filepath.Join(targetParent, "fixture")); !os.IsNotExist(err) {
			t.Fatal("fixture generation touched the symlink target")
		}
	})
}

func TestGeneratedTemporalRolloverProfileIsAuxiliaryAndOwned(t *testing.T) {
	repositoryRoot := realTemporaryDirectory(t)
	writeExampleConfiguration(t, repositoryRoot)
	root := filepath.Join(realTemporaryDirectory(t), "fixture")
	var stdout, stderr bytes.Buffer
	if status := run([]string{
		"--root", root,
		"--project", "pm-fixture-test",
		"--repo-root", repositoryRoot,
	}, &stdout, &stderr); status != 0 {
		t.Fatalf("generate fixture status = %d: %s", status, stderr.String())
	}
	var environment map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &environment); err != nil {
		t.Fatalf("decode generated environment: %v", err)
	}
	profilePath := filepath.Join(root, "temporal-dynamicconfig.yaml")
	if environment["PM_STACK_TEMPORAL_DYNAMIC_CONFIG"] != profilePath {
		t.Fatalf("dynamic config path = %q; want %q", environment["PM_STACK_TEMPORAL_DYNAMIC_CONFIG"], profilePath)
	}
	auxiliary := "pm-fixture-test-rollover"
	if environment["PM_STACK_TEST_ROLLOVER_NAMESPACE"] != auxiliary {
		t.Fatalf("rollover namespace = %q; want %q", environment["PM_STACK_TEST_ROLLOVER_NAMESPACE"], auxiliary)
	}
	info, err := os.Lstat(profilePath)
	if err != nil {
		t.Fatalf("inspect generated Temporal profile: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("generated Temporal profile mode = %v; want regular 0600", info.Mode())
	}
	profile, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("read generated Temporal profile: %v", err)
	}
	text := string(profile)
	var (
		constraintCount int
		constraintValue int
		namespace       string
	)
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		if strings.TrimSpace(line) != "limit.historyCount.suggestContinueAsNew:" {
			continue
		}
		constraintCount++
		for _, member := range lines[index+1:] {
			if member != "" && member[0] != ' ' {
				break
			}
			trimmed := strings.TrimSpace(member)
			switch {
			case strings.HasPrefix(trimmed, "- value:"):
				raw := strings.TrimSpace(strings.TrimPrefix(trimmed, "- value:"))
				if err := json.Unmarshal([]byte(raw), &constraintValue); err != nil {
					t.Fatalf("decode generated Temporal rollover value %q: %v", raw, err)
				}
			case strings.HasPrefix(trimmed, "namespace:"):
				raw := strings.TrimSpace(strings.TrimPrefix(trimmed, "namespace:"))
				switch {
				case strings.HasPrefix(raw, `"`):
					if err := json.Unmarshal([]byte(raw), &namespace); err != nil {
						t.Fatalf("decode generated Temporal rollover namespace %q: %v", raw, err)
					}
				case len(raw) >= 2 && raw[0] == '\'' && raw[len(raw)-1] == '\'':
					namespace = strings.ReplaceAll(raw[1:len(raw)-1], "''", "'")
				default:
					namespace = raw
				}
			}
		}
	}
	if !strings.Contains(text, "system.enableDeployments:") ||
		constraintCount != 1 ||
		constraintValue != 64 ||
		namespace != auxiliary {
		t.Fatalf("generated Temporal profile did not preserve base settings plus the exact auxiliary constraint: %s", text)
	}
	if namespace == "pm-fixture-test" {
		t.Fatal("generated Temporal rollover threshold also constrained the primary namespace")
	}
}

func TestGeneratedArtifactExecutionConfigurationIsExplicitAndBounded(t *testing.T) {
	repositoryRoot := realTemporaryDirectory(t)
	writeExampleConfiguration(t, repositoryRoot)
	example, err := os.ReadFile(filepath.Join(repositoryRoot, "deploy", "server.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := selectConfiguration(example, "pm-fixture-test", map[string]string{
		"PUBLIC":   "8443",
		"TEMPORAL": "7233",
	})
	if err != nil {
		t.Fatal(err)
	}
	var selected struct {
		Artifacts struct {
			Execution struct {
				Region                       string `json:"region"`
				MaxObjectBytes               uint64 `json:"max_object_bytes"`
				MaxInFlight                  uint64 `json:"max_in_flight"`
				RequestTimeout               string `json:"request_timeout"`
				OrganizationReservedBytes    uint64 `json:"organization_reserved_bytes"`
				WorkspaceReservedBytes       uint64 `json:"workspace_reserved_bytes"`
				OrganizationReservedAttempts uint64 `json:"organization_reserved_attempts"`
				WorkspaceReservedAttempts    uint64 `json:"workspace_reserved_attempts"`
			} `json:"execution"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(configuration, &selected); err != nil {
		t.Fatal(err)
	}
	execution := selected.Artifacts.Execution
	if execution.Region != "us-east-1" ||
		execution.MaxObjectBytes != 16777216 ||
		execution.MaxInFlight != 4 ||
		execution.RequestTimeout != "30s" ||
		execution.OrganizationReservedBytes != 1073741824 ||
		execution.WorkspaceReservedBytes != 268435456 ||
		execution.OrganizationReservedAttempts != 4096 ||
		execution.WorkspaceReservedAttempts != 1024 {
		t.Fatalf("generated artifact execution configuration = %#v", execution)
	}
}

func TestRepositoryReaderRefusesSymlinksAndFIFOsWithoutBlocking(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO and symlink controls are POSIX-only")
	}
	root := realTemporaryDirectory(t)
	if err := os.WriteFile(filepath.Join(root, "regular"), []byte("selected\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("regular", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := readRepositoryRegularFile(root, "link", 1024); err == nil {
		t.Fatal("repository reader followed a symlink")
	}
	if err := os.Mkdir(filepath.Join(root, "actual-directory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "actual-directory", "member"),
		[]byte("selected\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("actual-directory", filepath.Join(root, "linked-directory")); err != nil {
		t.Fatal(err)
	}
	if _, err := readRepositoryRegularFile(root, filepath.Join("linked-directory", "member"), 1024); err == nil {
		t.Fatal("repository reader followed a symlink while walking a member path")
	}
	if err := syscall.Mkfifo(filepath.Join(root, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	returned := make(chan error, 1)
	go func() {
		_, err := readRepositoryRegularFile(root, "fifo", 1024)
		returned <- err
	}()
	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("repository reader accepted a FIFO")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("repository reader blocked opening a FIFO")
	}
}

func TestGeneratedCertificateBundlesVerifyProtocolIdentities(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	testCases := []struct {
		name              string
		realm             string
		includeClient     bool
		serverNames       []string
		handshakeName     string
		clientCertificate string
	}{
		{
			name:          "PostgreSQL",
			realm:         "postgres",
			serverNames:   []string{"127.0.0.1", "postgres"},
			handshakeName: "127.0.0.1",
		},
		{
			name:              "Temporal mutual TLS",
			realm:             "temporal",
			includeClient:     true,
			serverNames:       []string{"127.0.0.1", "temporal.pm-stack.internal"},
			handshakeName:     "temporal.pm-stack.internal",
			clientCertificate: "client",
		},
		{
			name:              "artifact server and internal client",
			realm:             "seaweed-s3",
			serverNames:       []string{"127.0.0.1", "seaweed-s3-a", "seaweed-s3-b", "seaweed-s3.pm-stack.internal"},
			handshakeName:     "seaweed-s3.pm-stack.internal",
			clientCertificate: "server",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			bundle, err := generateCertificateBundle(testCase.realm, "pm-fixture-test", testCase.includeClient, now)
			if err != nil {
				t.Fatalf("generate certificate bundle: %v", err)
			}
			roots := x509.NewCertPool()
			if !roots.AppendCertsFromPEM(bundle["ca.pem"]) {
				t.Fatal("generated CA is not valid PEM")
			}
			serverPair, err := tls.X509KeyPair(bundle["server.pem"], bundle["server-key.pem"])
			if err != nil {
				t.Fatalf("load generated server identity: %v", err)
			}
			serverLeaf, err := x509.ParseCertificate(serverPair.Certificate[0])
			if err != nil {
				t.Fatalf("parse generated server certificate: %v", err)
			}
			for _, serverName := range testCase.serverNames {
				if _, err := serverLeaf.Verify(x509.VerifyOptions{
					Roots:       roots,
					DNSName:     serverName,
					CurrentTime: now,
					KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
				}); err != nil {
					t.Fatalf("verify generated server identity %q: %v", serverName, err)
				}
			}

			var clientPair *tls.Certificate
			switch testCase.clientCertificate {
			case "":
			case "client":
				pair, err := tls.X509KeyPair(bundle["client.pem"], bundle["client-key.pem"])
				if err != nil {
					t.Fatalf("load generated client identity: %v", err)
				}
				clientPair = &pair
			case "server":
				clientPair = &serverPair
			default:
				t.Fatalf("unsupported test client identity %q", testCase.clientCertificate)
			}
			if clientPair != nil {
				clientLeaf, err := x509.ParseCertificate(clientPair.Certificate[0])
				if err != nil {
					t.Fatalf("parse generated client certificate: %v", err)
				}
				if _, err := clientLeaf.Verify(x509.VerifyOptions{
					Roots:       roots,
					CurrentTime: now,
					KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
				}); err != nil {
					t.Fatalf("verify generated client identity: %v", err)
				}
			}
			verifyTLSHandshake(t, roots, serverPair, clientPair, testCase.handshakeName, now)
		})
	}
}

func verifyTLSHandshake(
	t *testing.T,
	roots *x509.CertPool,
	serverCertificate tls.Certificate,
	clientCertificate *tls.Certificate,
	serverName string,
	now time.Time,
) {
	t.Helper()
	serverConnection, clientConnection := net.Pipe()
	deadline := time.Now().Add(5 * time.Second)
	if err := serverConnection.SetDeadline(deadline); err != nil {
		t.Fatalf("set server handshake deadline: %v", err)
	}
	if err := clientConnection.SetDeadline(deadline); err != nil {
		t.Fatalf("set client handshake deadline: %v", err)
	}

	serverConfig := &tls.Config{
		Certificates: []tls.Certificate{serverCertificate},
		MinVersion:   tls.VersionTLS12,
		Time:         func() time.Time { return now },
	}
	if clientCertificate != nil {
		serverConfig.ClientAuth = tls.RequireAndVerifyClientCert
		serverConfig.ClientCAs = roots
	}
	clientConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: serverName,
		Time:       func() time.Time { return now },
	}
	if clientCertificate != nil {
		clientConfig.Certificates = []tls.Certificate{*clientCertificate}
	}

	serverTLS := tls.Server(serverConnection, serverConfig)
	clientTLS := tls.Client(clientConnection, clientConfig)
	serverResult := make(chan error, 1)
	go func() {
		serverResult <- serverTLS.Handshake()
		_ = serverConnection.Close()
	}()
	clientErr := clientTLS.Handshake()
	_ = clientConnection.Close()
	serverErr := <-serverResult
	if clientErr != nil || serverErr != nil {
		t.Fatalf("generated identities failed an actual TLS handshake: client=%v server=%v", clientErr, serverErr)
	}
}

func realTemporaryDirectory(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temporary directory: %v", err)
	}
	return path
}

func writeExampleConfiguration(t *testing.T, repositoryRoot string) {
	t.Helper()
	deploy := filepath.Join(repositoryRoot, "deploy")
	if err := os.Mkdir(deploy, 0o700); err != nil {
		t.Fatalf("create test deployment directory: %v", err)
	}
	if err := os.Mkdir(filepath.Join(deploy, "temporal"), 0o700); err != nil {
		t.Fatalf("create test Temporal deployment directory: %v", err)
	}
	contents := []byte(`{
  "schema_version": "1",
  "deployment_id": "pm-development",
  "public_origin": "https://pm.example.invalid",
  "postgres": {
    "ownership": "bundled",
    "database": "pm_app",
    "endpoint_ref": "secret://operator/example-postgres"
  },
  "temporal": {
    "ownership": "bundled",
    "endpoint": "temporal:7233",
    "namespace": "pm-development",
    "client_identity_ref": "secret://operator/example-temporal"
  },
  "artifacts": {
    "ownership": "bundled",
    "endpoint": "https://objects.example.invalid",
    "bucket": "pm-development-artifacts",
    "credential_ref": "secret://operator/example-artifacts"
  }
}
`)
	if err := os.WriteFile(filepath.Join(deploy, "server.example.json"), contents, 0o600); err != nil {
		t.Fatalf("write test server example: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(deploy, "temporal", "dynamicconfig.yaml"),
		[]byte("system.enableDeployments:\n  - value: true\n    constraints: {}\n"),
		0o600,
	); err != nil {
		t.Fatalf("write test Temporal dynamic config: %v", err)
	}
}
