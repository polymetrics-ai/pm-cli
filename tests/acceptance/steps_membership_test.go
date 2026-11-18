package acceptance

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/cucumber/godog"
)

// The steps of slice 284c: an admin adds a connection to a membership
// (criteria-worker v4, lines 270-317 and the step list at lines 556-566).
// Erratum E1 applies: each "pm memberships <verb>" of the criteria reads
// "pm access memberships <verb>".
//
// The user acts only through product paths: the real pm binary with the PM
// session that the login step stored, and the credential page request of the
// user's browser session. The user takes the organization, workspace,
// identity, membership and provider account IDs from the bootstrap result that
// the operator gives it. No step writes a PM row. The observer reads through
// its own read-only role only.
//
// A command runs with the user's PM session, so a failure message shows its
// stdout only by its byte count and its stderr only by its form (#402 review
// F1). The fixed pm lines in knownMembershipLines hold no value, so a message
// shows them whole.

// operationCredentialPage names the credential page request in a failure
// message, by its API path only.
const operationCredentialPage = "credential page (POST /admin/v1/workspaces/{workspace_id}/connections/{connection_id}/credentials)"

// knownMembershipLines are the pm stderr lines that a failure message can
// show whole. Each is a fixed text of pm with no value in it: the two
// required-flag lines of the update command (#332), the line of an unknown
// flag for each flag that these steps pass, and the session line.
var knownMembershipLines = []string{
	"error: --revision is required",
	"error: --add-connections is required",
	"error: unknown flag: --add-connections",
	"error: unknown flag: --revision",
	"error: unknown flag: --workspace",
	"error: unknown flag: --output",
	"error: unknown flag: --connector-id",
	"error: unknown flag: --provider-account-id",
	"error: unknown flag: --display-name",
	"error: PM session is required",
}

// controlRevision is the form of a control revision in the public API
// (api/pm.openapi.yaml, Membership.control_revision).
var controlRevision = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

// membershipScope holds the IDs of the bootstrap result that the operator
// gives the user.
type membershipScope struct {
	orgID             string
	workspaceID       string
	identityID        string
	membershipID      string
	providerAccountID string
}

// userConnection is the connection that the user created.
type userConnection struct {
	connectionID  string
	credentialRef string
}

// membershipCommand is the last update command that the steps ran as the
// lane command: the shared result state that the step wrote, and the whole
// stderr of the command.
type membershipCommand struct {
	result commandResult
	stderr string
}

// membershipScenario is the state of the 284c steps in one scenario.
type membershipScenario struct {
	shared    *stackScenario
	lane      *lane
	bootstrap *bootstrapScenario
	login     *loginScenario

	scope      *membershipScope
	connection *userConnection
	// observer reads through its own read-only role. It opens at the first
	// observer reading of the scenario.
	observer *observer
	// baseline is the membership state that the observer read just before the
	// update with the current membership revision.
	baseline membershipState
	// recorded is the membership state of the step "the observer records the
	// membership state of the workspace".
	recorded membershipState
	last     *membershipCommand
}

// registerMembershipSteps registers the 284c steps. The After hook is
// registered here, before any step runs. It closes the observer connection
// and drops the observer role, also after a failed step.
func registerMembershipSteps(sc *godog.ScenarioContext, shared *stackScenario, l *lane, bootstrap *bootstrapScenario, login *loginScenario) {
	s := &membershipScenario{shared: shared, lane: l, bootstrap: bootstrap, login: login}
	// godog keeps the step error. The hook returns only its own cleanup
	// error, so the step error is reported once.
	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		return ctx, s.close()
	})
	sc.Step(`^the user created a PostgreSQL connection with "pm connections create"$`, s.createConnection)
	sc.Step(`^the credential page refuses a credential of the user for that connection, and the observer sees no stored credential for it$`, s.credentialPageRefuses)
	sc.Step(`^the user runs "pm access memberships update" to add that connection to its own membership, with (the current membership revision|no revision)$`, s.runUpdate)
	sc.Step(`^the user runs "pm access memberships update" on its own membership, with the revision before the current revision$`, s.runUpdateWithStaleRevision)
	sc.Step(`^the user added that connection to its own membership with "pm access memberships update"$`, s.addedConnection)
	sc.Step(`^the observer records the membership state of the workspace$`, s.records)
	sc.Step(`^the observer sees the membership state of the workspace unchanged since it recorded it, and no new audit record$`, s.unchanged)
	sc.Step(`^the observer sees that the membership grants list that connection, that its other grants did not change, and that its revision is one higher$`, s.grantsListConnection)
	sc.Step(`^the observer sees one audit record of the update that names the user as its actor$`, s.oneAuditRecord)
	sc.Step(`^the user can store the customer role password through the product credential page for that connection$`, s.storesPassword)
	sc.Step(`^the lane command stderr starts with "([^"]*)"$`, s.stderrStartsWith)
	sc.Step(`^the lane command stderr is exactly the line: (.*)$`, s.stderrIsExactlyLine)
}

