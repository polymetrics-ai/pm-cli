package clientparity

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/cli"
	"github.com/polymetrics-ai/pm-cli/internal/store"
	"github.com/polymetrics-ai/pm-cli/internal/testfixture"
)

type browserInput struct {
	BaseURL              string                 `json:"base_url"`
	BootstrapNonce       string                 `json:"bootstrap_nonce"`
	InstanceID           string                 `json:"instance_id"`
	WorkspaceID          string                 `json:"workspace_id"`
	ForeignWorkspaceID   string                 `json:"foreign_workspace_id"`
	OperationRevision    string                 `json:"operation_revision"`
	RequestKey           string                 `json:"request_key"`
	InputField           string                 `json:"input_field"`
	Input                map[string]any         `json:"input"`
	FirstText            string                 `json:"first_text"`
	ChangedText          string                 `json:"changed_text"`
	ExpectedRunRef       string                 `json:"expected_run_ref"`
	ExpectedIntentDigest string                 `json:"expected_intent_digest"`
	CanonicalCases       []browserCanonicalCase `json:"canonical_cases"`
}

type browserCanonicalCase struct {
	Value  any    `json:"value"`
	Digest string `json:"digest"`
}

type parityCounters struct {
	admissions atomic.Int64
	cancels    atomic.Int64
	effects    atomic.Int64
}

type parityCLIInput struct {
	Args          []string
	ProfilesPath  string
	InstanceID    string
	SessionHandle string
	Certificate   []byte
	Session       pmapi.PMDeviceSession
}

