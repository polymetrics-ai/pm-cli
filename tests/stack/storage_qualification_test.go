package stack_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/config"
	stackoperator "github.com/polymetrics-ai/pm-cli/internal/stack"
	"go.temporal.io/sdk/client"
)

const selectedQualificationTimeout = 9 * time.Minute

type selectedStackFixture struct {
	configPath      string
	configuration   config.Configuration
	postgresSecrets string
	temporalSecrets string
	artifactSecrets string
	primaryS3       string
	secondaryS3     string
}

type workerReceipt struct {
	Length int    `json:"length"`
	SHA256 string `json:"sha256"`
	Body   []byte `json:"body"`
}

func TestSelectedStorageQualification(t *testing.T) {
	if receiptPath := os.Getenv("PM_STACK_STORAGE_WORKER_RECEIPT"); receiptPath != "" {
		runStorageWorker(t, receiptPath)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), selectedQualificationTimeout)
	defer cancel()
	fixture := loadSelectedStackFixture(t)
	runOperator(t, ctx, fixture.configPath, "check")

	primary := newS3Client(t, fixture, fixture.primaryS3)
	secondary := newS3Client(t, fixture, fixture.secondaryS3)
	bucket := fixture.configuration.Artifacts().Bucket
	seedObjects(t, ctx, primary, bucket)
	qualifyConcurrentNestedListing(t, ctx, fixture, bucket)
	assertFilerConnectionCap(t, ctx, fixture)

	body := bytes.Repeat([]byte("pm-stack selected storage qualification\n"), 4096)
	objectKey := "qualification/worker/" + uniqueSuffix(t)
	putObject(t, ctx, primary, bucket, objectKey, body)
	primaryBody, err := readVerifiedObject(ctx, primary, bucket, objectKey, body)
	if err != nil {
		t.Fatalf("primary gateway exact read failed: %v", err)
	}
	secondaryBody, err := readVerifiedObject(ctx, secondary, bucket, objectKey, body)
	if err != nil {
		t.Fatalf("secondary gateway exact read failed: %v", err)
	}
	if !bytes.Equal(primaryBody, secondaryBody) {
		t.Fatal("independent gateways returned different object bytes")
	}
	worker := runIndependentStorageWorker(t, fixture, bucket, objectKey)
	digest := sha256.Sum256(body)
	if worker.Length != len(body) || worker.SHA256 != hex.EncodeToString(digest[:]) || !bytes.Equal(worker.Body, body) {
		t.Fatal("independent worker receipt did not preserve exact length, digest, and content")
	}

	qualifyInterruptedMultipart(t, ctx, primary, bucket)
	qualifyRejectedCorruption(t, ctx, primary, bucket, body)
}