// bootstrapResult is the part of the pm-server bootstrap stdout that the user
// reads (internal/app/bootstrap.go, BootstrapResult).
type bootstrapResult struct {
	OrgID     string `json:"org_id"`
	Workspace struct {
		WorkspaceID string `json:"workspace_id"`
	} `json:"workspace"`
	Admin struct {
		IdentityID string `json:"identity_id"`
	} `json:"admin"`
	Membership struct {
		MembershipID string `json:"membership_id"`
	} `json:"membership"`
	ProviderAccounts []struct {
		ProviderAccountID string `json:"provider_account_id"`
		ConnectorID       string `json:"connector_id"`
	} `json:"provider_accounts"`
}

// scopeIDs reads the IDs of the bootstrap result once: the first bootstrap of
// the scenario, for the lane deployment, with one provider account of the
// lane connector bundle. A failure message never shows the bootstrap stdout.
func (s *membershipScenario) scopeIDs() (*membershipScope, error) {
	if s.scope != nil {
		return s.scope, nil
	}
	b := s.bootstrap
	if b.first == nil || b.last == nil || b.last.input.OrgID != b.first.OrgID || b.last.exitCode != 0 {
		return nil, errors.New("the operator did not bootstrap the deployment in this scenario")
	}
	if !b.isLaneDeployment(b.last.deployment) {
		return nil, errors.New("the bootstrap of this scenario is not for the lane deployment")
	}
	var result bootstrapResult
	if err := json.Unmarshal([]byte(b.last.stdout), &result); err != nil {
		return nil, fmt.Errorf("the bootstrap stdout is not the bootstrap result\nstdout: %s", describeOutput(b.last.stdout))
	}
	scope := &membershipScope{
		orgID:        result.OrgID,
		workspaceID:  result.Workspace.WorkspaceID,
		identityID:   result.Admin.IdentityID,
		membershipID: result.Membership.MembershipID,
	}
	if len(result.ProviderAccounts) == 1 && result.ProviderAccounts[0].ConnectorID == laneConnectorBundle {
		scope.providerAccountID = result.ProviderAccounts[0].ProviderAccountID
	}
	if scope.orgID != b.first.OrgID || scope.workspaceID != b.first.WorkspaceID || scope.identityID == "" ||
		scope.membershipID == "" || scope.providerAccountID == "" {
		return nil, fmt.Errorf("the bootstrap result does not name the organization and workspace of the input, the admin identity, the admin membership and one %q provider account\nstdout: %s",
			laneConnectorBundle, describeOutput(b.last.stdout))
	}
	s.scope = scope
	return scope, nil
}

// createdConnection returns the connection that the user created.
func (s *membershipScenario) createdConnection() (*userConnection, error) {
	if s.connection == nil {
		return nil, errors.New("the user did not create a connection in this scenario")
	}
	return s.connection, nil
}

// runAsUser runs one pm process as the user: the scenario HOME, the lane
// deployment CA, the lane profile and the private keyring with the PM session
// of the login step (loginScenario.environment). runPMProcess waits once for
// the exit, with the limit of one lane command, and ends the process inside
// the step.
func (s *membershipScenario) runAsUser(ctx context.Context, args []string) (int, string, string, error) {
	if s.login == nil || !s.login.loggedIn {
		return 0, "", "", errors.New("the user is not logged in in this scenario")
	}
	if err := s.lane.ensure(ctx); err != nil {
		return 0, "", "", err
	}
	environment, err := s.login.environment()
	if err != nil {
		return 0, "", "", err
	}
	return runPMProcess(ctx, s.shared.harness.bins.pm, args, environment)
}

// describeCommand describes one pm command for a failure message: its exit
// code, its stdout by byte count and its stderr by form.
func describeCommand(name string, code int, stdout, stderr string) string {
	return fmt.Sprintf("%s exited with code %d\nstdout: %d bytes, not shown\n%s", name, code, len(stdout), describeMembershipLines("stderr", stderr))
}

// describeMembershipLines describes one output stream for a failure message.
// It shows a line of knownMembershipLines whole, and every other line by its
// form (describeSafeLine).
func describeMembershipLines(name, text string) string {
	if text == "" {
		return name + ": 0 bytes"
	}
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	parts := make([]string, 0, min(len(lines), describedLineLimit+1))
	for index, raw := range lines {
		if index == describedLineLimit {
			parts = append(parts, fmt.Sprintf("%d more lines, not shown", len(lines)-index))
			break
		}
		line, complete := strings.CutSuffix(raw, "\n")
		if slices.Contains(knownMembershipLines, line) {
			suffix := ""
			if !complete {
				suffix = " (no line feed)"
			}
			parts = append(parts, fmt.Sprintf("%q%s", line, suffix))
			continue
		}
		parts = append(parts, describeSafeLine(line, complete))
	}
	return fmt.Sprintf("%s (%d bytes, line count %d, shown by form only):\n  %s", name, len(text), len(lines), strings.Join(parts, "\n  "))
}

