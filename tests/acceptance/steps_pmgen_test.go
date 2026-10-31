package acceptance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cucumber/godog"
)

// pmgenBuild holds the real pmgen binary, built once for the suite.
var pmgenBuild struct {
	once sync.Once
	path string
	err  error
}

// pmgenRepositoryRoot is the checkout that holds the checked-in connectors.
func pmgenRepositoryRoot() (string, error) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		return "", fmt.Errorf("resolve repository root: %w", err)
	}
	return root, nil
}

// pmgenBinary builds ./cmd/pmgen with CGO_ENABLED=0 and the go tool on PATH.
// The binary goes to the ignored .cache directory of the checkout, so a new run
// replaces it. CI sets TMPDIR to a persistent cache, so a new temporary
// directory for each run would stay there.
func pmgenBinary() (string, error) {
	pmgenBuild.once.Do(func() {
		root, err := pmgenRepositoryRoot()
		if err != nil {
			pmgenBuild.err = err
			return
		}
		goTool, err := exec.LookPath("go")
		if err != nil {
			pmgenBuild.err = fmt.Errorf("find the go tool on PATH: %w", err)
			return
		}
		dir := filepath.Join(root, ".cache", "acceptance")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			pmgenBuild.err = fmt.Errorf("create the binary directory: %w", err)
			return
		}
		binary := filepath.Join(dir, "pmgen")
		cmd := exec.Command(goTool, "build", "-o", binary, "./cmd/pmgen")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			pmgenBuild.err = fmt.Errorf("build pmgen: %v\n%s", err, out)
			return
		}
		pmgenBuild.path = binary
	})
	return pmgenBuild.path, pmgenBuild.err
}

// The synthetic source is one OpenAPI 3.0 document with one write (POST /items)
// and one read (GET /items/{id}). The write body has the provider dedup field
// client_request_id.
const (
	pmgenSourceID          = "items-api"
	pmgenWritePointer      = "#/paths/~1items/post"
	pmgenReadPointer       = "#/paths/~1items~1{id}/get"
	pmgenWriteBodyPointer  = "#/paths/~1items/post/requestBody/content/application~1json"
	pmgenDedupPointer      = pmgenWriteBodyPointer + "/schema/properties/client_request_id"
	pmgenNoDedupPointer    = pmgenWriteBodyPointer + "/schema"
	pmgenNoDedupField      = "none"
	pmgenSemanticsVersion  = "pm.semantics/v1"
	pmgenSourceLockVersion = "pm.connector-source/v1"
)

type pmgenSourceRef struct {
	SourceID string `json:"source_id"`
	Pointer  string `json:"pointer"`
}

type pmgenWitness struct {
	ProofID    string           `json:"proof_id"`
	SourceRefs []pmgenSourceRef `json:"source_refs"`
}

func (witness pmgenWitness) equal(other pmgenWitness) bool {
	return witness.ProofID == other.ProofID && slices.Equal(witness.SourceRefs, other.SourceRefs)
}

// pmgenWriteIdentity is the write identity of one write operation. Issue #117
// names the facts but not their members, and ADR 0003 (#67) is not merged. So
// this shape is an assumption of the RED scenarios. The semantics amendment
// carries it as "write_identity", and the bundle operation carries it as
// "execution.write_identity". The dedup field is a field identity of the write
// or "none".
type pmgenWriteIdentity struct {
	Dedup    *pmgenDedup    `json:"dedup,omitempty"`
	ReadBack *pmgenReadBack `json:"read_back,omitempty"`
}

type pmgenDedup struct {
	FieldID string       `json:"field_id"`
	Witness pmgenWitness `json:"witness"`
}

type pmgenReadBack struct {
	OperationID string       `json:"operation_id"`
	Witness     pmgenWitness `json:"witness"`
}

type pmgenRetry struct {
	Classification string       `json:"classification"`
	Witness        pmgenWitness `json:"witness"`
}

type pmgenLane struct {
	Lane           string           `json:"lane"`
	Classification string           `json:"classification"`
	SourceRefs     []pmgenSourceRef `json:"source_refs"`
}

type pmgenAmendment struct {
	OperationID   string              `json:"operation_id"`
	SourceRefs    []pmgenSourceRef    `json:"source_refs"`
	Retry         *pmgenRetry         `json:"retry,omitempty"`
	WriteIdentity *pmgenWriteIdentity `json:"write_identity,omitempty"`
	Lanes         []pmgenLane         `json:"lanes"`
}

