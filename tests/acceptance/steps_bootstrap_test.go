package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cucumber/godog"
)

const (
	// bootstrapCredentialMember is the member of the PostgreSQL secret bundle
	// that holds the bootstrap database credential: a connection string for
	// the role pm_app_bootstrap. The stack fixture writes it into the stack
	// secrets directory. pm-server bootstrap reads it from the secrets
	// directory of the deployment (ADR 0006).
	bootstrapCredentialMember = "pm-app-bootstrap-endpoint"
	// bootstrapAdminSubject and bootstrapSecondAdminSubject are the fixed
	// admin subjects of the bootstrap inputs. The issuer is the issuer of the
	// identity provider of the deployment. Both are provisional (LOGIN): they
	// change when the owner chooses the login mechanism.
	bootstrapAdminSubject       = "pm-acceptance-admin"
	bootstrapSecondAdminSubject = "pm-acceptance-admin-second"
	// bootstrapUndeclaredConnector is a connector ID that no deployment
	// declares.
	bootstrapUndeclaredConnector = "acceptance-undeclared"
)

// bootstrapTables are the tables of the bootstrap state: every workspace,
// identity, login binding, membership, provider account, bootstrap record
// and audit record on the stack. pm.workspace_audit_entries holds the audit
// records of a workspace that do not belong to a run, and pm.audit_entries
// holds the audit records of runs.
var bootstrapTables = []string{
	"workspace_namespaces",
	"identities",
	"auth_bootstrap_identities",
	"memberships",
	"provider_accounts",
	"bootstrap_records",
	"workspace_audit_entries",
	"audit_entries",
}

// bootstrapAuditTables are the audit tables of the bootstrap state.
var bootstrapAuditTables = []string{"workspace_audit_entries", "audit_entries"}

// bootstrapInput is the input file of pm-server bootstrap. It holds the
// organization, the workspace ID and name, the admin issuer and subject, and
// the provider accounts (ADR 0006, "The planned interface"). It holds no
// secret. The field names are those of the product input: one flat object,
// with every field present (edf079a, internal/app/bootstrap.go:83-93).
type bootstrapInput struct {
	OrgID            string                     `json:"org_id"`
	WorkspaceID      string                     `json:"workspace_id"`
	WorkspaceName    string                     `json:"workspace_name"`
	AdminIssuer      string                     `json:"admin_issuer"`
	AdminSubject     string                     `json:"admin_subject"`
	ProviderAccounts []bootstrapProviderAccount `json:"provider_accounts"`
}

type bootstrapProviderAccount struct {
	ConnectorID string `json:"connector_id"`
	DisplayName string `json:"display_name"`
}

// bootstrapRun is one run of pm-server bootstrap.
type bootstrapRun struct {
	deployment *deployment
	input      bootstrapInput
	exitCode   int
	stdout     string
	stderr     string
}

// bootstrapState is one observer reading: for each table, the rows as JSON
// text, in sorted order.
type bootstrapState map[string][]string

// bootstrapScenario is the state of the bootstrap steps in one scenario. The
// steps use the deployment of the shared deployment step. No step writes a PM
// row: the input is a file, and the credential step copies a file.
type bootstrapScenario struct {
	shared *stackScenario
	dir    string
	files  int
	runs   int

	// second is the second deployment of K5.
	second *deployment
	// observer reads through its own read-only role. It opens at the first
	// observer step.
	observer *observer

	// input is the current input, and inputFile is its file.
	input     *bootstrapInput
	inputFile string
	// first is the input of the first bootstrap of the scenario, and
	// firstFile is its file.
	first     *bootstrapInput
	firstFile string
	// last is the last run of pm-server bootstrap.
	last *bootstrapRun

	recorded      bootstrapState
	recordedScope string
}