// readJSONOutput decodes the stdout of a pm command with --output json as
// exactly one JSON document. A failure message never shows the stdout.
func readJSONOutput(name, stdout string, target any) error {
	decoder := json.NewDecoder(strings.NewReader(stdout))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("the %s stdout (%d bytes, not shown) is not the JSON document that the step reads", name, len(stdout))
	}
	if decoder.More() {
		return fmt.Errorf("the %s stdout (%d bytes, not shown) holds more than one JSON document", name, len(stdout))
	}
	return nil
}

// connectionCollectionRevision reads the connection collection revision of
// the workspace with pm connections list. pm connections create needs it as
// its expected revision (internal/app/connections.go, CreateConnection).
func (s *membershipScenario) connectionCollectionRevision(ctx context.Context, scope *membershipScope) (string, error) {
	code, stdout, stderr, err := s.runAsUser(ctx, []string{"connections", "list", "--workspace", scope.workspaceID, "--output", "json"})
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", errors.New(describeCommand("pm connections list", code, stdout, stderr))
	}
	var page struct {
		CollectionRevision string `json:"collection_revision"`
	}
	if err := readJSONOutput("pm connections list", stdout, &page); err != nil {
		return "", err
	}
	if !controlRevision.MatchString(page.CollectionRevision) {
		return "", fmt.Errorf("pm connections list gave no valid collection revision (stdout: %d bytes, not shown)", len(stdout))
	}
	return page.CollectionRevision, nil
}

// createConnection is the Background step "the user created a PostgreSQL
// connection with "pm connections create"". The user reads the connection
// collection revision, then creates a connection of the lane connector bundle
// for the provider account of the bootstrap result. The step needs exit code
// 0 and one connection with that connector and provider account. It keeps
// the connection ID and its credential reference.
func (s *membershipScenario) createConnection(ctx context.Context) error {
	if s.connection != nil {
		return errors.New("the user already created a connection in this scenario")
	}
	scope, err := s.scopeIDs()
	if err != nil {
		return err
	}
	revision, err := s.connectionCollectionRevision(ctx, scope)
	if err != nil {
		return err
	}
	code, stdout, stderr, err := s.runAsUser(ctx, []string{
		"connections", "create", "--workspace", scope.workspaceID, "--output", "json",
		"--connector-id", laneConnectorBundle,
		"--provider-account-id", scope.providerAccountID,
		"--display-name", "acceptance-postgres-" + randomHex(6),
		"--revision", revision,
	})
	if err != nil {
		return err
	}
	if code != 0 {
		return errors.New(describeCommand("pm connections create", code, stdout, stderr))
	}
	var created struct {
		Connection struct {
			WorkspaceID       string `json:"workspace_id"`
			ConnectionID      string `json:"connection_id"`
			ConnectorID       string `json:"connector_id"`
			ProviderAccountID string `json:"provider_account_id"`
			CredentialRef     string `json:"credential_ref"`
		} `json:"connection"`
	}
	if err := readJSONOutput("pm connections create", stdout, &created); err != nil {
		return err
	}
	connection := created.Connection
	if connection.ConnectionID == "" || connection.CredentialRef == "" || connection.WorkspaceID != scope.workspaceID ||
		connection.ConnectorID != laneConnectorBundle || connection.ProviderAccountID != scope.providerAccountID {
		return fmt.Errorf("pm connections create did not print one %q connection of the workspace for the bootstrap provider account, with a connection ID and a credential reference (stdout: %d bytes, not shown)",
			laneConnectorBundle, len(stdout))
	}
	s.connection = &userConnection{connectionID: connection.ConnectionID, credentialRef: connection.CredentialRef}
	godog.Logf(ctx, "the user created the connection %s with the connection collection revision %s", connection.ConnectionID, revision)
	return nil
}

// ensureObserver opens the membership observer at the first observer reading
// of the scenario.
func (s *membershipScenario) ensureObserver(ctx context.Context) (*observer, error) {
	if s.observer != nil {
		return s.observer, nil
	}
	if err := s.lane.running(); err != nil {
		return nil, err
	}
	o, err := openMembershipObserver(ctx, s.lane.deployment.stack)
	if err != nil {
		return nil, err
	}
	s.observer = o
	return o, nil
}

// readState reads the membership state of the workspace once.
func (s *membershipScenario) readState(ctx context.Context, scope *membershipScope) (membershipState, error) {
	o, err := s.ensureObserver(ctx)
	if err != nil {
		return nil, err
	}
	return o.readMembershipState(ctx, scope.orgID, scope.workspaceID)
}

