package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/credentials"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// BootstrapEndpointMember is the member of the PostgreSQL secret bundle that
// holds the bootstrap credential: a connection string for pm_app_bootstrap.
// Only the operator bootstrap reads it. The running server never reads it.
const BootstrapEndpointMember = "pm-app-bootstrap-endpoint"

// The refusals of the operator bootstrap (ADR 0006). Each text is the exact
// stderr line after the "pm-server: " prefix.
var (
	ErrBootstrapScopeExists         = store.ErrBootstrapScopeExists
	ErrBootstrapLoginBound          = store.ErrBootstrapLoginBound
	ErrBootstrapWorkspaceName       = errors.New("bootstrap workspace name is required")
	ErrBootstrapAdminSubject        = errors.New("bootstrap admin subject is required")
	ErrBootstrapUndeclaredConnector = errors.New("bootstrap provider account names an undeclared connector")
	ErrBootstrapAuthority           = store.ErrBootstrapAuthority
)

// The other failures of the operator bootstrap. ADR 0006 does not name them.
var (
	ErrBootstrapInput          = errors.New("bootstrap input is invalid")
	ErrBootstrapFailed         = errors.New("bootstrap failed and wrote nothing")
	ErrBootstrapOutcomeUnknown = errors.New("bootstrap outcome is unknown; run the same bootstrap again")
)

const (
	bootstrapInputLimit       = 64 << 10
	bootstrapAccountLimit     = 64
	bootstrapAdminRole        = "admin"
	bootstrapAuditAction      = "workspace.bootstrap"
	bootstrapAccountCapacity  = int64(1)
	bootstrapIdentifierLength = 18
)

// bootstrapAdminPermissions are all the permissions that the product checks.
// The first admin can grant only what it holds, so it holds each of them. Its
// grants name no connection and no operation (ADR 0006).
var bootstrapAdminPermissions = []string{
	"artifact.read",
	"artifact.write",
	"catalog.read",
	"connection.credentials.manage",
	"connection.manage",
	"connection.read",
	"event.read",
	"operation.invoke",
	"run.control",
	"run.read",
	"workspace.access.manage",
	"workspace.access.read",
	"workspace.read",
}

// BootstrapInput is the operator's input file. It holds no secret.
type BootstrapInput struct {
	OrgID            string
	WorkspaceID      string
	WorkspaceName    string
	AdminIssuer      string
	AdminSubject     string
	ProviderAccounts []BootstrapAccountInput
}

type BootstrapAccountInput struct {
	ConnectorID string
	DisplayName string
}

type bootstrapInputWire struct {
	OrgID            *string                      `json:"org_id"`
	WorkspaceID      *string                      `json:"workspace_id"`
	WorkspaceName    *string                      `json:"workspace_name"`
	AdminIssuer      *string                      `json:"admin_issuer"`
	AdminSubject     *string                      `json:"admin_subject"`
	ProviderAccounts *[]bootstrapAccountInputWire `json:"provider_accounts"`
}

type bootstrapAccountInputWire struct {
	ConnectorID *string `json:"connector_id"`
	DisplayName *string `json:"display_name"`
}