type pmgenSemantics struct {
	SchemaVersion string            `json:"schema_version"`
	ConnectorID   string            `json:"connector_id"`
	SourceDigest  string            `json:"source_digest"`
	Revision      string            `json:"revision"`
	Operations    []*pmgenAmendment `json:"operations"`
}

// pmgenBundle is the part of a generated bundle that the observer reads.
type pmgenBundle struct {
	ConnectorID  string `json:"connector_id"`
	SourceDigest string `json:"source_digest"`
	Operations   []struct {
		OperationID string `json:"operation_id"`
		Execution   *struct {
			Retry         *pmgenRetry         `json:"retry"`
			WriteIdentity *pmgenWriteIdentity `json:"write_identity"`
		} `json:"execution"`
	} `json:"operations"`
}

// pmgenRun records one real pmgen process.
type pmgenRun struct {
	args     []string
	exitCode int
	stdout   string
	stderr   string
}

// describe gives the pmgen mode, the exit code and both output streams. The
// arguments are temporary paths, so it does not repeat them.
func (run *pmgenRun) describe() string {
	return fmt.Sprintf("pmgen %s: exit code %d\nstdout: %s\nstderr: %s",
		run.args[0], run.exitCode, run.stdout, run.stderr)
}

// pmgenScenario holds the state of one scenario: the synthetic source files,
// the semantics that the Given steps build, and the last pmgen run.
type pmgenScenario struct {
	dir         string
	connectorID string
	writeID     string
	readID      string
	semantics   pmgenSemantics
	bundle      string
	report      string
	last        *pmgenRun
}

func registerPmgenSteps(sc *godog.ScenarioContext) {
	scenario := &pmgenScenario{}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		*scenario = pmgenScenario{}
		return ctx, nil
	})
	// godog reports any error from this hook as a hook failure, so the hook
	// returns only its own error and never the step error.
	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		if scenario.dir == "" {
			return ctx, nil
		}
		if err := os.RemoveAll(scenario.dir); err != nil {
			return ctx, fmt.Errorf("remove the scenario directory: %w", err)
		}
		return ctx, nil
	})
	sc.Step(`^a connector source "([^"]*)" with the write operation "([^"]*)" and the read operation "([^"]*)"$`, scenario.writeSource)
	sc.Step(`^the semantics mark "([^"]*)" eligible for "([^"]*)"$`, scenario.markEligible)
	sc.Step(`^the semantics declare the dedup field "([^"]*)" for "([^"]*)" with a source witness$`, scenario.declareDedup)
	sc.Step(`^the semantics declare the read-back operation "([^"]*)" for "([^"]*)" with a source witness$`, scenario.declareReadBack)
	sc.Step(`^the semantics declare the retry classification "([^"]*)" for "([^"]*)" with a source witness$`, scenario.declareRetry)
	sc.Step(`^the semantics declare no write identity for "([^"]*)"$`, scenario.declareNoWriteIdentity)
	sc.Step(`^pmgen compiles the connector$`, scenario.compile)
	sc.Step(`^pmgen checks the checked-in connectors$`, scenario.checkCheckedIn)
	sc.Step(`^pmgen succeeds$`, scenario.succeeds)
	sc.Step(`^pmgen refuses to compile "([^"]*)"$`, scenario.refuses)
	sc.Step(`^pmgen writes no bundle$`, scenario.writesNoBundle)
	sc.Step(`^the bundle operation "([^"]*)" has the dedup field "([^"]*)" with its source witness$`, scenario.bundleHasDedup)
	sc.Step(`^the bundle operation "([^"]*)" has the read-back operation "([^"]*)" with its source witness$`, scenario.bundleHasReadBack)
	sc.Step(`^the bundle operation "([^"]*)" has the retry classification "([^"]*)"$`, scenario.bundleHasRetry)
}

func pmgenDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// pmgenObject is one JSON object of the synthetic OpenAPI document.
type pmgenObject = map[string]any

func pmgenItemSchema() pmgenObject {
	return pmgenObject{
		"type": "object", "additionalProperties": false, "required": []string{"id"},
		"properties": pmgenObject{"id": pmgenObject{"type": "string"}, "name": pmgenObject{"type": "string"}},
	}
}