// readCredentials reads the stored credential of the connection once.
func (s *membershipScenario) readCredentials(ctx context.Context, scope *membershipScope, connection *userConnection, secret []byte) (storedCredentials, error) {
	o, err := s.ensureObserver(ctx)
	if err != nil {
		return storedCredentials{}, err
	}
	return o.readStoredCredentials(ctx, scope.orgID, scope.workspaceID, connection.connectionID, secret)
}

// sendCredential sends the request of the product credential page
// (internal/server/browser_assets/credentials.js) with the user's browser
// session from the login step: the session cookie, the exact origin, the
// session-bound CSRF token, a new idempotency key and the secret as base64,
// with no scope. It waits once for the answer, with the limit of one lane
// command (browserSession.send). It clears its copy of the request body.
func (s *membershipScenario) sendCredential(ctx context.Context, scope *membershipScope, connection *userConnection, secret []byte) (int, []byte, error) {
	if s.login == nil || s.login.browser == nil || s.login.browser.csrf == "" {
		return 0, nil, errors.New("the user has no browser session from the product login flow in this scenario")
	}
	if err := s.lane.ensure(ctx); err != nil {
		return 0, nil, err
	}
	browser := s.login.browser
	body, err := json.Marshal(struct {
		CredentialValue []byte   `json:"credential_value"`
		Scopes          []string `json:"scopes"`
	}{CredentialValue: secret, Scopes: []string{}})
	if err != nil {
		return 0, nil, errors.New("encode the credential page request")
	}
	defer clear(body)
	header := jsonHeaders(browser.origin)
	header.Set("X-CSRF-Token", browser.csrf)
	header.Set("Idempotency-Key", "acceptance-credential-"+randomHex(8))
	target := browser.origin + "/admin/v1/workspaces/" + url.PathEscape(scope.workspaceID) +
		"/connections/" + url.PathEscape(connection.connectionID) + "/credentials"
	status, _, answer, err := browser.send(ctx, operationCredentialPage, http.MethodPost, target, body, header)
	if err != nil {
		return 0, nil, err
	}
	return status, answer, nil
}

// answerHoldsSecret reports whether a credential page answer holds the
// secret, raw or as base64.
func answerHoldsSecret(answer, secret []byte) bool {
	encoded := []byte(base64.StdEncoding.EncodeToString(secret))
	defer clear(encoded)
	return bytes.Contains(answer, secret) || bytes.Contains(answer, encoded)
}

// problemCode returns the problem code of a PM problem document, or "" when
// the answer is not one.
func problemCode(answer []byte) string {
	var problem struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(answer, &problem) != nil || problem.Code == "" || len(problem.Code) > 64 ||
		strings.ContainsFunc(problem.Code, func(r rune) bool { return !(r == '_' || r >= 'a' && r <= 'z') }) {
		return ""
	}
	return problem.Code
}

// credentialPageRefuses is M1's first Given. The bootstrap admin's grants list
// no connection (ADR 0006), so the credential page refuses the user for the
// new connection: status 403 with grant_unavailable, or 404 with
// resource_not_found (criteria-worker v4, line 314). A 404 that is not a PM
// problem document, for example from a missing route, does not pass. Then the
// observer reads the stored credential of the connection once and sees none.
func (s *membershipScenario) credentialPageRefuses(ctx context.Context) error {
	scope, err := s.scopeIDs()
	if err != nil {
		return err
	}
	connection, err := s.createdConnection()
	if err != nil {
		return err
	}
	secret := []byte("pm-acceptance-refused-" + randomHex(16))
	defer clear(secret)
	status, answer, err := s.sendCredential(ctx, scope, connection, secret)
	if err != nil {
		return err
	}
	if answerHoldsSecret(answer, secret) {
		return fmt.Errorf("the %s answer with status %d holds the submitted value", operationCredentialPage, status)
	}
	code := problemCode(answer)
	if !(status == http.StatusForbidden && code == "grant_unavailable") && !(status == http.StatusNotFound && code == "resource_not_found") {
		return fmt.Errorf("the %s answered status %d, want 403 with grant_unavailable or 404 with resource_not_found: %s",
			operationCredentialPage, status, describeProblem(answer))
	}
	stored, err := s.readCredentials(ctx, scope, connection, nil)
	if err != nil {
		return err
	}
	if stored.rowCount() != 0 {
		return fmt.Errorf("the observer sees a stored credential for the connection: %d ciphertext, %d lifecycle and %d receipt rows, want none",
			len(stored.secrets), len(stored.lifecycles), len(stored.receipts))
	}
	godog.Logf(ctx, "the credential page refused the user with status %d and problem code %s, and the observer sees no stored credential for the connection", status, code)
	return nil
}