func registerBootstrapSteps(sc *godog.ScenarioContext, shared *stackScenario) {
	s := &bootstrapScenario{shared: shared}
	// godog keeps the step error. The hook returns only its own cleanup
	// error, so the step error is reported once.
	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		return ctx, s.close()
	})
	sc.Step(`^its secrets directory (holds|does not hold) the bootstrap database credential$`, s.secretsDirectoryCredential)
	sc.Step(`^a second new PM deployment configuration on the job-owned stack, with the bootstrap database credential$`, s.secondDeployment)
	sc.Step(`^the (deployment|second deployment) configuration declares the "([^"]*)" connector bundle$`, s.declaresBundle)
	sc.Step(`^a bootstrap input with a new organization, a workspace, the admin bound to the login user of the stack identity provider and a "([^"]*)" provider account$`, s.newInput)
	sc.Step(`^a bootstrap input with (an empty workspace name|an admin subject that is empty|a provider account for a connector that the deployment does not declare)$`, s.invalidInput)
	sc.Step(`^a bootstrap input for a second organization in the same deployment, with another admin subject and a "([^"]*)" provider account$`, s.secondOrganizationInput)
	sc.Step(`^a bootstrap input for a second organization in the same deployment, with the admin issuer and subject of the first scope$`, s.secondOrganizationFirstAdmin)
	sc.Step(`^the operator runs pm-server bootstrap with that input$`, s.runWithInput)
	sc.Step(`^the operator runs pm-server bootstrap again with the same input$`, s.runAgain)
	sc.Step(`^the operator runs pm-server bootstrap for the second deployment$`, s.runForSecondDeployment)
	sc.Step(`^the operator bootstrapped the deployment with a "([^"]*)" provider account$`, s.bootstrapped)
	sc.Step(`^the bootstrap exits with code (\d+)$`, s.exitsWithCode)
	sc.Step(`^the bootstrap stdout is empty$`, s.stdoutIsEmpty)
	sc.Step(`^the bootstrap stdout is one JSON document that names the workspace, the admin identity, the admin membership and the provider account$`, s.stdoutNamesResult)
	sc.Step(`^the bootstrap stdout holds no value from the secrets directory$`, s.stdoutHoldsNoSecret)
	sc.Step(`^the bootstrap stderr is exactly the line: (.*)$`, s.stderrIsExactlyLine)
	sc.Step(`^the observer records the (bootstrap state of the stack|bootstrap state of the first scope)$`, s.records)
	sc.Step(`^the observer sees the bootstrap state of the stack unchanged since it recorded it, and no new audit record$`, s.stackUnchanged)
	sc.Step(`^the observer sees the bootstrap state of the first scope unchanged since it recorded it$`, s.firstScopeUnchanged)
	sc.Step(`^the observer sees exactly one new workspace, one new identity with its login binding for this deployment, one admin membership, one provider account, one bootstrap record for this deployment and organization, and one audit record that names the operator bootstrap as its actor$`, s.seesBootstrappedScope)
	sc.Step(`^the observer sees one new workspace, identity, login binding, admin membership, provider account and bootstrap record for the second scope, and one new audit record$`, s.seesSecondScope)
}

// deployment returns the deployment of the shared deployment step.
func (s *bootstrapScenario) deployment() (*deployment, error) {
	if s.shared.deployment == nil {
		return nil, errors.New("the scenario has no deployment configuration")
	}
	return s.shared.deployment, nil
}

// secretsDirectoryCredential copies the bootstrap credential from the stack
// secrets directory into the secrets directory of the deployment, or checks
// that the deployment secrets directory does not hold it. It creates no role
// and writes nothing to PostgreSQL.
func (s *bootstrapScenario) secretsDirectoryCredential(state string) error {
	d, err := s.deployment()
	if err != nil {
		return err
	}
	if state == "holds" {
		return copyBootstrapCredential(d)
	}
	path, err := bootstrapCredentialPath(d.secrets, d.stack.postgresRef)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("the deployment secrets directory holds the bootstrap database credential, or it cannot be read: %v", err)
	}
	return nil
}

func bootstrapCredentialPath(root, postgresRef string) (string, error) {
	bundle, err := secretBundlePath(postgresRef)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, bundle, bootstrapCredentialMember), nil
}

// copyBootstrapCredential copies the one credential file. It never writes
// the file into the stack secrets directory.
func copyBootstrapCredential(d *deployment) error {
	source, err := bootstrapCredentialPath(d.stack.secretsDir, d.stack.postgresRef)
	if err != nil {
		return err
	}
	contents, err := readBoundedFile(source, laneFileLimit)
	if err != nil {
		return fmt.Errorf("read the stack bootstrap database credential: %w", err)
	}
	bundle, err := secretBundlePath(d.stack.postgresRef)
	if err != nil {
		return err
	}
	return writeSecretMember(d.secrets, bundle, bootstrapCredentialMember, contents)
}

// secondDeployment is the second new deployment configuration of K5, with
// the bootstrap credential in its secrets directory.
func (s *bootstrapScenario) secondDeployment() error {
	if _, err := s.deployment(); err != nil {
		return err
	}
	if s.second != nil {
		return errors.New("the scenario already has a second deployment configuration")
	}
	stack, err := readStackInputs()
	if err != nil {
		return err
	}
	if s.second, err = newDeployment(stack); err != nil {
		return err
	}
	return copyBootstrapCredential(s.second)
}