func TestSelectedPostgresTemporalQualification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), selectedQualificationTimeout)
	defer cancel()
	fixture := loadSelectedStackFixture(t)
	qualifyOperatorConfigurationBoundary(t, fixture)
	t.Run("operator ignores unapproved Compose dotenv inputs", func(t *testing.T) {
		t.Setenv("COMPOSE_DISABLE_ENV_FILE", "false")
		t.Setenv("COMPOSE_ENV_FILES", filepath.Join(t.TempDir(), "must-not-read.env"))
		runOperator(t, ctx, fixture.configPath, "up")
	})

	start := make(chan struct{})
	errorsFromMigration := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			var stdout, stderr bytes.Buffer
			err := stackoperator.Run(ctx, []string{"migrate", "--config", fixture.configPath}, nil, &stdout, &stderr)
			errorsFromMigration <- err
		}()
	}
	close(start)
	for index := range 2 {
		if err := <-errorsFromMigration; err != nil {
			t.Fatalf("concurrent fenced migration %d failed: %v", index+1, err)
		}
	}

	assertPostgresTemporalTopology(t, ctx, fixture)
	assertWrongSchemaRejected(t, ctx, fixture)
	assertDeniedPostgresRole(t, ctx, fixture)
	assertTemporalClientCertificateRequired(t, ctx, fixture)

	t.Run("exposed infrastructure port is refused before stop", func(t *testing.T) {
		t.Setenv("PM_STACK_PUBLISH_HOST", "0.0.0.0")
		var stdout, stderr bytes.Buffer
		err := stackoperator.Run(ctx, []string{"drain", "--dependency", "all", "--config", fixture.configPath}, nil, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), "127.0.0.1") {
			t.Fatalf("unsafe publish request was not refused before lifecycle mutation: %v", err)
		}
	})
	runOperator(t, ctx, fixture.configPath, "check")

	managedPath := writeManagedProjection(t, fixture.configPath)
	stateDir := requiredEnvironment(t, "PM_STACK_STATE_DIR")
	sentinelPath := filepath.Join(stateDir, "managed-stop-refusal.sentinel")
	if err := os.WriteFile(sentinelPath, []byte("unchanged"), 0o600); err != nil {
		t.Fatalf("write managed-stop sentinel: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(sentinelPath) })
	var stdout, stderr bytes.Buffer
	err := stackoperator.Run(ctx, []string{"drain", "--dependency", "all", "--config", managedPath}, nil, &stdout, &stderr)
	var diagnostic *config.Diagnostic
	if !errors.As(err, &diagnostic) || diagnostic.Kind != config.DiagnosticOwnership {
		t.Fatalf("managed stop was not rejected by the configuration authority: %v", err)
	}
	sentinel, err := os.ReadFile(sentinelPath)
	if err != nil || string(sentinel) != "unchanged" {
		t.Fatalf("managed stop refusal mutated operator state: %v", err)
	}
	runOperator(t, ctx, managedPath, "check")
}

func loadSelectedStackFixture(t *testing.T) selectedStackFixture {
	t.Helper()
	configPath := requiredEnvironment(t, "PM_STACK_TEST_CONFIG")
	file, err := os.Open(configPath)
	if err != nil {
		t.Fatalf("open selected stack configuration: %v", err)
	}
	document, err := config.Load(file)
	_ = file.Close()
	if err != nil {
		t.Fatalf("load selected stack configuration: %v", err)
	}
	configuration, err := config.Validate(document, config.Prelaunch{})
	if err != nil {
		t.Fatalf("validate selected stack configuration: %v", err)
	}
	secretRoot := requiredEnvironment(t, "PM_STACK_SECRETS_DIR")
	return selectedStackFixture{
		configPath:      configPath,
		configuration:   configuration,
		postgresSecrets: resolveTestSecretBundle(t, secretRoot, configuration.Postgres().EndpointRef),
		temporalSecrets: resolveTestSecretBundle(t, secretRoot, configuration.Temporal().ClientIdentityRef),
		artifactSecrets: resolveTestSecretBundle(t, secretRoot, configuration.Artifacts().CredentialRef),
		primaryS3:       requiredEnvironment(t, "PM_STACK_TEST_S3_PRIMARY_ENDPOINT"),
		secondaryS3:     requiredEnvironment(t, "PM_STACK_TEST_S3_SECONDARY_ENDPOINT"),
	}
}

func requiredEnvironment(t *testing.T, name string) string {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		t.Fatalf("%s is required for the selected real-stack qualification", name)
	}
	return value
}

func resolveTestSecretBundle(t *testing.T, root, reference string) string {
	t.Helper()
	parsed, err := url.Parse(reference)
	if err != nil || parsed.Scheme != "secret" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		t.Fatal("selected configuration contains an invalid secret reference")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatalf("resolve test secret root: %v", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve test secret root: %v", err)
	}
	pathValue, err := url.PathUnescape(filepath.FromSlash(parsed.Host + "/" + strings.TrimPrefix(parsed.EscapedPath(), "/")))
	if err != nil {
		t.Fatal("selected configuration contains an invalid escaped secret reference")
	}
	bundle, err := filepath.EvalSymlinks(filepath.Join(root, pathValue))
	if err != nil {
		t.Fatalf("resolve selected secret bundle: %v", err)
	}
	relative, err := filepath.Rel(root, bundle)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		t.Fatal("selected secret bundle escapes its root")
	}
	return bundle
}