// pmgenOpenAPI returns the synthetic OpenAPI document. The descriptions are
// the source facts that the witnesses point to.
func pmgenOpenAPI(writeID, readID string) pmgenObject {
	return pmgenObject{
		"openapi": "3.0.3",
		"info":    pmgenObject{"title": "Synthetic items API", "version": "1.0.0"},
		"servers": []pmgenObject{{"url": "https://api.items.example.test"}},
		"paths": pmgenObject{
			"/items": pmgenObject{"post": pmgenObject{
				"operationId": writeID,
				"description": "Creates one item. The provider keeps the first item for each client_request_id and returns it again for a repeated request.",
				"requestBody": pmgenObject{"required": true, "content": pmgenObject{"application/json": pmgenObject{
					"schema": pmgenObject{
						"type": "object", "additionalProperties": false, "required": []string{"name", "client_request_id"},
						"properties": pmgenObject{
							"name": pmgenObject{"type": "string"},
							"client_request_id": pmgenObject{
								"type":        "string",
								"description": "Provider deduplication key. A repeated request with the same value creates no second item.",
							},
						},
					},
				}}},
				"responses": pmgenObject{"201": pmgenObject{"description": "created", "content": pmgenObject{
					"application/json": pmgenObject{"schema": pmgenItemSchema()},
				}}},
			}},
			"/items/{id}": pmgenObject{"get": pmgenObject{
				"operationId": readID,
				"description": "Reads one item by its identifier.",
				"parameters": []pmgenObject{{
					"name": "id", "in": "path", "required": true, "schema": pmgenObject{"type": "string"},
				}},
				"responses": pmgenObject{"200": pmgenObject{"description": "the item", "content": pmgenObject{
					"application/json": pmgenObject{"schema": pmgenItemSchema()},
				}}},
			}},
		},
	}
}

func (s *pmgenScenario) path(name string) string { return filepath.Join(s.dir, name) }