// makeInput returns a new input for a new organization and workspace in the
// deployment. The admin is the fixed subject at the issuer of the identity
// provider of the deployment (LOGIN).
func makeInput(d *deployment, subject, connector string) bootstrapInput {
	suffix := randomHex(6)
	return bootstrapInput{
		OrgID:         "acceptance-org-" + suffix,
		WorkspaceID:   "acceptance-workspace-" + suffix,
		WorkspaceName: "Acceptance workspace " + suffix,
		AdminIssuer:   d.identity.issuer,
		AdminSubject:  subject,
		ProviderAccounts: []bootstrapProviderAccount{
			{ConnectorID: connector, DisplayName: "Acceptance " + connector + " account " + suffix},
		},
	}
}

// writeInput writes the input as a new file in the scenario directory and
// makes it the current input.
func (s *bootstrapScenario) writeInput(input bootstrapInput) error {
	directory, err := s.directory()
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(input, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the bootstrap input: %w", err)
	}
	s.files++
	path := filepath.Join(directory, fmt.Sprintf("bootstrap-input-%d.json", s.files))
	if err := writePrivateFile(path, append(encoded, '\n')); err != nil {
		return err
	}
	s.input, s.inputFile = &input, path
	return nil
}

// directory makes the scenario directory of the input and output files at
// the first call. close removes it.
func (s *bootstrapScenario) directory() (string, error) {
	if s.dir == "" {
		dir, err := os.MkdirTemp("", "pm-acceptance-bootstrap-")
		if err != nil {
			return "", fmt.Errorf("create the bootstrap scenario directory: %w", err)
		}
		s.dir = dir
	}
	return s.dir, nil
}

func (s *bootstrapScenario) newInput(connector string) error {
	d, err := s.deployment()
	if err != nil {
		return err
	}
	return s.writeInput(makeInput(d, bootstrapAdminSubject, connector))
}

// invalidInput is a valid input with one change of K3.
func (s *bootstrapScenario) invalidInput(change string) error {
	d, err := s.deployment()
	if err != nil {
		return err
	}
	input := makeInput(d, bootstrapAdminSubject, "postgres")
	switch change {
	case "an empty workspace name":
		input.WorkspaceName = ""
	case "an admin subject that is empty":
		input.AdminSubject = ""
	case "a provider account for a connector that the deployment does not declare":
		input.ProviderAccounts[0].ConnectorID = bootstrapUndeclaredConnector
	default:
		return fmt.Errorf("unknown bootstrap input change %q", change)
	}
	return s.writeInput(input)
}

// secondOrganizationInput is the K6 input: a new organization in the same
// deployment, with another admin subject at the same issuer.
func (s *bootstrapScenario) secondOrganizationInput(connector string) error {
	d, err := s.deployment()
	if err != nil {
		return err
	}
	if s.first == nil {
		return errors.New("the operator did not bootstrap the deployment in this scenario")
	}
	return s.writeInput(makeInput(d, bootstrapSecondAdminSubject, connector))
}

// secondOrganizationFirstAdmin is the K7 input: a new organization in the
// same deployment, with the admin issuer and subject of the first scope.
func (s *bootstrapScenario) secondOrganizationFirstAdmin() error {
	d, err := s.deployment()
	if err != nil {
		return err
	}
	if s.first == nil {
		return errors.New("the operator did not bootstrap the deployment in this scenario")
	}
	input := makeInput(d, bootstrapAdminSubject, s.first.ProviderAccounts[0].ConnectorID)
	input.AdminIssuer, input.AdminSubject = s.first.AdminIssuer, s.first.AdminSubject
	return s.writeInput(input)
}