func readTestSecret(t *testing.T, directory, name string) string {
	t.Helper()
	value, err := os.ReadFile(filepath.Join(directory, name))
	if err != nil {
		t.Fatalf("read selected secret bundle member %q: %v", name, err)
	}
	text := strings.TrimSpace(string(value))
	if text == "" {
		t.Fatalf("selected secret bundle member %q is empty", name)
	}
	return text
}

func runOperator(t *testing.T, ctx context.Context, configPath string, action string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if err := stackoperator.Run(ctx, []string{action, "--config", configPath}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("pm-stack %s failed: %v", action, err)
	}
}

func newS3Client(t *testing.T, fixture selectedStackFixture, endpoint string) *s3.Client {
	t.Helper()
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		t.Fatalf("selected S3 gateway endpoint is not HTTPS: %q", endpoint)
	}
	caPEM, err := os.ReadFile(filepath.Join(fixture.artifactSecrets, "ca.pem"))
	if err != nil {
		t.Fatalf("read artifact CA: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("artifact CA is not valid PEM")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	accessKey := readTestSecret(t, fixture.artifactSecrets, "access-key")
	secretKey := readTestSecret(t, fixture.artifactSecrets, "secret-key")
	return s3.New(s3.Options{
		BaseEndpoint: aws.String(parsed.String()),
		Region:       "us-east-1",
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: accessKey, SecretAccessKey: secretKey, Source: "selected-stack-test"}, nil
		}),
		HTTPClient:   &http.Client{Transport: transport, Timeout: 30 * time.Second},
		UsePathStyle: true,
	})
}

func seedObjects(t *testing.T, ctx context.Context, client *s3.Client, bucket string) {
	t.Helper()
	for key, body := range map[string][]byte{
		"a/one":   []byte("seed-one\n"),
		"a/two":   []byte("seed-two\n"),
		"b/three": []byte("seed-three\n"),
	} {
		response, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err == nil {
			existing, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr != nil || !bytes.Equal(existing, body) {
				t.Fatalf("pre-existing immutable seed %q has unexpected bytes", key)
			}
			continue
		}
		putObject(t, ctx, client, bucket, key, body)
	}
}