// currentMembershipRevision reads the control revision of the user's own
// membership with pm access memberships list: the item whose membership_ref
// is the bootstrap membership and whose principal_ref is the bootstrap admin.
func (s *membershipScenario) currentMembershipRevision(ctx context.Context, scope *membershipScope) (uint64, error) {
	code, stdout, stderr, err := s.runAsUser(ctx, []string{"access", "memberships", "list", "--workspace", scope.workspaceID, "--output", "json"})
	if err != nil {
		return 0, err
	}
	if code != 0 {
		return 0, errors.New(describeCommand("pm access memberships list", code, stdout, stderr))
	}
	var page struct {
		Items []struct {
			MembershipRef   string `json:"membership_ref"`
			PrincipalRef    string `json:"principal_ref"`
			ControlRevision string `json:"control_revision"`
		} `json:"items"`
	}
	if err := readJSONOutput("pm access memberships list", stdout, &page); err != nil {
		return 0, err
	}
	var found []string
	for _, item := range page.Items {
		if item.MembershipRef == scope.membershipID {
			if item.PrincipalRef != scope.identityID {
				return 0, fmt.Errorf("pm access memberships list names the membership %s with principal %s, want the bootstrap admin %s",
					scope.membershipID, item.PrincipalRef, scope.identityID)
			}
			found = append(found, item.ControlRevision)
		}
	}
	if len(found) != 1 || !controlRevision.MatchString(found[0]) {
		return 0, fmt.Errorf("pm access memberships list names the user's membership %s %d times with a valid control revision, want once (stdout: %d bytes, not shown)",
			scope.membershipID, len(found), len(stdout))
	}
	revision, err := strconv.ParseUint(found[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("read the control revision of the user's membership: %w", err)
	}
	return revision, nil
}

// update runs pm access memberships update on the user's own membership, to
// add the connection, with the revision when it is not empty. With lane true,
// the command is the lane command: the step writes the shared result state,
// with its stderr by form, and keeps the whole stderr for the stderr steps.
func (s *membershipScenario) update(ctx context.Context, scope *membershipScope, connection *userConnection, revision string, lane bool) (int, string, string, error) {
	args := []string{
		"access", "memberships", "update", scope.membershipID,
		"--workspace", scope.workspaceID, "--output", "json",
		"--add-connections", connection.connectionID,
	}
	if revision != "" {
		args = append(args, "--revision", revision)
	}
	code, stdout, stderr, err := s.runAsUser(ctx, args)
	if err != nil {
		return 0, "", "", err
	}
	if lane {
		result := authenticatedResult(code, stdout, stderr, s.lane.deployment.deploymentID)
		result.stderr = describeMembershipLines("stderr", stderr)
		s.shared.result = result
		s.last = &membershipCommand{result: result, stderr: stderr}
	}
	return code, stdout, stderr, nil
}

// runUpdate is the When step of M1 and M3. With the current membership
// revision, the user reads the revision first, the observer then reads the
// membership state once as the baseline of M1's Then steps, and the update
// runs. With no revision, only the update runs, with no --revision.
func (s *membershipScenario) runUpdate(ctx context.Context, revision string) error {
	scope, err := s.scopeIDs()
	if err != nil {
		return err
	}
	connection, err := s.createdConnection()
	if err != nil {
		return err
	}
	switch revision {
	case "no revision":
		_, _, _, err = s.update(ctx, scope, connection, "", true)
		return err
	case "the current membership revision":
		current, err := s.currentMembershipRevision(ctx, scope)
		if err != nil {
			return err
		}
		if s.baseline, err = s.readState(ctx, scope); err != nil {
			return err
		}
		_, _, _, err = s.update(ctx, scope, connection, strconv.FormatUint(current, 10), true)
		return err
	}
	return fmt.Errorf("unknown membership revision %q", revision)
}

// runUpdateWithStaleRevision is M2's When step. The user reads the current
// revision and runs the update with the revision before it, which must be a
// positive revision (criteria-worker v4, line 315).
func (s *membershipScenario) runUpdateWithStaleRevision(ctx context.Context) error {
	scope, err := s.scopeIDs()
	if err != nil {
		return err
	}
	connection, err := s.createdConnection()
	if err != nil {
		return err
	}
	current, err := s.currentMembershipRevision(ctx, scope)
	if err != nil {
		return err
	}
	if current < 2 {
		return fmt.Errorf("the current revision of the user's membership is %d, so no positive revision comes before it", current)
	}
	_, _, _, err = s.update(ctx, scope, connection, strconv.FormatUint(current-1, 10), true)
	return err
}

// addedConnection is M2's Given. The user reads the current revision and
// adds the connection to its own membership. It needs exit code 0. It is not
// the lane command.
func (s *membershipScenario) addedConnection(ctx context.Context) error {
	scope, err := s.scopeIDs()
	if err != nil {
		return err
	}
	connection, err := s.createdConnection()
	if err != nil {
		return err
	}
	current, err := s.currentMembershipRevision(ctx, scope)
	if err != nil {
		return err
	}
	code, stdout, stderr, err := s.update(ctx, scope, connection, strconv.FormatUint(current, 10), false)
	if err != nil {
		return err
	}
	if code != 0 {
		return errors.New(describeCommand("pm access memberships update", code, stdout, stderr))
	}
	return nil
}

// records reads the membership state of the workspace once.
func (s *membershipScenario) records(ctx context.Context) error {
	if s.recorded != nil {
		return errors.New("the observer already recorded the membership state in this scenario")
	}
	scope, err := s.scopeIDs()
	if err != nil {
		return err
	}
	state, err := s.readState(ctx, scope)
	if err != nil {
		return err
	}
	s.recorded = state
	return nil
}

// unchanged reads the membership state once more and compares the two
// readings. Each row added, changed or removed fails the step.
func (s *membershipScenario) unchanged(ctx context.Context) error {
	if s.recorded == nil {
		return errors.New("the observer did not record the membership state in this scenario")
	}
	scope, err := s.scopeIDs()
	if err != nil {
		return err
	}
	after, err := s.readState(ctx, scope)
	if err != nil {
		return err
	}
	var changes []string
	for _, table := range membershipStateTables {
		added, removed := rowDifference(s.recorded[table], after[table])
		if len(added) == 0 && len(removed) == 0 {
			continue
		}
		kind := "rows"
		if slices.Contains(membershipAuditTables, table) {
			kind = "audit records"
		}
		changes = append(changes, fmt.Sprintf("pm.%s: %d new %s %s, %d removed %s %s",
			table, len(added), kind, added, len(removed), kind, removed))
	}
	if len(changes) > 0 {
		return fmt.Errorf("the observer sees the membership state of the workspace changed since it recorded it:\n%s", strings.Join(changes, "\n"))
	}
	return nil
}

// membershipRow is one row of pm.memberships, decoded from the JSON text of
// an observer reading.
type membershipRow struct {
	MembershipID    string   `json:"membership_id"`
	IdentityID      string   `json:"identity_id"`
	Role            string   `json:"role"`
	ResourceGrants  []string `json:"resource_grants"`
	Permissions     []string `json:"permissions"`
	ConnectionIDs   []string `json:"connection_ids"`
	OperationIDs    []string `json:"operation_ids"`
	ControlRevision int64    `json:"control_revision"`
	ExpiresAt       *string  `json:"expires_at"`
	RevokedAt       *string  `json:"revoked_at"`
}

// membershipRows decodes the membership rows of a reading by membership ID,
// with the JSON text of each row.
func membershipRows(state membershipState) (map[string]membershipRow, map[string]string, error) {
	rows := map[string]membershipRow{}
	texts := map[string]string{}
	for _, text := range state["memberships"] {
		var row membershipRow
		if err := json.Unmarshal([]byte(text), &row); err != nil || row.MembershipID == "" {
			return nil, nil, fmt.Errorf("decode the pm.memberships row %s", text)
		}
		if _, duplicate := rows[row.MembershipID]; duplicate {
			return nil, nil, fmt.Errorf("the observer reads the membership %s twice", row.MembershipID)
		}
		rows[row.MembershipID], texts[row.MembershipID] = row, text
	}
	return rows, texts, nil
}

// sortedWith returns the sorted set of the values and the value.
func sortedWith(values []string, value string) []string {
	result := slices.Clone(values)
	if !slices.Contains(result, value) {
		result = append(result, value)
	}
	slices.Sort(result)
	return result
}

// sameSet reports whether the two lists hold the same values.
func sameSet(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// sameTime reports whether two optional timestamps are equal.
func sameTime(a, b *string) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// grantsListConnection reads the membership state once after the update and
// compares it with the baseline of the When step. The user's membership must
// list the connection, in connection_ids and as the resource grant
// "connection:<id>", and gain nothing else. Its role, identity, permissions,
// operations, expiry and revocation must not change, and its control
// revision must be one higher. Every other membership of the workspace must
// not change.
func (s *membershipScenario) grantsListConnection(ctx context.Context) error {
	if s.baseline == nil {
		return errors.New("the observer has no membership state from before the update")
	}
	scope, err := s.scopeIDs()
	if err != nil {
		return err
	}
	connection, err := s.createdConnection()
	if err != nil {
		return err
	}
	after, err := s.readState(ctx, scope)
	if err != nil {
		return err
	}
	beforeRows, beforeTexts, err := membershipRows(s.baseline)
	if err != nil {
		return err
	}
	afterRows, afterTexts, err := membershipRows(after)
	if err != nil {
		return err
	}
	before, found := beforeRows[scope.membershipID]
	if !found {
		return fmt.Errorf("the observer did not read the user's membership %s before the update", scope.membershipID)
	}
	current, found := afterRows[scope.membershipID]
	if !found {
		return fmt.Errorf("the observer does not read the user's membership %s after the update", scope.membershipID)
	}
	var problems []string
	check := func(ok bool, format string, args ...any) {
		if !ok {
			problems = append(problems, fmt.Sprintf(format, args...))
		}
	}
	grant := "connection:" + connection.connectionID
	check(!slices.Contains(before.ConnectionIDs, connection.connectionID) && !slices.Contains(before.ResourceGrants, grant),
		"the membership listed the connection before the update: %s", beforeTexts[scope.membershipID])
	check(sameSet(current.ConnectionIDs, sortedWith(before.ConnectionIDs, connection.connectionID)),
		"the membership connections are %q, want %q", current.ConnectionIDs, sortedWith(before.ConnectionIDs, connection.connectionID))
	check(sameSet(current.ResourceGrants, sortedWith(before.ResourceGrants, grant)),
		"the membership resource grants are %q, want %q", current.ResourceGrants, sortedWith(before.ResourceGrants, grant))
	check(sameSet(current.Permissions, before.Permissions), "the membership permissions changed from %q to %q", before.Permissions, current.Permissions)
	check(sameSet(current.OperationIDs, before.OperationIDs), "the membership operations changed from %q to %q", before.OperationIDs, current.OperationIDs)
	check(current.Role == before.Role && current.IdentityID == before.IdentityID && current.IdentityID == scope.identityID,
		"the membership role or identity changed, or it is not the membership of the user: before %s, after %s", beforeTexts[scope.membershipID], afterTexts[scope.membershipID])
	check(sameTime(current.ExpiresAt, before.ExpiresAt) && sameTime(current.RevokedAt, before.RevokedAt),
		"the membership expiry or revocation changed: before %s, after %s", beforeTexts[scope.membershipID], afterTexts[scope.membershipID])
	check(current.ControlRevision == before.ControlRevision+1,
		"the membership control revision is %d, want %d (one higher than %d)", current.ControlRevision, before.ControlRevision+1, before.ControlRevision)
	for id, text := range beforeTexts {
		if id != scope.membershipID && afterTexts[id] != text {
			problems = append(problems, fmt.Sprintf("another membership changed: before %s, after %q", text, afterTexts[id]))
		}
	}
	for id, text := range afterTexts {
		if _, existed := beforeTexts[id]; !existed {
			problems = append(problems, fmt.Sprintf("a new membership appeared: %s", text))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

// workspaceAuditRecord is the part of one audit record that the step checks.
type workspaceAuditRecord struct {
	Action             string `json:"action"`
	TargetRef          string `json:"target_ref"`
	RequestedActorID   string `json:"requested_actor_id"`
	RequestedActorKind string `json:"requested_actor_kind"`
	EffectiveActorID   string `json:"effective_actor_id"`
	EffectiveActorKind string `json:"effective_actor_kind"`
}

// membershipUpdateAction is the action of the audit record of a membership
// update: the control name of the update (#332).
const membershipUpdateAction = "membership.update"

// oneAuditRecord reads the membership state once after the update and
// compares its audit records with the baseline of the When step. It needs
// exactly one new workspace audit record, no removed record and no new run
// audit record. The new record must be the record of the update of the
// user's membership, with the user as its requested and effective actor.
func (s *membershipScenario) oneAuditRecord(ctx context.Context) error {
	if s.baseline == nil {
		return errors.New("the observer has no membership state from before the update")
	}
	scope, err := s.scopeIDs()
	if err != nil {
		return err
	}
	after, err := s.readState(ctx, scope)
	if err != nil {
		return err
	}
	added, removed := rowDifference(s.baseline["workspace_audit_entries"], after["workspace_audit_entries"])
	runAdded, runRemoved := rowDifference(s.baseline["audit_entries"], after["audit_entries"])
	if len(added) != 1 || len(removed) != 0 || len(runAdded) != 0 || len(runRemoved) != 0 {
		return fmt.Errorf("the observer sees %d new and %d removed workspace audit records %s %s, and %d new and %d removed run audit records %s %s, want exactly one new workspace audit record",
			len(added), len(removed), added, removed, len(runAdded), len(runRemoved), runAdded, runRemoved)
	}
	var record workspaceAuditRecord
	if err := json.Unmarshal([]byte(added[0]), &record); err != nil {
		return fmt.Errorf("decode the new pm.workspace_audit_entries row %s", added[0])
	}
	if record.Action != membershipUpdateAction || record.TargetRef != scope.membershipID ||
		record.RequestedActorKind != "user" || record.RequestedActorID != scope.identityID ||
		record.EffectiveActorKind != "user" || record.EffectiveActorID != scope.identityID {
		return fmt.Errorf("the new audit record is %s, want the action %q for the membership %s with the user %s as its requested and effective actor",
			added[0], membershipUpdateAction, scope.membershipID, scope.identityID)
	}
	return nil
}

// storesPassword is M1's last Then step. The user's browser session sends the
// credential page request for the connection with a new random password. M1
// has no customer database, so the password is a value that the step makes.
// The step needs status 200 with an active credential of the connection's
// credential reference, and an answer that does not hold the password. Then
// the observer reads the stored credential of the connection once: one active
// lifecycle with the version and revision of the answer, one ciphertext of
// that version that does not hold the password, and one receipt with the user
// as its principal.
func (s *membershipScenario) storesPassword(ctx context.Context) error {
	scope, err := s.scopeIDs()
	if err != nil {
		return err
	}
	connection, err := s.createdConnection()
	if err != nil {
		return err
	}
	password := []byte("pm-acceptance-customer-role-" + randomHex(16))
	defer clear(password)
	status, answer, err := s.sendCredential(ctx, scope, connection, password)
	if err != nil {
		return err
	}
	if answerHoldsSecret(answer, password) {
		return fmt.Errorf("the %s answer with status %d holds the password", operationCredentialPage, status)
	}
	if status != http.StatusOK {
		return fmt.Errorf("the %s answered status %d, want 200: %s", operationCredentialPage, status, describeProblem(answer))
	}
	var stored struct {
		CredentialRef     string `json:"credential_ref"`
		CiphertextVersion int64  `json:"ciphertext_version"`
		Status            string `json:"status"`
		LifecycleRevision int64  `json:"lifecycle_revision"`
	}
	if err := json.Unmarshal(answer, &stored); err != nil || stored.Status != "active" || stored.CredentialRef != connection.credentialRef ||
		stored.CiphertextVersion < 1 || stored.LifecycleRevision < 1 {
		return fmt.Errorf("the %s answer (%d bytes, not shown) is not an active credential of the connection's credential reference", operationCredentialPage, len(answer))
	}
	observed, err := s.readCredentials(ctx, scope, connection, password)
	if err != nil {
		return err
	}
	var problems []string
	if len(observed.lifecycles) != 1 || observed.lifecycles[0] != (storedLifecycle{
		credentialRef: stored.CredentialRef, ciphertextVersion: stored.CiphertextVersion, status: "active", revision: stored.LifecycleRevision,
	}) {
		problems = append(problems, fmt.Sprintf("the observer sees the credential lifecycles %+v, want one active lifecycle %s with version %d and revision %d",
			observed.lifecycles, stored.CredentialRef, stored.CiphertextVersion, stored.LifecycleRevision))
	}
	if len(observed.secrets) != 1 || observed.secrets[0].credentialRef != stored.CredentialRef || observed.secrets[0].ciphertextVersion != stored.CiphertextVersion {
		problems = append(problems, fmt.Sprintf("the observer sees %d ciphertext rows, want one of %s with version %d", len(observed.secrets), stored.CredentialRef, stored.CiphertextVersion))
	}
	for _, secret := range observed.secrets {
		if secret.holdsSecret {
			problems = append(problems, "a stored ciphertext of the connection holds the password")
		}
	}
	if len(observed.receipts) != 1 || observed.receipts[0] != (storedReceipt{
		principalID: scope.identityID, principalKind: "user", credentialRef: stored.CredentialRef, ciphertextVersion: stored.CiphertextVersion,
	}) {
		problems = append(problems, fmt.Sprintf("the observer sees the credential receipts %+v, want one receipt of the user %s for %s with version %d",
			observed.receipts, scope.identityID, stored.CredentialRef, stored.CiphertextVersion))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

// lastCommand returns the last update command of these steps when it is the
// last lane command of the scenario.
func (s *membershipScenario) lastCommand() (*membershipCommand, error) {
	if s.last == nil || !s.shared.result.ran || s.shared.result != s.last.result {
		return nil, errors.New("the last lane command is not a pm access memberships update command of these steps")
	}
	return s.last, nil
}

// stderrStartsWith reads the whole stderr of the last update command.
func (s *membershipScenario) stderrStartsWith(prefix string) error {
	last, err := s.lastCommand()
	if err != nil {
		return err
	}
	if !strings.HasPrefix(last.stderr, prefix) {
		return fmt.Errorf("the lane command stderr does not start with %q\n%s", prefix, describeMembershipLines("stderr", last.stderr))
	}
	return nil
}

// stderrIsExactlyLine compares the whole stderr of the last update command
// with the line and one line feed, with no other byte.
func (s *membershipScenario) stderrIsExactlyLine(line string) error {
	last, err := s.lastCommand()
	if err != nil {
		return err
	}
	if last.stderr != line+"\n" {
		return fmt.Errorf("the lane command stderr is not exactly the line %q\n%s", line+"\n", describeMembershipLines("stderr", last.stderr))
	}
	return nil
}

// close closes the observer connection and drops the observer role.
func (s *membershipScenario) close() error {
	err := s.observer.close()
	s.observer, s.scope, s.connection, s.baseline, s.recorded, s.last = nil, nil, nil, nil, nil, nil
	return err
}