// run runs pm-server bootstrap once as a separate process, with the clean
// environment of the deployment. It waits once for the exit, with the limit
// of one lane command, and records the exit code, stdout and stderr.
//
// stdout and stderr go to files that the step owns, as in the starter. A
// file needs no copy goroutine, so the wait ends with the exit of the
// process, or at the limit when exec kills it. A descendant that keeps a
// descriptor open cannot hold the step past the limit. The step reads each
// file once after the wait, at most laneFileLimit bytes.
func (s *bootstrapScenario) run(parent context.Context, d *deployment, input bootstrapInput, file string) error {
	binary, err := s.shared.harness.binary("pm-server")
	if err != nil {
		return err
	}
	configuration, err := d.configurationPath()
	if err != nil {
		return err
	}
	directory, err := s.directory()
	if err != nil {
		return err
	}
	s.runs++
	stdoutFile, err := os.Create(filepath.Join(directory, fmt.Sprintf("bootstrap-%d.stdout", s.runs)))
	if err != nil {
		return fmt.Errorf("create the bootstrap stdout file: %w", err)
	}
	defer stdoutFile.Close()
	stderrFile, err := os.Create(filepath.Join(directory, fmt.Sprintf("bootstrap-%d.stderr", s.runs)))
	if err != nil {
		return fmt.Errorf("create the bootstrap stderr file: %w", err)
	}
	defer stderrFile.Close()
	ctx, cancel := context.WithTimeout(parent, laneCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "bootstrap",
		"--config", configuration, "--secrets-directory", d.secrets, "--input", file)
	command.Env = d.processEnvironment()
	command.Dir = d.home()
	command.Stdout = stdoutFile
	command.Stderr = stderrFile
	runErr := command.Run()
	if ctx.Err() != nil {
		return fmt.Errorf("pm-server bootstrap did not finish within %s", laneCommandTimeout)
	}
	code := 0
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
		code = exitErr.ExitCode()
	default:
		return fmt.Errorf("start pm-server bootstrap: %w", runErr)
	}
	stdout, err := readOutputFile(stdoutFile, "stdout")
	if err != nil {
		return err
	}
	stderr, err := readOutputFile(stderrFile, "stderr")
	if err != nil {
		return err
	}
	s.last = &bootstrapRun{deployment: d, input: input, exitCode: code, stdout: stdout, stderr: stderr}
	return nil
}

// readOutputFile reads an output file from its start, at most laneFileLimit
// bytes. An empty file is an empty output. A larger file fails the step.
func readOutputFile(file *os.File, name string) (string, error) {
	contents, err := io.ReadAll(io.NewSectionReader(file, 0, laneFileLimit+1))
	if err != nil {
		return "", fmt.Errorf("read the bootstrap %s file: %w", name, err)
	}
	if len(contents) > laneFileLimit {
		return "", fmt.Errorf("the bootstrap %s is larger than %d bytes", name, laneFileLimit)
	}
	return string(contents), nil
}

// shownOutputLimit is the most bytes of one output that a message shows.
const shownOutputLimit = 4096

// shown returns one output of the run for a failure message. The output can
// hold a secret when the product is wrong, so the method reads the
// deployment secrets directory once and checks the whole output first
// (findSecret). It quotes clean output, at most shownOutputLimit bytes. It
// withholds output that holds a form of a secret, and output that it cannot
// check. The text names the file member and the form, never the value.
func (run *bootstrapRun) shown(output string) string {
	forms, err := readDeploymentSecrets(run.deployment.secrets)
	if err != nil {
		return fmt.Sprintf("(%d bytes, withheld: the deployment secrets directory could not be read)", len(output))
	}
	if source, found := findSecret([]byte(output), forms); found {
		return fmt.Sprintf("(%d bytes, withheld: it holds a value from the deployment secrets directory: %s)", len(output), source)
	}
	if len(output) > shownOutputLimit {
		return fmt.Sprintf("%q (the first %d of %d bytes)", output[:shownOutputLimit], shownOutputLimit, len(output))
	}
	return fmt.Sprintf("%q", output)
}

func (s *bootstrapScenario) runWithInput(ctx context.Context) error {
	d, err := s.deployment()
	if err != nil {
		return err
	}
	if s.input == nil {
		return errors.New("the scenario has no bootstrap input")
	}
	return s.run(ctx, d, *s.input, s.inputFile)
}

// runAgain runs the bootstrap with the same input file as the first
// bootstrap.
func (s *bootstrapScenario) runAgain(ctx context.Context) error {
	d, err := s.deployment()
	if err != nil {
		return err
	}
	if s.first == nil {
		return errors.New("the operator did not bootstrap the deployment in this scenario")
	}
	return s.run(ctx, d, *s.first, s.firstFile)
}