// writeSource writes the synthetic OpenAPI capture, its license and the source
// lock. The semantics must repeat the source digest that pmgen computes from
// the lock, so the observer takes the digest from one compile without
// semantics.
func (s *pmgenScenario) writeSource(connectorID, writeID, readID string) error {
	dir, err := os.MkdirTemp("", "pm-acceptance-pmgen-")
	if err != nil {
		return fmt.Errorf("create the scenario directory: %w", err)
	}
	s.dir, s.connectorID, s.writeID, s.readID = dir, connectorID, writeID, readID
	capture, err := json.MarshalIndent(pmgenOpenAPI(writeID, readID), "", "  ")
	if err != nil {
		return fmt.Errorf("encode the OpenAPI capture: %w", err)
	}
	license := []byte("Synthetic test license. No rights reserved.\n")
	revision := "sha256:" + pmgenDigest([]byte(connectorID+" semantics"))
	lock := pmgenObject{
		"schema_version": pmgenSourceLockVersion,
		"connector_id":   connectorID,
		"source_set": pmgenObject{
			"dialect": "openapi-3.0",
			"documents": []pmgenObject{{
				"source_id":    pmgenSourceID,
				"dialect":      "openapi-3.0",
				"official_url": "https://items.example.test/openapi.json",
				"path":         "openapi.json",
				"captured_at":  "2026-10-31T00:00:00Z",
				"byte_length":  len(capture),
				"sha256":       pmgenDigest(capture),
				"license": pmgenObject{
					"spdx": "CC0-1.0", "path": "LICENSE.txt", "sha256": pmgenDigest(license), "notice_required": false,
				},
			}},
			"references_closed": true,
		},
		"operations": []pmgenObject{
			{
				"source_operation_id": writeID,
				"source_refs":         []pmgenSourceRef{{SourceID: pmgenSourceID, Pointer: pmgenWritePointer}},
				"request_variants":    []pmgenObject{{"pointer": pmgenWriteBodyPointer, "media_type": "application/json"}},
				"response_variants": []pmgenObject{{
					"pointer": "#/paths/~1items/post/responses/201/content/application~1json", "status": "201", "media_type": "application/json",
				}},
			},
			{
				"source_operation_id": readID,
				"source_refs":         []pmgenSourceRef{{SourceID: pmgenSourceID, Pointer: pmgenReadPointer}},
				"request_variants":    []pmgenObject{{"pointer": pmgenReadPointer, "media_type": "application/x.pm.parameters"}},
				"response_variants": []pmgenObject{{
					"pointer": "#/paths/~1items~1{id}/get/responses/200/content/application~1json", "status": "200", "media_type": "application/json",
				}},
			},
		},
		"semantics_revision": revision,
	}
	lockDocument, err := json.MarshalIndent(lock, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the source lock: %w", err)
	}
	for name, data := range map[string][]byte{
		pmgenSourceID: capture, "LICENSE.txt": license, "source.lock.json": lockDocument,
	} {
		if err := os.WriteFile(s.path(name), data, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	if err := os.Mkdir(s.path("digest"), 0o700); err != nil {
		return fmt.Errorf("create the digest directory: %w", err)
	}
	run, err := s.runPmgen(s.compileArguments("", s.path("digest/bundle.json"), s.path("digest/report.json"))...)
	if err != nil {
		return err
	}
	if run.exitCode != 0 {
		return fmt.Errorf("pmgen did not compile the source without semantics\n%s", run.describe())
	}
	bundle, err := readPmgenBundle(s.path("digest/bundle.json"))
	if err != nil {
		return err
	}
	s.semantics = pmgenSemantics{
		SchemaVersion: pmgenSemanticsVersion,
		ConnectorID:   connectorID,
		SourceDigest:  bundle.SourceDigest,
		Revision:      revision,
		Operations:    []*pmgenAmendment{},
	}
	return nil
}

func (s *pmgenScenario) compileArguments(semantics, bundle, report string) []string {
	arguments := []string{
		"compile",
		"--source-lock", s.path("source.lock.json"),
		"--capture", pmgenSourceID + "=" + s.path(pmgenSourceID),
		"--license", pmgenSourceID + "=" + s.path("LICENSE.txt"),
		"--bundle-out", bundle,
		"--report-out", report,
	}
	if semantics != "" {
		arguments = append(arguments, "--semantics", semantics)
	}
	return arguments
}

// operationPointer gives the source pointer of a synthetic operation.
func (s *pmgenScenario) operationPointer(operationID string) (string, error) {
	switch operationID {
	case "":
		return "", errors.New("the connector source was not written")
	case s.writeID:
		return pmgenWritePointer, nil
	case s.readID:
		return pmgenReadPointer, nil
	}
	return "", fmt.Errorf("the connector source has no operation %q", operationID)
}

// amendment returns the semantics amendment of one operation, and adds it
// when it is absent.
func (s *pmgenScenario) amendment(operationID string) (*pmgenAmendment, error) {
	pointer, err := s.operationPointer(operationID)
	if err != nil {
		return nil, err
	}
	for _, amendment := range s.semantics.Operations {
		if amendment.OperationID == operationID {
			return amendment, nil
		}
	}
	amendment := &pmgenAmendment{
		OperationID: operationID,
		SourceRefs:  []pmgenSourceRef{{SourceID: pmgenSourceID, Pointer: pointer}},
		Lanes:       []pmgenLane{},
	}
	s.semantics.Operations = append(s.semantics.Operations, amendment)
	return amendment, nil
}

func (s *pmgenScenario) markEligible(operationID, lane string) error {
	amendment, err := s.amendment(operationID)
	if err != nil {
		return err
	}
	amendment.Lanes = append(amendment.Lanes, pmgenLane{
		Lane:           lane,
		Classification: "eligible",
		SourceRefs:     slices.Clone(amendment.SourceRefs),
	})
	return nil
}

func (s *pmgenScenario) writeIdentity(operationID string) (*pmgenWriteIdentity, error) {
	amendment, err := s.amendment(operationID)
	if err != nil {
		return nil, err
	}
	if amendment.WriteIdentity == nil {
		amendment.WriteIdentity = &pmgenWriteIdentity{}
	}
	return amendment.WriteIdentity, nil
}

// declareDedup declares the dedup field. The witness of a field is its
// schema property in the source. The witness of "none" is the request body
// schema.
func (s *pmgenScenario) declareDedup(fieldID, operationID string) error {
	identity, err := s.writeIdentity(operationID)
	if err != nil {
		return err
	}
	pointer := pmgenDedupPointer
	if fieldID == pmgenNoDedupField {
		pointer = pmgenNoDedupPointer
	}
	identity.Dedup = &pmgenDedup{
		FieldID: fieldID,
		Witness: pmgenWitness{
			ProofID:    "items-create-dedup",
			SourceRefs: []pmgenSourceRef{{SourceID: pmgenSourceID, Pointer: pointer}},
		},
	}
	return nil
}

func (s *pmgenScenario) declareReadBack(readBackID, operationID string) error {
	pointer, err := s.operationPointer(readBackID)
	if err != nil {
		return err
	}
	identity, err := s.writeIdentity(operationID)
	if err != nil {
		return err
	}
	identity.ReadBack = &pmgenReadBack{
		OperationID: readBackID,
		Witness: pmgenWitness{
			ProofID:    "items-create-read-back",
			SourceRefs: []pmgenSourceRef{{SourceID: pmgenSourceID, Pointer: pointer}},
		},
	}
	return nil
}

func (s *pmgenScenario) declareRetry(classification, operationID string) error {
	amendment, err := s.amendment(operationID)
	if err != nil {
		return err
	}
	amendment.Retry = &pmgenRetry{
		Classification: classification,
		Witness: pmgenWitness{
			ProofID:    "items-create-retry",
			SourceRefs: slices.Clone(amendment.SourceRefs),
		},
	}
	return nil
}

// declareNoWriteIdentity checks that the semantics of the operation have no
// write identity, so the scenario text and the input agree.
func (s *pmgenScenario) declareNoWriteIdentity(operationID string) error {
	amendment, err := s.amendment(operationID)
	if err != nil {
		return err
	}
	if amendment.WriteIdentity != nil {
		return fmt.Errorf("the semantics of %q already declare a write identity", operationID)
	}
	return nil
}

func (s *pmgenScenario) compile() error {
	if s.dir == "" {
		return errors.New("the connector source was not written")
	}
	semantics, err := json.MarshalIndent(s.semantics, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the semantics: %w", err)
	}
	if err := os.WriteFile(s.path("semantics.json"), semantics, 0o600); err != nil {
		return fmt.Errorf("write the semantics: %w", err)
	}
	if err := os.Mkdir(s.path("out"), 0o700); err != nil {
		return fmt.Errorf("create the output directory: %w", err)
	}
	s.bundle, s.report = s.path("out/bundle.json"), s.path("out/report.json")
	run, err := s.runPmgen(s.compileArguments(s.path("semantics.json"), s.bundle, s.report)...)
	if err != nil {
		return err
	}
	s.last = run
	return nil
}

// checkCheckedIn regenerates every checked-in connector in memory and compares
// the result with the checkout. It writes nothing.
func (s *pmgenScenario) checkCheckedIn() error {
	root, err := pmgenRepositoryRoot()
	if err != nil {
		return err
	}
	run, err := s.runPmgen("connector", "--root", root, "--check")
	if err != nil {
		return err
	}
	s.last = run
	return nil
}

func (s *pmgenScenario) runPmgen(arguments ...string) (*pmgenRun, error) {
	binary, err := pmgenBinary()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, arguments...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	run := &pmgenRun{args: arguments, stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		run.exitCode = exitErr.ExitCode()
	default:
		return nil, fmt.Errorf("start pmgen: %w", err)
	}
	if ctx.Err() != nil {
		return nil, errors.New("pmgen did not finish within 2m")
	}
	return run, nil
}

func (s *pmgenScenario) lastRun() (*pmgenRun, error) {
	if s.last == nil {
		return nil, errors.New("pmgen did not run")
	}
	return s.last, nil
}

func (s *pmgenScenario) succeeds() error {
	run, err := s.lastRun()
	if err != nil {
		return err
	}
	if run.exitCode != 0 {
		return fmt.Errorf("pmgen did not succeed\n%s", run.describe())
	}
	return nil
}

// refuses requires a compile refusal that names the refused operation.
func (s *pmgenScenario) refuses(operationID string) error {
	run, err := s.lastRun()
	if err != nil {
		return err
	}
	if run.exitCode != 1 || !strings.HasPrefix(run.stderr, "pmgen: compile: ") {
		return fmt.Errorf("pmgen exit code %d, want 1 with a compile refusal\n%s", run.exitCode, run.describe())
	}
	if !strings.Contains(run.stderr, operationID) {
		return fmt.Errorf("the refusal does not name %q\n%s", operationID, run.describe())
	}
	return nil
}

func (s *pmgenScenario) writesNoBundle() error {
	for _, path := range []string{s.bundle, s.report} {
		_, err := os.Lstat(path)
		switch {
		case err == nil:
			return fmt.Errorf("pmgen wrote %s", path)
		case !errors.Is(err, os.ErrNotExist):
			return fmt.Errorf("inspect %s: %w", path, err)
		}
	}
	return nil
}

func readPmgenBundle(path string) (pmgenBundle, error) {
	var bundle pmgenBundle
	data, err := os.ReadFile(path)
	if err != nil {
		return bundle, fmt.Errorf("read the bundle: %w", err)
	}
	if err := json.Unmarshal(data, &bundle); err != nil {
		return bundle, fmt.Errorf("decode the bundle: %w", err)
	}
	return bundle, nil
}

// bundleWriteIdentity reads the write identity of one bundle operation, and
// the write identity that the semantics declared for it.
func (s *pmgenScenario) bundleWriteIdentity(operationID string) (*pmgenWriteIdentity, *pmgenWriteIdentity, error) {
	if err := s.succeeds(); err != nil {
		return nil, nil, err
	}
	declared := &pmgenWriteIdentity{}
	for _, amendment := range s.semantics.Operations {
		if amendment.OperationID == operationID && amendment.WriteIdentity != nil {
			declared = amendment.WriteIdentity
		}
	}
	bundle, err := readPmgenBundle(s.bundle)
	if err != nil {
		return nil, nil, err
	}
	for _, operation := range bundle.Operations {
		if operation.OperationID != operationID {
			continue
		}
		if operation.Execution == nil || operation.Execution.WriteIdentity == nil {
			return nil, nil, fmt.Errorf("the bundle operation %q has no execution.write_identity", operationID)
		}
		return operation.Execution.WriteIdentity, declared, nil
	}
	return nil, nil, fmt.Errorf("the bundle has no operation %q", operationID)
}

func (s *pmgenScenario) bundleHasDedup(operationID, fieldID string) error {
	actual, declared, err := s.bundleWriteIdentity(operationID)
	if err != nil {
		return err
	}
	if actual.Dedup == nil {
		return fmt.Errorf("the bundle operation %q has no dedup field", operationID)
	}
	if actual.Dedup.FieldID != fieldID {
		return fmt.Errorf("the bundle operation %q has the dedup field %q, want %q", operationID, actual.Dedup.FieldID, fieldID)
	}
	if declared.Dedup == nil || !actual.Dedup.Witness.equal(declared.Dedup.Witness) {
		return fmt.Errorf("the dedup witness of %q is %+v, the semantics declared %+v", operationID, actual.Dedup.Witness, declared.Dedup)
	}
	return nil
}

func (s *pmgenScenario) bundleHasReadBack(operationID, readBackID string) error {
	actual, declared, err := s.bundleWriteIdentity(operationID)
	if err != nil {
		return err
	}
	if actual.ReadBack == nil {
		return fmt.Errorf("the bundle operation %q has no read-back operation", operationID)
	}
	if actual.ReadBack.OperationID != readBackID {
		return fmt.Errorf("the bundle operation %q has the read-back operation %q, want %q",
			operationID, actual.ReadBack.OperationID, readBackID)
	}
	if declared.ReadBack == nil || !actual.ReadBack.Witness.equal(declared.ReadBack.Witness) {
		return fmt.Errorf("the read-back witness of %q is %+v, the semantics declared %+v",
			operationID, actual.ReadBack.Witness, declared.ReadBack)
	}
	return nil
}

func (s *pmgenScenario) bundleHasRetry(operationID, classification string) error {
	if err := s.succeeds(); err != nil {
		return err
	}
	bundle, err := readPmgenBundle(s.bundle)
	if err != nil {
		return err
	}
	for _, operation := range bundle.Operations {
		if operation.OperationID != operationID {
			continue
		}
		if operation.Execution == nil || operation.Execution.Retry == nil {
			return fmt.Errorf("the bundle operation %q has no execution.retry", operationID)
		}
		if operation.Execution.Retry.Classification != classification {
			return fmt.Errorf("the bundle operation %q has the retry classification %q, want %q",
				operationID, operation.Execution.Retry.Classification, classification)
		}
		return nil
	}
	return fmt.Errorf("the bundle has no operation %q", operationID)
}