func qualifyConcurrentNestedListing(t *testing.T, ctx context.Context, fixture selectedStackFixture, bucket string) {
	t.Helper()
	start := make(chan struct{})
	failures := make(chan error, 4)
	clients := make([]*s3.Client, 4)
	for index := range clients {
		clients[index] = newS3Client(t, fixture, fixture.primaryS3)
	}
	var wait sync.WaitGroup
	for index := range clients {
		wait.Add(1)
		go func(client *s3.Client) {
			defer wait.Done()
			<-start
			root, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
				Bucket: aws.String(bucket), Delimiter: aws.String("/"),
			})
			if err != nil {
				failures <- fmt.Errorf("root nested listing: %w", err)
				return
			}
			prefixes := make(map[string]bool)
			for _, prefix := range root.CommonPrefixes {
				prefixes[aws.ToString(prefix.Prefix)] = true
			}
			if !prefixes["a/"] || !prefixes["b/"] {
				failures <- fmt.Errorf("root listing omitted seeded nested prefixes")
				return
			}
			a, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
				Bucket: aws.String(bucket), Prefix: aws.String("a/"), Delimiter: aws.String("/"),
			})
			if err != nil {
				failures <- fmt.Errorf("nested a listing: %w", err)
				return
			}
			keys := make([]string, 0, len(a.Contents))
			for _, object := range a.Contents {
				keys = append(keys, aws.ToString(object.Key))
			}
			sort.Strings(keys)
			if len(keys) != 2 || keys[0] != "a/one" || keys[1] != "a/two" {
				failures <- fmt.Errorf("nested a listing did not return the exact seeded keys")
				return
			}
			b, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
				Bucket: aws.String(bucket), Prefix: aws.String("b/"), Delimiter: aws.String("/"),
			})
			if err != nil || len(b.Contents) != 1 || aws.ToString(b.Contents[0].Key) != "b/three" {
				failures <- fmt.Errorf("nested b listing did not return the exact seeded key")
			}
		}(clients[index])
	}
	close(start)
	wait.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func assertFilerConnectionCap(t *testing.T, ctx context.Context, fixture selectedStackFixture) {
	t.Helper()
	endpoint := readTestSecret(t, fixture.postgresSecrets, "pm-app-endpoint")
	connection, err := pgx.Connect(ctx, endpoint)
	if err != nil {
		t.Fatalf("connect for filer pool observation: %v", err)
	}
	defer connection.Close(context.Background())
	var connections int
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = 'pm_objects_meta' AND usename = 'pm_objects_runtime'`).Scan(&connections); err != nil {
		t.Fatalf("observe filer PostgreSQL pool: %v", err)
	}
	// The selected configuration caps the pools at two; lazy opening and
	// idle expiry need not leave both connections occupied at observation.
	if connections > 2 {
		t.Fatalf("postgres2 pool exceeded the selected ceiling of two connections: observed %d", connections)
	}
}

func putObject(t *testing.T, ctx context.Context, client *s3.Client, bucket, key string, body []byte) {
	t.Helper()
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(body),
	}); err != nil {
		t.Fatalf("put qualification object %q: %v", key, err)
	}
}

func readVerifiedObject(ctx context.Context, client *s3.Client, bucket, key string, expected []byte) ([]byte, error) {
	response, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	expectedDigest := sha256.Sum256(expected)
	actualDigest := sha256.Sum256(body)
	if len(body) != len(expected) || actualDigest != expectedDigest || !bytes.Equal(body, expected) {
		return nil, fmt.Errorf("object length, SHA-256, or content mismatch")
	}
	return body, nil
}

func runIndependentStorageWorker(t *testing.T, fixture selectedStackFixture, bucket, key string) workerReceipt {
	t.Helper()
	receiptPath := filepath.Join(t.TempDir(), "worker-receipt.json")
	command := exec.Command(os.Args[0], "-test.run=^TestSelectedStorageQualification$", "-test.count=1")
	command.Env = append(os.Environ(),
		"PM_STACK_STORAGE_WORKER_RECEIPT="+receiptPath,
		"PM_STACK_STORAGE_WORKER_ENDPOINT="+fixture.secondaryS3,
		"PM_STACK_STORAGE_WORKER_BUCKET="+bucket,
		"PM_STACK_STORAGE_WORKER_KEY="+key,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("independent storage worker failed: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	contents, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatalf("read independent worker receipt: %v", err)
	}
	var receipt workerReceipt
	if err := json.Unmarshal(contents, &receipt); err != nil {
		t.Fatalf("decode independent worker receipt: %v", err)
	}
	return receipt
}

func assertNoIndependentWorkerReceipt(t *testing.T, bucket, key string) {
	t.Helper()
	receiptPath := filepath.Join(t.TempDir(), "worker-receipt.json")
	command := exec.Command(os.Args[0], "-test.run=^TestSelectedStorageQualification$", "-test.count=1")
	command.Env = append(os.Environ(),
		"PM_STACK_STORAGE_WORKER_RECEIPT="+receiptPath,
		"PM_STACK_STORAGE_WORKER_ENDPOINT="+requiredEnvironment(t, "PM_STACK_TEST_S3_PRIMARY_ENDPOINT"),
		"PM_STACK_STORAGE_WORKER_BUCKET="+bucket,
		"PM_STACK_STORAGE_WORKER_KEY="+key,
	)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("independent worker accepted an interrupted object (%s)", strings.TrimSpace(string(output)))
	}
	if _, err := os.Stat(receiptPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted object produced a worker receipt: %v", err)
	}
}

func runStorageWorker(t *testing.T, receiptPath string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fixture := loadSelectedStackFixture(t)
	endpoint := requiredEnvironment(t, "PM_STACK_STORAGE_WORKER_ENDPOINT")
	bucket := requiredEnvironment(t, "PM_STACK_STORAGE_WORKER_BUCKET")
	key := requiredEnvironment(t, "PM_STACK_STORAGE_WORKER_KEY")
	client := newS3Client(t, fixture, endpoint)
	response, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatalf("worker get exact object: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatalf("worker read exact object: %v", err)
	}
	digest := sha256.Sum256(body)
	receipt := workerReceipt{Length: len(body), SHA256: hex.EncodeToString(digest[:]), Body: body}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("encode worker receipt: %v", err)
	}
	if err := os.WriteFile(receiptPath, encoded, 0o600); err != nil {
		t.Fatalf("write worker receipt: %v", err)
	}
}

func qualifyInterruptedMultipart(t *testing.T, ctx context.Context, client *s3.Client, bucket string) {
	t.Helper()
	key := "qualification/interrupted/" + uniqueSuffix(t)
	created, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key),
	})
	if err != nil {
		t.Fatalf("create interrupted multipart upload: %v", err)
	}
	if created.UploadId == nil || *created.UploadId == "" {
		t.Fatal("multipart upload did not return an upload identifier")
	}
	part := bytes.Repeat([]byte("partial"), 1<<20)
	if _, err := client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: created.UploadId,
		PartNumber: aws.Int32(1), Body: bytes.NewReader(part),
	}); err != nil {
		t.Fatalf("upload interrupted multipart part: %v", err)
	}
	if _, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}); err == nil {
		t.Fatal("interrupted multipart upload became a completed object")
	}
	listed, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(key)})
	if err != nil {
		t.Fatalf("list interrupted multipart object: %v", err)
	}
	if len(listed.Contents) != 0 {
		t.Fatal("interrupted multipart upload produced an object reference")
	}
	assertNoIndependentWorkerReceipt(t, bucket, key)
	uploads, err := client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket), Prefix: aws.String(key)})
	if err != nil {
		t.Fatalf("list interrupted multipart upload: %v", err)
	}
	found := false
	for _, upload := range uploads.Uploads {
		if aws.ToString(upload.Key) == key && aws.ToString(upload.UploadId) == aws.ToString(created.UploadId) {
			found = true
		}
	}
	if !found {
		t.Fatal("incomplete multipart state was not observable before abort")
	}
	if _, err := client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: created.UploadId,
	}); err != nil {
		t.Fatalf("abort interrupted multipart upload: %v", err)
	}
	if _, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}); err == nil {
		t.Fatal("aborted multipart upload became a completed object")
	}
}

func qualifyRejectedCorruption(t *testing.T, ctx context.Context, client *s3.Client, bucket string, expected []byte) {
	t.Helper()
	corrupted := append([]byte(nil), expected...)
	corrupted[len(corrupted)/2] ^= 0xff
	corruptKey := "qualification/corrupt/" + uniqueSuffix(t)
	putObject(t, ctx, client, bucket, corruptKey, corrupted)
	if _, err := readVerifiedObject(ctx, client, bucket, corruptKey, expected); err == nil {
		t.Fatal("corrupted object bytes passed exact verification")
	}
	truncatedKey := "qualification/truncated/" + uniqueSuffix(t)
	putObject(t, ctx, client, bucket, truncatedKey, expected[:len(expected)/2])
	if _, err := readVerifiedObject(ctx, client, bucket, truncatedKey, expected); err == nil {
		t.Fatal("truncated object bytes passed exact verification")
	}
}

func uniqueSuffix(t *testing.T) string {
	t.Helper()
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", t.Name(), time.Now().UnixNano())))
	return hex.EncodeToString(digest[:8])
}

func qualifyOperatorConfigurationBoundary(t *testing.T, fixture selectedStackFixture) {
	t.Helper()
	valid, err := os.ReadFile(fixture.configPath)
	if err != nil {
		t.Fatalf("read selected configuration for operator boundary checks: %v", err)
	}
	var missingDocument map[string]any
	if err := json.Unmarshal(valid, &missingDocument); err != nil {
		t.Fatalf("decode selected configuration for missing-reference check: %v", err)
	}
	artifacts, ok := missingDocument["artifacts"].(map[string]any)
	if !ok {
		t.Fatal("selected configuration does not contain an artifacts object")
	}
	delete(artifacts, "credential_ref")
	missingReference, err := json.Marshal(missingDocument)
	if err != nil {
		t.Fatalf("encode missing-reference configuration: %v", err)
	}
	trimmed := strings.TrimSpace(string(valid))
	if len(trimmed) < 2 || trimmed[0] != '{' {
		t.Fatal("selected configuration is not a JSON object")
	}
	cases := []struct {
		name string
		raw  string
		kind config.DiagnosticKind
	}{
		{
			name: "duplicate",
			raw:  `{"schema_version":"1",` + trimmed[1:],
			kind: config.DiagnosticDuplicate,
		},
		{
			name: "unknown",
			raw:  `{"unknown_stack_key":true,` + trimmed[1:],
			kind: config.DiagnosticUnknown,
		},
		{
			name: "missing reference",
			raw:  string(missingReference),
			kind: config.DiagnosticRequired,
		},
	}
	for _, testCase := range cases {
		t.Run("operator rejects "+testCase.name+" before state mutation", func(t *testing.T) {
			statePath := filepath.Join(t.TempDir(), "must-not-exist")
			t.Setenv("PM_STACK_STATE_DIR", statePath)
			var stdout, stderr bytes.Buffer
			err := stackoperator.Run(context.Background(), []string{"up"}, strings.NewReader(testCase.raw), &stdout, &stderr)
			var diagnostic *config.Diagnostic
			if !errors.As(err, &diagnostic) || diagnostic.Kind != testCase.kind {
				t.Fatalf("operator returned the wrong strict configuration diagnostic: %v", err)
			}
			if _, statErr := os.Stat(statePath); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("invalid configuration created operator state: %v", statErr)
			}
		})
	}
}

func assertPostgresTemporalTopology(t *testing.T, ctx context.Context, fixture selectedStackFixture) {
	t.Helper()
	checks := []struct {
		file     string
		database string
		user     string
		version  string
	}{
		{file: "pm-app-endpoint", database: "pm_app", user: "pm_app_runtime"},
		{file: "temporal-endpoint", database: "temporal", user: "temporal_runtime", version: "1.19"},
		{file: "temporal-visibility-endpoint", database: "temporal_visibility", user: "temporal_visibility_runtime", version: "1.14"},
		{file: "objects-endpoint", database: "pm_objects_meta", user: "pm_objects_runtime"},
	}
	for _, check := range checks {
		connection, err := pgx.Connect(ctx, readTestSecret(t, fixture.postgresSecrets, check.file))
		if err != nil {
			t.Fatalf("connect %s as %s: %v", check.database, check.user, err)
		}
		var database, user, serverVersion string
		if err := connection.QueryRow(ctx, `SELECT current_database(), current_user, current_setting('server_version')`).Scan(&database, &user, &serverVersion); err != nil {
			connection.Close(context.Background())
			t.Fatalf("query %s identity: %v", check.database, err)
		}
		if database != check.database || user != check.user || !strings.HasPrefix(serverVersion, "16.15") {
			connection.Close(context.Background())
			t.Fatalf("%s returned the wrong database, role, or PostgreSQL version", check.database)
		}
		if check.database == "pm_app" {
			t.Run("application bootstrap preserves store grant ownership", func(t *testing.T) {
				var readMarker, mutateMarker, inheritedDefaults, databaseDDL bool
				err := connection.QueryRow(ctx, `SELECT
					has_table_privilege(current_user, 'pm.stack_schema', 'SELECT'),
					has_table_privilege(current_user, 'pm.stack_schema', 'INSERT') OR
					has_table_privilege(current_user, 'pm.stack_schema', 'UPDATE') OR
					has_table_privilege(current_user, 'pm.stack_schema', 'DELETE') OR
					has_table_privilege(current_user, 'pm.stack_schema', 'TRUNCATE'),
					EXISTS (
						SELECT 1 FROM pg_default_acl d
						CROSS JOIN LATERAL aclexplode(d.defaclacl) a
						WHERE d.defaclrole = (SELECT oid FROM pg_roles WHERE rolname = 'pm_app_migrator')
						AND d.defaclnamespace = 'pm'::regnamespace
						AND d.defaclobjtype IN ('r', 'S')
						AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = current_user)
					),
					has_database_privilege(current_user, current_database(), 'CREATE') OR
					has_database_privilege(current_user, current_database(), 'TEMPORARY')`).Scan(&readMarker, &mutateMarker, &inheritedDefaults, &databaseDDL)
				if err != nil {
					t.Fatalf("observe application runtime grant ownership: %v", err)
				}
				if !readMarker || mutateMarker || inheritedDefaults || databaseDDL {
					t.Fatalf("bootstrap must leave the marker read-only, future grants store-owned, and database DDL denied: read=%t mutate=%t inherited=%t databaseDDL=%t", readMarker, mutateMarker, inheritedDefaults, databaseDDL)
				}
			})
		}
		if check.version != "" {
			var version string
			if err := connection.QueryRow(ctx, `SELECT curr_version FROM schema_version WHERE version_partition = 0 AND db_name = $1`, check.database).Scan(&version); err != nil {
				connection.Close(context.Background())
				t.Fatalf("query %s schema version: %v", check.database, err)
			}
			if version != check.version {
				connection.Close(context.Background())
				t.Fatalf("%s schema version = %q, want %q", check.database, version, check.version)
			}
			t.Run(check.database+" rejects changed schema version", func(t *testing.T) {
				migrationConfig, err := pgx.ParseConfig(readTestSecret(t, fixture.postgresSecrets, check.file))
				if err != nil {
					t.Fatal("parse selected schema migration endpoint")
				}
				migrationConfig.User = strings.TrimSuffix(check.user, "_runtime") + "_migrator"
				migrationConfig.Password = readTestSecret(t, fixture.postgresSecrets, strings.TrimSuffix(check.file, "-endpoint")+"-migrator-password")
				migration, err := pgx.ConnectConfig(ctx, migrationConfig)
				if err != nil {
					t.Fatal("connect selected schema migrator")
				}
				defer migration.Close(context.Background())
				const incompatibleVersion = "0.0"
				defer func() {
					restoreCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					restored, err := migration.Exec(restoreCtx, `UPDATE schema_version SET curr_version = $1 WHERE version_partition = 0 AND db_name = $2 AND curr_version IN ($1, $3)`, version, check.database, incompatibleVersion)
					if err != nil || restored.RowsAffected() != 1 {
						t.Errorf("restore the owned %s schema version: %v", check.database, err)
						return
					}
					var stdout, stderr bytes.Buffer
					if err := stackoperator.Run(restoreCtx, []string{"check", "--config", fixture.configPath}, nil, &stdout, &stderr); err != nil {
						t.Errorf("restored %s schema did not recover readiness: %v", check.database, err)
					}
				}()
				changed, err := migration.Exec(ctx, `UPDATE schema_version SET curr_version = $1 WHERE version_partition = 0 AND db_name = $2 AND curr_version = $3`, incompatibleVersion, check.database, version)
				if err != nil || changed.RowsAffected() != 1 {
					t.Fatalf("inject the owned %s schema version fault: %v", check.database, err)
				}
				var stdout, stderr bytes.Buffer
				if err := stackoperator.Run(ctx, []string{"check", "--config", fixture.configPath}, nil, &stdout, &stderr); err == nil {
					t.Fatalf("operator accepted incompatible %s schema version", check.database)
				}
			})
		}
		connection.Close(context.Background())
	}
	runOperator(t, ctx, fixture.configPath, "check")
}

func assertWrongSchemaRejected(t *testing.T, ctx context.Context, fixture selectedStackFixture) {
	t.Helper()
	t.Run("operator rejects an endpoint with the wrong schema", func(t *testing.T) {
		originalRoot := requiredEnvironment(t, "PM_STACK_SECRETS_DIR")
		originalRoot, err := filepath.EvalSymlinks(originalRoot)
		if err != nil {
			t.Fatalf("resolve PM_STACK_SECRETS_DIR: %v", err)
		}
		clonedRoot := t.TempDir()
		bundles := []string{fixture.postgresSecrets, fixture.temporalSecrets, fixture.artifactSecrets}
		var clonedPostgres string
		for _, source := range bundles {
			relative, err := filepath.Rel(originalRoot, source)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				t.Fatal("selected secret bundle is outside PM_STACK_SECRETS_DIR")
			}
			destination := filepath.Join(clonedRoot, relative)
			copyFlatSecretBundle(t, source, destination)
			if source == fixture.postgresSecrets {
				clonedPostgres = destination
			}
		}
		pmAppEndpoint, err := os.ReadFile(filepath.Join(fixture.postgresSecrets, "pm-app-endpoint"))
		if err != nil {
			t.Fatalf("read pm_app endpoint for wrong-schema projection: %v", err)
		}
		if err := os.WriteFile(filepath.Join(clonedPostgres, "temporal-endpoint"), pmAppEndpoint, 0o600); err != nil {
			t.Fatalf("write wrong-schema endpoint projection: %v", err)
		}
		t.Setenv("PM_STACK_SECRETS_DIR", clonedRoot)
		var stdout, stderr bytes.Buffer
		err = stackoperator.Run(ctx, []string{"check", "--config", fixture.configPath}, nil, &stdout, &stderr)
		if err == nil {
			t.Fatal("operator accepted a Temporal endpoint with the wrong schema")
		}
	})
}

func copyFlatSecretBundle(t *testing.T, source, destination string) {
	t.Helper()
	if err := os.MkdirAll(destination, 0o700); err != nil {
		t.Fatalf("create cloned secret bundle: %v", err)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatalf("read source secret bundle: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("secret bundle member %q is unexpectedly a directory", entry.Name())
		}
		value, err := os.ReadFile(filepath.Join(source, entry.Name()))
		if err != nil {
			t.Fatalf("read secret bundle member %q: %v", entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(destination, entry.Name()), value, 0o600); err != nil {
			t.Fatalf("clone secret bundle member %q: %v", entry.Name(), err)
		}
	}
}

func assertDeniedPostgresRole(t *testing.T, ctx context.Context, fixture selectedStackFixture) {
	t.Helper()
	endpoint := readTestSecret(t, fixture.postgresSecrets, "denied-endpoint")
	connection, err := pgx.Connect(ctx, endpoint)
	if err == nil {
		connection.Close(context.Background())
		t.Fatal("PostgreSQL role without CONNECT privilege reached an infrastructure database")
	}
}

func assertTemporalClientCertificateRequired(t *testing.T, ctx context.Context, fixture selectedStackFixture) {
	t.Helper()
	caPEM, err := os.ReadFile(filepath.Join(fixture.temporalSecrets, "ca.pem"))
	if err != nil {
		t.Fatalf("read Temporal CA: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("Temporal CA is not valid PEM")
	}
	host, _, err := net.SplitHostPort(fixture.configuration.Temporal().Endpoint)
	if err != nil {
		t.Fatalf("parse Temporal endpoint: %v", err)
	}
	unauthenticated, err := client.NewNamespaceClient(client.Options{
		HostPort:  fixture.configuration.Temporal().Endpoint,
		Namespace: fixture.configuration.Temporal().Namespace,
		Identity:  "pm-stack-unauthenticated-negative",
		ConnectionOptions: client.ConnectionOptions{TLS: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
			ServerName: host,
		}},
	})
	if err == nil {
		defer unauthenticated.Close()
		_, err = unauthenticated.Describe(ctx, fixture.configuration.Temporal().Namespace)
	}
	if err == nil {
		t.Fatal("Temporal namespace call without a client certificate succeeded")
	}
}

func writeManagedProjection(t *testing.T, configPath string) string {
	t.Helper()
	contents, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read selected configuration for managed projection: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(contents, &document); err != nil {
		t.Fatalf("decode selected configuration for managed projection: %v", err)
	}
	for _, sectionName := range []string{"postgres", "temporal", "artifacts"} {
		section, ok := document[sectionName].(map[string]any)
		if !ok || section["ownership"] != string(config.OwnershipBundled) {
			t.Fatalf("selected fixture %s ownership is not bundled", sectionName)
		}
		section["ownership"] = string(config.OwnershipManaged)
	}
	managed, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode managed stack projection: %v", err)
	}
	path := filepath.Join(t.TempDir(), "managed-stack.json")
	if err := os.WriteFile(path, managed, 0o600); err != nil {
		t.Fatalf("write managed stack projection: %v", err)
	}
	return path
}