// runForSecondDeployment runs the bootstrap for the second deployment, with a
// new input in the form of the first: a new organization and workspace, the
// admin at the identity provider of the second deployment, and the provider
// account of the first input.
func (s *bootstrapScenario) runForSecondDeployment(ctx context.Context) error {
	if s.second == nil {
		return errors.New("the scenario has no second deployment configuration")
	}
	if s.first == nil {
		return errors.New("the operator did not bootstrap the deployment in this scenario")
	}
	if err := s.writeInput(makeInput(s.second, bootstrapAdminSubject, s.first.ProviderAccounts[0].ConnectorID)); err != nil {
		return err
	}
	return s.run(ctx, s.second, *s.input, s.inputFile)
}

// bootstrapped runs the first bootstrap of the scenario with a new input. It
// fails unless the bootstrap exits with code 0.
func (s *bootstrapScenario) bootstrapped(ctx context.Context, connector string) error {
	d, err := s.deployment()
	if err != nil {
		return err
	}
	if s.first != nil {
		return errors.New("the operator already bootstrapped the deployment in this scenario")
	}
	if err := s.writeInput(makeInput(d, bootstrapAdminSubject, connector)); err != nil {
		return err
	}
	if err := s.run(ctx, d, *s.input, s.inputFile); err != nil {
		return err
	}
	if s.last.exitCode != 0 {
		return fmt.Errorf("the bootstrap of the deployment exited with code %d, want 0\nstdout: %s\nstderr: %s",
			s.last.exitCode, s.last.shown(s.last.stdout), s.last.shown(s.last.stderr))
	}
	s.first, s.firstFile = s.input, s.inputFile
	return nil
}

func (s *bootstrapScenario) lastRun() (*bootstrapRun, error) {
	if s.last == nil {
		return nil, errors.New("pm-server bootstrap did not run in this scenario")
	}
	return s.last, nil
}

func (s *bootstrapScenario) exitsWithCode(want int) error {
	run, err := s.lastRun()
	if err != nil {
		return err
	}
	if run.exitCode != want {
		return fmt.Errorf("the bootstrap exited with code %d, want %d\nstdout: %s\nstderr: %s",
			run.exitCode, want, run.shown(run.stdout), run.shown(run.stderr))
	}
	return nil
}

func (s *bootstrapScenario) stdoutIsEmpty() error {
	run, err := s.lastRun()
	if err != nil {
		return err
	}
	if run.stdout != "" {
		return fmt.Errorf("the bootstrap stdout is %s, want no byte", run.shown(run.stdout))
	}
	return nil
}

// stderrIsExactlyLine compares the whole stderr with the line and one line
// feed, with no other byte.
func (s *bootstrapScenario) stderrIsExactlyLine(line string) error {
	run, err := s.lastRun()
	if err != nil {
		return err
	}
	if run.stderr != line+"\n" {
		return fmt.Errorf("the bootstrap stderr is %s, want exactly the line %q", run.shown(run.stderr), line+"\n")
	}
	return nil
}

// stdoutNamesResult decodes stdout as exactly one JSON object. The observer
// reads the scope of the run once. The object must hold, as string values,
// the workspace ID of the input and the IDs of the one admin identity, the
// one admin membership and the one provider account that the observer reads.
func (s *bootstrapScenario) stdoutNamesResult(ctx context.Context) error {
	run, err := s.lastRun()
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(run.stdout))
	var document map[string]any
	if err := decoder.Decode(&document); err != nil || document == nil {
		// The decoder error can quote a part of stdout, so the message
		// shows only the checked output.
		return fmt.Errorf("the bootstrap stdout is not one JSON object\nstdout: %s", run.shown(run.stdout))
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("the bootstrap stdout holds more than one JSON document\nstdout: %s", run.shown(run.stdout))
	}
	state, err := s.readState(ctx, run.input.OrgID)
	if err != nil {
		return err
	}
	var identity scopeIdentity
	var membership scopeMembership
	var account scopeProviderAccount
	if err := errors.Join(
		oneRow(state, "identities", "identities", &identity),
		oneRow(state, "memberships", "memberships", &membership),
		oneRow(state, "provider_accounts", "provider accounts", &account),
	); err != nil {
		return err
	}
	names := map[string]bool{}
	collectStrings(document, names)
	wanted := []struct{ what, id string }{
		{"the workspace", run.input.WorkspaceID},
		{"the admin identity", identity.IdentityID},
		{"the admin membership", membership.MembershipID},
		{"the provider account", account.ProviderAccountID},
	}
	for _, want := range wanted {
		if want.id == "" || !names[want.id] {
			return fmt.Errorf("the bootstrap stdout does not name %s %q\nstdout: %s", want.what, want.id, run.shown(run.stdout))
		}
	}
	return nil
}