// DecodeBootstrapInput reads one strict JSON input document. Every field must
// be present, and no other field may be present. It checks the shape only;
// RunBootstrap checks the values.
func DecodeBootstrapInput(reader io.Reader) (BootstrapInput, error) {
	if reader == nil {
		return BootstrapInput{}, ErrBootstrapInput
	}
	contents, err := io.ReadAll(io.LimitReader(reader, bootstrapInputLimit+1))
	if err != nil || len(contents) > bootstrapInputLimit {
		return BootstrapInput{}, ErrBootstrapInput
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var wire bootstrapInputWire
	if err := decoder.Decode(&wire); err != nil {
		return BootstrapInput{}, ErrBootstrapInput
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return BootstrapInput{}, ErrBootstrapInput
	}
	if wire.OrgID == nil || wire.WorkspaceID == nil || wire.WorkspaceName == nil ||
		wire.AdminIssuer == nil || wire.AdminSubject == nil || wire.ProviderAccounts == nil ||
		len(*wire.ProviderAccounts) > bootstrapAccountLimit {
		return BootstrapInput{}, ErrBootstrapInput
	}
	input := BootstrapInput{
		OrgID:            *wire.OrgID,
		WorkspaceID:      *wire.WorkspaceID,
		WorkspaceName:    *wire.WorkspaceName,
		AdminIssuer:      *wire.AdminIssuer,
		AdminSubject:     *wire.AdminSubject,
		ProviderAccounts: make([]BootstrapAccountInput, 0, len(*wire.ProviderAccounts)),
	}
	for _, account := range *wire.ProviderAccounts {
		if account.ConnectorID == nil || account.DisplayName == nil {
			return BootstrapInput{}, ErrBootstrapInput
		}
		input.ProviderAccounts = append(input.ProviderAccounts, BootstrapAccountInput{
			ConnectorID: *account.ConnectorID,
			DisplayName: *account.DisplayName,
		})
	}
	return input, nil
}

// BootstrapConfig composes one operator bootstrap.
type BootstrapConfig struct {
	DeploymentID     string
	ConnectorBundles []string
	// Write runs the store transaction. It reads the bootstrap credential
	// only when it runs, after every input check passed.
	Write  func(context.Context, store.BootstrapRequest) error
	Random io.Reader
}

// BootstrapResult is the one JSON document on stdout. It holds no secret.
type BootstrapResult struct {
	DeploymentID     string                   `json:"deployment_id"`
	OrgID            string                   `json:"org_id"`
	Workspace        BootstrapWorkspace       `json:"workspace"`
	Admin            BootstrapAdmin           `json:"admin"`
	Membership       BootstrapMembership      `json:"membership"`
	ProviderAccounts []BootstrapAccountResult `json:"provider_accounts"`
}

type BootstrapWorkspace struct {
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
}

type BootstrapAdmin struct {
	IdentityID string `json:"identity_id"`
	Issuer     string `json:"issuer"`
	Subject    string `json:"subject"`
}

type BootstrapMembership struct {
	MembershipID string   `json:"membership_id"`
	Role         string   `json:"role"`
	Permissions  []string `json:"permissions"`
}

type BootstrapAccountResult struct {
	ProviderAccountID string `json:"provider_account_id"`
	ConnectorID       string `json:"connector_id"`
	DisplayName       string `json:"display_name"`
}

// RunBootstrap checks the input against the deployment, then writes the
// bootstrap in one store transaction. Each refusal writes nothing.
func RunBootstrap(ctx context.Context, configuration BootstrapConfig, input BootstrapInput) (BootstrapResult, error) {
	if ctx == nil || configuration.Write == nil || !validApplicationID(configuration.DeploymentID) {
		return BootstrapResult{}, ErrBootstrapFailed
	}
	if err := validateBootstrapInput(input); err != nil {
		return BootstrapResult{}, err
	}
	if len(input.ProviderAccounts) > 0 {
		declared, err := declaredConnectorIDs(configuration.ConnectorBundles)
		if err != nil {
			return BootstrapResult{}, ErrBootstrapFailed
		}
		for _, account := range input.ProviderAccounts {
			if _, found := declared[account.ConnectorID]; !found {
				return BootstrapResult{}, ErrBootstrapUndeclaredConnector
			}
		}
	}
	random := configuration.Random
	if random == nil {
		random = rand.Reader
	}
	identifier := func(prefix string) (string, error) {
		return randomApplicationID(random, prefix, bootstrapIdentifierLength)
	}
	result := BootstrapResult{
		DeploymentID: configuration.DeploymentID,
		OrgID:        input.OrgID,
		Workspace:    BootstrapWorkspace{WorkspaceID: input.WorkspaceID, Name: input.WorkspaceName},
		Admin:        BootstrapAdmin{Issuer: input.AdminIssuer, Subject: input.AdminSubject},
		Membership: BootstrapMembership{
			Role:        bootstrapAdminRole,
			Permissions: append([]string(nil), bootstrapAdminPermissions...),
		},
		ProviderAccounts: make([]BootstrapAccountResult, 0, len(input.ProviderAccounts)),
	}
	var err error
	if result.Admin.IdentityID, err = identifier("identity-"); err != nil {
		return BootstrapResult{}, ErrBootstrapFailed
	}
	if result.Membership.MembershipID, err = identifier("membership-"); err != nil {
		return BootstrapResult{}, ErrBootstrapFailed
	}
	auditID, err := identifier("audit-")
	if err != nil {
		return BootstrapResult{}, ErrBootstrapFailed
	}
	request := store.BootstrapRequest{
		DeploymentID:     configuration.DeploymentID,
		Scope:            contract.Scope{OrgID: input.OrgID, WorkspaceID: input.WorkspaceID},
		WorkspaceName:    input.WorkspaceName,
		AdminIdentityID:  result.Admin.IdentityID,
		AdminIssuer:      input.AdminIssuer,
		AdminSubject:     input.AdminSubject,
		MembershipID:     result.Membership.MembershipID,
		Role:             bootstrapAdminRole,
		Permissions:      append([]string(nil), bootstrapAdminPermissions...),
		ProviderAccounts: make([]store.BootstrapProviderAccount, 0, len(input.ProviderAccounts)),
		AuditID:          auditID,
		Action:           bootstrapAuditAction,
	}
	for _, account := range input.ProviderAccounts {
		accountID, err := identifier("provider-account-")
		if err != nil {
			return BootstrapResult{}, ErrBootstrapFailed
		}
		request.ProviderAccounts = append(request.ProviderAccounts, store.BootstrapProviderAccount{
			AccountID:   accountID,
			ConnectorID: account.ConnectorID,
			DisplayName: account.DisplayName,
			Capacity:    bootstrapAccountCapacity,
		})
		result.ProviderAccounts = append(result.ProviderAccounts, BootstrapAccountResult{
			ProviderAccountID: accountID,
			ConnectorID:       account.ConnectorID,
			DisplayName:       account.DisplayName,
		})
	}
	if err := configuration.Write(ctx, request); err != nil {
		return BootstrapResult{}, bootstrapFailure(err)
	}
	return result, nil
}

// validateBootstrapInput keeps the order of the checks fixed, so that one
// input always gives the same refusal.
func validateBootstrapInput(input BootstrapInput) error {
	if !validApplicationID(input.OrgID) || !validApplicationID(input.WorkspaceID) {
		return ErrBootstrapInput
	}
	if input.WorkspaceName == "" {
		return ErrBootstrapWorkspaceName
	}
	if !validDisplayName(input.WorkspaceName) || !validApplicationID(input.AdminIssuer) {
		return ErrBootstrapInput
	}
	if input.AdminSubject == "" {
		return ErrBootstrapAdminSubject
	}
	if !validApplicationID(input.AdminSubject) {
		return ErrBootstrapInput
	}
	for _, account := range input.ProviderAccounts {
		if !validApplicationName(account.ConnectorID) || !validDisplayName(account.DisplayName) {
			return ErrBootstrapInput
		}
	}
	return nil
}

func declaredConnectorIDs(paths []string) (map[string]struct{}, error) {
	bundles, err := loadConnectorBundles(paths)
	if err != nil {
		return nil, err
	}
	declared := make(map[string]struct{}, len(bundles))
	for _, bundle := range bundles {
		declared[bundle.ConnectorID] = struct{}{}
	}
	return declared, nil
}

func bootstrapFailure(err error) error {
	switch {
	case errors.Is(err, store.ErrCommitOutcomeUnknown):
		return ErrBootstrapOutcomeUnknown
	case errors.Is(err, store.ErrBootstrapScopeExists):
		return ErrBootstrapScopeExists
	case errors.Is(err, store.ErrBootstrapLoginBound):
		return ErrBootstrapLoginBound
	case errors.Is(err, store.ErrBootstrapAuthority):
		return ErrBootstrapAuthority
	default:
		return ErrBootstrapFailed
	}
}

// BootstrapStoreWriter returns the write step of a bootstrap. It opens the
// operator secret directory, reads only the bootstrap credential, opens the
// bootstrap pool and writes the request. A missing or refused credential is
// ErrBootstrapAuthority.
func BootstrapStoreWriter(secretsDirectory, endpointRef string) func(context.Context, store.BootstrapRequest) error {
	return func(ctx context.Context, request store.BootstrapRequest) error {
		secrets, err := credentials.OpenOperatorSecrets(secretsDirectory)
		if err != nil {
			return store.ErrBootstrapAuthority
		}
		endpoint, err := secrets.Text(endpointRef, BootstrapEndpointMember)
		closeErr := secrets.Close()
		if err != nil || closeErr != nil {
			return store.ErrBootstrapAuthority
		}
		pool, err := store.OpenBootstrapPool(ctx, endpoint)
		if err != nil {
			return err
		}
		defer pool.Close()
		return store.Bootstrap(ctx, pool, request)
	}
}