// The child runs the actual generated Cobra CLI entrypoint. Only its PM session
// store and owned TLS trust root are injected; no request, response, admission,
// provider behavior, parser, or generated command is replaced.
func TestMain(m *testing.M) {
	if os.Getenv("PM_BROWSER_PARITY_CLI_CHILD") != "1" {
		os.Exit(m.Run())
	}
	var input parityCLIInput
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		os.Exit(90)
	}
	certificate, err := x509.ParseCertificate(input.Certificate)
	if err != nil {
		os.Exit(90)
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	client := &http.Client{
		Transport:     transport,
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	sessions := pmapi.NewMemorySessionStore()
	key, err := pmapi.NewSessionKey(input.InstanceID, input.SessionHandle)
	if err != nil || sessions.Store(context.Background(), key, input.Session) != nil {
		os.Exit(90)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Execute(ctx, cli.Options{
		Args: input.Args, ProfilesPath: input.ProfilesPath,
		Stdin: strings.NewReader(""), Stdout: os.Stdout, Stderr: os.Stderr,
		HTTPClient:   client,
		SessionStore: func(pmapi.SessionStoreMode) (pmapi.SessionStore, error) { return sessions, nil },
	})
	stop()
	transport.CloseIdleConnections()
	os.Exit(code)
}

type parityCLIProcessResult struct {
	stdout []byte
	stderr []byte
	err    error
}

func runParityCLIProcess(t *testing.T, input parityCLIInput) parityCLIProcessResult {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal("encode owned CLI input")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable)
	command.Env = []string{"PM_BROWSER_PARITY_CLI_CHILD=1"}
	command.Stdin = bytes.NewReader(payload)
	var stdout, stderr parityOutput
	command.Stdout, command.Stderr = &stdout, &stderr
	runErr := command.Run()
	if ctx.Err() != nil {
		t.Fatalf("generated CLI process exceeded its owned time bound: %v", ctx.Err())
	}
	return parityCLIProcessResult{
		stdout: bytes.Clone(stdout.Bytes()),
		stderr: bytes.Clone(stderr.Bytes()),
		err:    runErr,
	}
}

func runParityCLI(t *testing.T, input parityCLIInput) []byte {
	t.Helper()
	result := runParityCLIProcess(t, input)
	if result.err != nil {
		t.Fatalf("generated CLI process failed: %v; diagnostic: %s", result.err, result.stderr)
	}
	if len(result.stderr) != 0 {
		t.Fatalf("generated CLI emitted an unexpected diagnostic: %s", result.stderr)
	}
	return result.stdout
}

type parityOutput struct{ bytes.Buffer }

func (output *parityOutput) Write(value []byte) (int, error) {
	if output.Len()+len(value) > 1<<20 {
		return 0, errors.New("owned child output exceeds bound")
	}
	return output.Buffer.Write(value)
}

func parityCertificate(t *testing.T, fixture *testfixture.Fixture) (*x509.Certificate, string) {
	t.Helper()
	response, err := fixture.Client.Get(fixture.BaseURL + "/v1/info")
	if err != nil {
		t.Fatal("read owned API certificate")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.TLS == nil || len(response.TLS.PeerCertificates) != 1 {
		t.Fatal("owned API did not present its selected leaf certificate")
	}
	certificate := response.TLS.PeerCertificates[0]
	publicKey, err := x509.MarshalPKIXPublicKey(certificate.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(publicKey)
	return certificate, base64.StdEncoding.EncodeToString(digest[:])
}

func installBrowserSurface(t *testing.T, fixture *testfixture.Fixture, authentication *auth.Service, root string, counters *parityCounters) string {
	t.Helper()
	if authentication == nil || !filepath.IsAbs(root) {
		t.Fatal("owned authentication service and checkout are required")
	}
	origin, err := url.Parse(fixture.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	nonce := rand.Text()
	files := map[string]string{
		"/__pm_fixture__/":                         "tests/client-parity/index.html",
		"/__pm_fixture__/sdk/browser/client.js":    ".cache/browser-sdk/sdk/browser/client.js",
		"/__pm_fixture__/api/controls.json":        ".cache/browser-sdk/api/controls.json",
		"/__pm_fixture__/vendor/openapi-fetch.mjs": "sdk/browser/node_modules/openapi-fetch/dist/index.mjs",
	}
	for route, relativePath := range files {
		path := filepath.Join(root, relativePath)
		information, err := os.Stat(path)
		if err != nil || !information.Mode().IsRegular() {
			t.Fatalf("owned browser asset unavailable: %s", relativePath)
		}
		fixture.HandleHTTP(t, route, func(writer http.ResponseWriter, request *http.Request, _ http.Handler) {
			if request.Method != http.MethodGet || request.URL.Path != route || request.Host != origin.Host {
				http.NotFound(writer, request)
				return
			}
			writer.Header().Set("Cache-Control", "no-store")
			writer.Header().Set("X-Content-Type-Options", "nosniff")
			http.ServeFile(writer, request, path)
		})
	}
	fixture.HandleHTTP(t, "/__pm_fixture__/bootstrap", func(writer http.ResponseWriter, request *http.Request, _ http.Handler) {
		if request.Method != http.MethodPost || request.Host != origin.Host || request.Header.Get("Origin") != fixture.BaseURL || request.TLS == nil {
			http.Error(writer, "fixture bootstrap denied", http.StatusForbidden)
			return
		}
		var input struct {
			Nonce string `json:"nonce"`
			Actor string `json:"actor"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 4<<10))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || subtle.ConstantTimeCompare([]byte(input.Nonce), []byte(nonce)) != 1 {
			http.Error(writer, "fixture bootstrap denied", http.StatusForbidden)
			return
		}
		principal := fixture.Actor.RequestedActor
		switch input.Actor {
		case "alpha":
		case "beta":
			principal = fixture.ForeignActor.RequestedActor
		default:
			http.Error(writer, "fixture bootstrap denied", http.StatusForbidden)
			return
		}
		session, err := authentication.IssuePMSession(request.Context(), auth.SessionIssue{Principal: principal, Transport: store.SessionTransportBrowser})
		if err != nil {
			http.Error(writer, "fixture session unavailable", http.StatusInternalServerError)
			return
		}
		cookie, err := auth.NewBrowserCookie(session)
		if err != nil || cookie.SameSite != "Lax" || !cookie.HTTPOnly || !cookie.Secure || cookie.Path != "/" {
			http.Error(writer, "fixture session unavailable", http.StatusInternalServerError)
			return
		}
		http.SetCookie(writer, &http.Cookie{
			Name: cookie.Name, Value: cookie.Value, Path: cookie.Path,
			Secure: cookie.Secure, HttpOnly: cookie.HTTPOnly, SameSite: http.SameSiteLaxMode,
			Expires: session.ExpiresAt,
		})
		http.SetCookie(writer, &http.Cookie{
			Name: "__Host-pm_csrf", Value: session.CSRFToken, Path: "/",
			Secure: true, SameSite: http.SameSiteStrictMode, Expires: session.ExpiresAt,
		})
		writer.Header().Set("Cache-Control", "no-store")
		writer.WriteHeader(http.StatusNoContent)
	})
	fixture.HandleHTTP(t, "/__pm_fixture__/observations", func(writer http.ResponseWriter, request *http.Request, _ http.Handler) {
		if request.Method != http.MethodGet || request.Host != origin.Host {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(writer).Encode(map[string]int64{
			"admission_calls": counters.admissions.Load(),
			"cancel_calls":    counters.cancels.Load(),
			"effect_calls":    counters.effects.Load(),
		})
	})
	for _, fault := range []string{"none", "origin", "host"} {
		fixture.HandleHTTP(t, "/__pm_fixture__/inbound-fault/"+fault, func(writer http.ResponseWriter, request *http.Request, api http.Handler) {
			if request.Method != http.MethodPost || request.Host != origin.Host || request.Header.Get("Origin") != fixture.BaseURL || request.TLS == nil {
				http.Error(writer, "fixture fault ingress denied", http.StatusForbidden)
				return
			}
			forwarded := request.Clone(request.Context())
			forwarded.URL.Path = "/v1/workspaces/" + url.PathEscape(fixture.Scope.WorkspaceID) + "/invocations"
			forwarded.URL.RawPath = ""
			forwarded.URL.RawQuery = ""
			forwarded.RequestURI = forwarded.URL.RequestURI()
			if fault == "origin" {
				forwarded.Header.Set("Origin", "https://foreign-origin.invalid")
			} else if fault == "host" {
				forwarded.Host = "foreign-host.invalid"
			}
			api.ServeHTTP(writer, forwarded)
		})
	}
	return nonce
}