// collectStrings adds every string value of a decoded JSON value.
func collectStrings(value any, names map[string]bool) {
	switch typed := value.(type) {
	case string:
		names[typed] = true
	case []any:
		for _, item := range typed {
			collectStrings(item, names)
		}
	case map[string]any:
		for _, item := range typed {
			collectStrings(item, names)
		}
	}
}

// stdoutHoldsNoSecret reads each file of the deployment secrets directory
// once. stdout must hold no form of a secret value (secretForms), in its raw
// bytes or in a decoded JSON string. The message names the file member and
// the form, never the value.
func (s *bootstrapScenario) stdoutHoldsNoSecret() error {
	run, err := s.lastRun()
	if err != nil {
		return err
	}
	forms, err := readDeploymentSecrets(run.deployment.secrets)
	if err != nil {
		return err
	}
	if source, found := findSecret([]byte(run.stdout), forms); found {
		return fmt.Errorf("the bootstrap stdout holds a value from the deployment secrets directory: %s", source)
	}
	return nil
}

// ensureObserver opens the bootstrap observer at the first observer step.
func (s *bootstrapScenario) ensureObserver(ctx context.Context) (*observer, error) {
	if s.observer != nil {
		return s.observer, nil
	}
	d, err := s.deployment()
	if err != nil {
		return nil, err
	}
	if s.observer, err = openBootstrapObserver(ctx, d.stack); err != nil {
		return nil, err
	}
	return s.observer, nil
}

// readState reads the bootstrap state once. An empty organization reads the
// whole stack. Another organization reads only the rows of that organization.
func (s *bootstrapScenario) readState(ctx context.Context, orgID string) (bootstrapState, error) {
	o, err := s.ensureObserver(ctx)
	if err != nil {
		return nil, err
	}
	return o.readBootstrapState(ctx, orgID)
}

// records reads the bootstrap state of the stack or of the first scope once.
func (s *bootstrapScenario) records(ctx context.Context, what string) error {
	if s.recorded != nil {
		return errors.New("the observer already recorded a bootstrap state in this scenario")
	}
	scope := ""
	if what == "bootstrap state of the first scope" {
		if s.first == nil {
			return errors.New("the operator did not bootstrap the deployment in this scenario")
		}
		scope = s.first.OrgID
	}
	state, err := s.readState(ctx, scope)
	if err != nil {
		return err
	}
	s.recorded, s.recordedScope = state, scope
	return nil
}

func (s *bootstrapScenario) stackUnchanged(ctx context.Context) error {
	return s.unchanged(ctx, "")
}

func (s *bootstrapScenario) firstScopeUnchanged(ctx context.Context) error {
	if s.first == nil {
		return errors.New("the operator did not bootstrap the deployment in this scenario")
	}
	return s.unchanged(ctx, s.first.OrgID)
}

// unchanged reads the recorded state once more and compares the two
// readings. Each row added, changed or removed fails the step.
func (s *bootstrapScenario) unchanged(ctx context.Context, scope string) error {
	if s.recorded == nil || s.recordedScope != scope {
		return errors.New("the observer did not record this bootstrap state")
	}
	after, err := s.readState(ctx, scope)
	if err != nil {
		return err
	}
	var changes []string
	for _, table := range bootstrapTables {
		added, removed := rowDifference(s.recorded[table], after[table])
		if len(added) == 0 && len(removed) == 0 {
			continue
		}
		kind := "rows"
		if slices.Contains(bootstrapAuditTables, table) {
			kind = "audit records"
		}
		changes = append(changes, fmt.Sprintf("pm.%s: %d new %s %s, %d removed %s %s",
			table, len(added), kind, added, len(removed), kind, removed))
	}
	if len(changes) > 0 {
		return fmt.Errorf("the observer sees the bootstrap state changed since it recorded it:\n%s", strings.Join(changes, "\n"))
	}
	return nil
}

// rowDifference returns the rows of after that are not in before, and the
// rows of before that are not in after. Both lists are sorted.
func rowDifference(before, after []string) ([]string, []string) {
	var added, removed []string
	for _, row := range after {
		if _, found := slices.BinarySearch(before, row); !found {
			added = append(added, row)
		}
	}
	for _, row := range before {
		if _, found := slices.BinarySearch(after, row); !found {
			removed = append(removed, row)
		}
	}
	return added, removed
}

// The rows of one scope, decoded from the JSON text of the observer reading.
type (
	scopeWorkspace struct {
		WorkspaceID string `json:"workspace_id"`
		Name        string `json:"name"`
	}
	scopeIdentity struct {
		IdentityID string `json:"identity_id"`
		Kind       string `json:"kind"`
		Issuer     string `json:"issuer"`
		Subject    string `json:"subject"`
	}
	scopeLoginBinding struct {
		DeploymentID  string `json:"deployment_id"`
		Issuer        string `json:"issuer"`
		Subject       string `json:"subject"`
		PrincipalID   string `json:"principal_id"`
		PrincipalKind string `json:"principal_kind"`
		Enabled       bool   `json:"enabled"`
	}
	scopeMembership struct {
		WorkspaceID    string   `json:"workspace_id"`
		MembershipID   string   `json:"membership_id"`
		IdentityID     string   `json:"identity_id"`
		Role           string   `json:"role"`
		ResourceGrants []string `json:"resource_grants"`
		Permissions    []string `json:"permissions"`
		ConnectionIDs  []string `json:"connection_ids"`
		ExpiresAt      *string  `json:"expires_at"`
		RevokedAt      *string  `json:"revoked_at"`
	}
	scopeProviderAccount struct {
		ProviderAccountID string `json:"provider_account_id"`
		Provider          string `json:"provider"`
	}
	scopeBootstrapRecord struct {
		DeploymentID    string `json:"deployment_id"`
		WorkspaceID     string `json:"workspace_id"`
		AdminIdentityID string `json:"admin_identity_id"`
		MembershipID    string `json:"membership_id"`
	}
	scopeAudit struct {
		RequestedActorKind string `json:"requested_actor_kind"`
		EffectiveActorKind string `json:"effective_actor_kind"`
	}
	scope struct {
		workspace  scopeWorkspace
		identity   scopeIdentity
		binding    scopeLoginBinding
		membership scopeMembership
		account    scopeProviderAccount
		record     scopeBootstrapRecord
		audit      scopeAudit
		runAudits  int
	}
)

// oneRow decodes the one row of a table. It fails unless the table has
// exactly one row in the reading.
func oneRow(state bootstrapState, table, what string, target any) error {
	rows := state[table]
	if len(rows) != 1 {
		return fmt.Errorf("the observer sees %d %s in the scope (pm.%s), want exactly one: %s", len(rows), what, table, rows)
	}
	if err := json.Unmarshal([]byte(rows[0]), target); err != nil {
		return fmt.Errorf("decode the pm.%s row: %w", table, err)
	}
	return nil
}

// scopeRows decodes the one row of each kind in a scope reading.
func scopeRows(state bootstrapState) (scope, error) {
	var result scope
	rows := []struct {
		table, what string
		target      any
	}{
		{"workspace_namespaces", "workspaces", &result.workspace},
		{"identities", "identities", &result.identity},
		{"auth_bootstrap_identities", "login bindings", &result.binding},
		{"memberships", "memberships", &result.membership},
		{"provider_accounts", "provider accounts", &result.account},
		{"bootstrap_records", "bootstrap records", &result.record},
		{"workspace_audit_entries", "workspace audit records", &result.audit},
	}
	for _, row := range rows {
		if err := oneRow(state, row.table, row.what, row.target); err != nil {
			return scope{}, err
		}
	}
	result.runAudits = len(state["audit_entries"])
	return result, nil
}

// bootstrapAdminPermissions are the permissions of the admin membership.
// ADR 0006 (line 70): the admin membership holds the right to manage
// memberships and connections. The product checks workspace.access.manage
// before it changes a membership (bindAndAuthorizeAccessMutation in
// internal/app/workspaces.go) and connection.manage before it changes a
// connection (internal/app/connections.go).
var bootstrapAdminPermissions = []string{"connection.manage", "workspace.access.manage"}

// adminAuthorityProblem describes why the membership is not the admin
// membership of ADR 0006, or returns "". The membership must have a role,
// hold each admin permission both as a permission and as a "permission:"
// resource grant, list no connection at the start, and have no expiry and
// no revocation. ADR 0006 fixes no role text, so the check compares none.
func adminAuthorityProblem(membership scopeMembership) string {
	if membership.Role == "" {
		return "it has no role"
	}
	for _, permission := range bootstrapAdminPermissions {
		if !slices.Contains(membership.Permissions, permission) {
			return fmt.Sprintf("its permissions do not hold %q", permission)
		}
		if !slices.Contains(membership.ResourceGrants, "permission:"+permission) {
			return fmt.Sprintf("its resource grants do not hold %q", "permission:"+permission)
		}
	}
	if len(membership.ConnectionIDs) != 0 {
		return "it lists a connection at the start"
	}
	for _, grant := range membership.ResourceGrants {
		if strings.HasPrefix(grant, "connection:") {
			return "its resource grants list a connection at the start"
		}
	}
	if membership.ExpiresAt != nil || membership.RevokedAt != nil {
		return "it has an expiry or a revocation"
	}
	return ""
}

// checkScope checks that the scope of the run holds exactly the rows of one
// bootstrap of its input in its deployment, and one audit record. With
// operator true, the audit record must name the operator as its requested
// and effective actor.
func checkScope(state bootstrapState, run *bootstrapRun, operator bool) error {
	got, err := scopeRows(state)
	if err != nil {
		return err
	}
	input := run.input
	deploymentID := run.deployment.deploymentID
	var problems []string
	check := func(ok bool, format string, args ...any) {
		if !ok {
			problems = append(problems, fmt.Sprintf(format, args...))
		}
	}
	check(got.workspace.WorkspaceID == input.WorkspaceID && got.workspace.Name == input.WorkspaceName,
		"the workspace is %+v, want ID %q and name %q", got.workspace, input.WorkspaceID, input.WorkspaceName)
	check(got.identity.Kind == "user" && got.identity.Issuer == input.AdminIssuer && got.identity.Subject == input.AdminSubject,
		"the identity is %+v, want a user with the admin issuer and subject of the input", got.identity)
	check(got.binding.DeploymentID == deploymentID && got.binding.Issuer == input.AdminIssuer &&
		got.binding.Subject == input.AdminSubject && got.binding.PrincipalID == got.identity.IdentityID &&
		got.binding.PrincipalKind == "user" && got.binding.Enabled,
		"the login binding is %+v, want an enabled binding of deployment %q for the admin issuer, subject and identity", got.binding, deploymentID)
	check(got.membership.WorkspaceID == input.WorkspaceID && got.membership.IdentityID == got.identity.IdentityID,
		"the membership is %+v, want the membership of the admin identity in the workspace", got.membership)
	if problem := adminAuthorityProblem(got.membership); problem != "" {
		problems = append(problems, fmt.Sprintf("the membership is %+v, but %s", got.membership, problem))
	}
	check(got.account.Provider == input.ProviderAccounts[0].ConnectorID,
		"the provider account is %+v, want a provider account for %q", got.account, input.ProviderAccounts[0].ConnectorID)
	check(got.record.DeploymentID == deploymentID && got.record.WorkspaceID == input.WorkspaceID &&
		got.record.AdminIdentityID == got.identity.IdentityID && got.record.MembershipID == got.membership.MembershipID,
		"the bootstrap record is %+v, want the record of deployment %q that names the workspace, the admin identity and the admin membership", got.record, deploymentID)
	check(got.runAudits == 0, "the observer sees %d run audit records in the scope, want none", got.runAudits)
	if operator {
		check(got.audit.RequestedActorKind == "operator" && got.audit.EffectiveActorKind == "operator",
			"the audit record is %+v, want the operator as its requested and effective actor", got.audit)
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

// seesBootstrappedScope reads the scope of the last run once. The input made
// a new organization, so each row of that organization is new.
func (s *bootstrapScenario) seesBootstrappedScope(ctx context.Context) error {
	run, err := s.lastRun()
	if err != nil {
		return err
	}
	state, err := s.readState(ctx, run.input.OrgID)
	if err != nil {
		return err
	}
	return checkScope(state, run, true)
}

// seesSecondScope reads the scope of the last run once: the second scope of
// K5 or K6.
func (s *bootstrapScenario) seesSecondScope(ctx context.Context) error {
	run, err := s.lastRun()
	if err != nil {
		return err
	}
	if s.first == nil || run.input.OrgID == s.first.OrgID {
		return errors.New("the last bootstrap did not run for a second scope")
	}
	state, err := s.readState(ctx, run.input.OrgID)
	if err != nil {
		return err
	}
	return checkScope(state, run, false)
}

// close closes the second deployment and the observer, and removes the
// input directory.
func (s *bootstrapScenario) close() error {
	var errs []error
	if s.second != nil {
		errs = append(errs, s.second.close())
	}
	errs = append(errs, s.observer.close())
	if s.dir != "" {
		errs = append(errs, os.RemoveAll(s.dir))
	}
	return errors.Join(errs...)
}
