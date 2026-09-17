package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

var (
	ErrMutationReceiptExpired = errors.New("identity mutation retry receipt expired")
	ErrMutationUnresolved     = errors.New("identity mutation outcome is unresolved")
)

type IdentityCollection string

const (
	IdentityCollectionMemberships     IdentityCollection = "memberships"
	IdentityCollectionServiceAccounts IdentityCollection = "service_accounts"
	IdentityCollectionDelegations     IdentityCollection = "delegations"
	IdentityCollectionConnections     IdentityCollection = "connections"
)

type IdentityCollectionRevisions struct {
	Memberships     uint64
	ServiceAccounts uint64
	Delegations     uint64
	Connections     uint64
}

func LoadIdentityCollectionRevisions(ctx context.Context, tx *Transaction) (IdentityCollectionRevisions, error) {
	if _, err := tx.requireScope(); err != nil {
		return IdentityCollectionRevisions{}, err
	}
	revisions := IdentityCollectionRevisions{
		Memberships: 1, ServiceAccounts: 1, Delegations: 1, Connections: 1,
	}
	if err := tx.queryRow(ctx, `
		SELECT membership_revision, service_account_revision, delegation_revision, connection_revision
		FROM pm.identity_collection_revisions
	`).Scan(
		&revisions.Memberships,
		&revisions.ServiceAccounts,
		&revisions.Delegations,
		&revisions.Connections,
	); errors.Is(err, pgx.ErrNoRows) {
		return revisions, nil
	} else if err != nil {
		return IdentityCollectionRevisions{}, fmt.Errorf("load identity collection revisions: %w", err)
	}
	return revisions, nil
}

type MutationReceiptKey struct {
	Scope          contract.Scope
	PrincipalID    string
	OperationID    string
	TargetID       string
	IdempotencyKey string
}

func (key MutationReceiptKey) validate() error {
	if err := key.Scope.Validate(); err != nil {
		return err
	}
	if !validStoreID(key.PrincipalID) || !validStoreName(key.OperationID) ||
		!validStoreID(key.TargetID) || !validStoreID(key.IdempotencyKey) {
		return errors.New("identity mutation receipt key is invalid")
	}
	return nil
}

type MutationReceiptState string

const (
	MutationReceiptPending   MutationReceiptState = "pending"
	MutationReceiptResolved  MutationReceiptState = "resolved"
	MutationReceiptCompacted MutationReceiptState = "compacted"
)

type MutationReceipt struct {
	Key               MutationReceiptKey
	IntentDigest      contract.SemanticDigest
	IntentRevision    uint64
	State             MutationReceiptState
	ResultRef         string
	ResultRevision    uint64
	ResponseStatus    int
	SafeResult        json.RawMessage
	ResolvedAt        *time.Time
	ResponseExpiresAt *time.Time
}

type MutationBegin struct {
	Receipt MutationReceipt
	Fresh   bool
}

type MutationReceiptExpiredError struct {
	ResultRef      string
	ResultRevision uint64
}

func (err *MutationReceiptExpiredError) Error() string {
	return ErrMutationReceiptExpired.Error()
}

func (err *MutationReceiptExpiredError) Unwrap() error {
	return ErrMutationReceiptExpired
}

func BeginIdentityMutation(
	ctx context.Context,
	tx *Transaction,
	key MutationReceiptKey,
	intentDigest contract.SemanticDigest,
	intentRevision uint64,
) (MutationBegin, error) {
	if err := tx.requireMatchingScope(key.Scope); err != nil {
		return MutationBegin{}, err
	}
	if err := key.validate(); err != nil {
		return MutationBegin{}, err
	}
	if err := intentDigest.Validate(); err != nil {
		return MutationBegin{}, err
	}
	if intentRevision == 0 {
		return MutationBegin{}, errors.New("identity mutation intent revision must be positive")
	}
	if err := requireActiveWorkspace(ctx, tx); err != nil {
		return MutationBegin{}, err
	}
	tag, err := tx.exec(ctx, `
		INSERT INTO pm.identity_mutation_keys (
			org_id, workspace_id, principal_id, operation_id, target_id,
			idempotency_key, intent_digest, expected_revision, receipt_state, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending', clock_timestamp())
		ON CONFLICT DO NOTHING
	`,
		key.Scope.OrgID,
		key.Scope.WorkspaceID,
		key.PrincipalID,
		key.OperationID,
		key.TargetID,
		key.IdempotencyKey,
		string(intentDigest),
		intentRevision,
	)
	if err != nil {
		return MutationBegin{}, safeMetadataWriteError("begin identity mutation", err)
	}
	if tag.RowsAffected() == 1 {
		return MutationBegin{
			Fresh: true,
			Receipt: MutationReceipt{
				Key: key, IntentDigest: intentDigest, IntentRevision: intentRevision,
				State: MutationReceiptPending,
			},
		}, nil
	}
	receipt, responseExpired, err := loadMutationReceipt(ctx, tx, key, true)
	if err != nil {
		return MutationBegin{}, err
	}
	if receipt.IntentDigest != intentDigest || receipt.IntentRevision != intentRevision {
		return MutationBegin{}, ErrIdempotencyConflict
	}
	switch receipt.State {
	case MutationReceiptResolved:
		if responseExpired {
			return MutationBegin{}, &MutationReceiptExpiredError{
				ResultRef: receipt.ResultRef, ResultRevision: receipt.ResultRevision,
			}
		}
		return MutationBegin{Receipt: receipt}, nil
	case MutationReceiptCompacted:
		return MutationBegin{}, &MutationReceiptExpiredError{
			ResultRef: receipt.ResultRef, ResultRevision: receipt.ResultRevision,
		}
	case MutationReceiptPending:
		return MutationBegin{}, ErrMutationUnresolved
	default:
		return MutationBegin{}, errors.New("stored identity mutation receipt state is invalid")
	}
}

func FinishIdentityMutation(
	ctx context.Context,
	tx *Transaction,
	key MutationReceiptKey,
	resultRef string,
	resultRevision uint64,
	responseStatus int,
	safeResult json.RawMessage,
	responseRetention time.Duration,
) (MutationReceipt, error) {
	if err := tx.requireMatchingScope(key.Scope); err != nil {
		return MutationReceipt{}, err
	}
	if err := key.validate(); err != nil {
		return MutationReceipt{}, err
	}
	if !validStoreID(resultRef) || resultRevision == 0 ||
		(responseStatus != 200 && responseStatus != 201 && responseStatus != 202) ||
		len(safeResult) == 0 || len(safeResult) > 64*1024 || !json.Valid(safeResult) ||
		responseRetention < 7*24*time.Hour {
		return MutationReceipt{}, errors.New("identity mutation safe result or retention is invalid")
	}
	var receipt MutationReceipt
	var encoded []byte
	var digest string
	if err := tx.queryRow(ctx, `
		UPDATE pm.identity_mutation_keys
		SET receipt_state = 'resolved', result_ref = $5, result_revision = $6,
		    response_status = $7, safe_result = $8,
		    resolved_at = clock_timestamp(),
		    response_expires_at = clock_timestamp() + make_interval(secs => $9),
		    updated_at = clock_timestamp()
		WHERE principal_id = $1 AND operation_id = $2 AND target_id = $3 AND idempotency_key = $4
		  AND receipt_state = 'pending'
		RETURNING intent_digest, expected_revision, receipt_state, result_ref,
		          result_revision, response_status, safe_result, resolved_at, response_expires_at
	`,
		key.PrincipalID,
		key.OperationID,
		key.TargetID,
		key.IdempotencyKey,
		resultRef,
		resultRevision,
		responseStatus,
		safeResult,
		int64(responseRetention/time.Second),
	).Scan(
		&digest,
		&receipt.IntentRevision,
		&receipt.State,
		&receipt.ResultRef,
		&receipt.ResultRevision,
		&receipt.ResponseStatus,
		&encoded,
		&receipt.ResolvedAt,
		&receipt.ResponseExpiresAt,
	); errors.Is(err, pgx.ErrNoRows) {
		return MutationReceipt{}, ErrMutationUnresolved
	} else if err != nil {
		return MutationReceipt{}, safeMetadataWriteError("finish identity mutation", err)
	}
	receipt.Key = key
	receipt.IntentDigest = contract.SemanticDigest(digest)
	receipt.SafeResult = append(json.RawMessage(nil), encoded...)
	normalizeReceiptTimes(&receipt)
	return receipt, nil
}

func CompactIdentityMutationReceipts(ctx context.Context, tx *Transaction, through time.Time, maximum uint32) (uint32, error) {
	if _, err := tx.requireScope(); err != nil {
		return 0, err
	}
	if through.IsZero() || through.Location() != time.UTC || maximum == 0 || maximum > 1000 {
		return 0, errors.New("identity mutation compaction bound is invalid")
	}
	tag, err := tx.exec(ctx, `
		WITH candidates AS (
			SELECT principal_id, operation_id, target_id, idempotency_key
			FROM pm.identity_mutation_keys
			WHERE receipt_state = 'resolved' AND resolved_at IS NOT NULL
			  AND response_expires_at IS NOT NULL AND response_expires_at <= $1
			ORDER BY response_expires_at, principal_id, operation_id, target_id, idempotency_key
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE pm.identity_mutation_keys AS receipt
		SET receipt_state = 'compacted', safe_result = NULL, response_status = NULL,
		    compacted_at = clock_timestamp(), updated_at = clock_timestamp()
		FROM candidates
		WHERE receipt.principal_id = candidates.principal_id
		  AND receipt.operation_id = candidates.operation_id
		  AND receipt.target_id = candidates.target_id
		  AND receipt.idempotency_key = candidates.idempotency_key
	`, through, int64(maximum))
	if err != nil {
		return 0, safeMetadataWriteError("compact identity mutation receipts", err)
	}
	return uint32(tag.RowsAffected()), nil
}

func loadMutationReceipt(ctx context.Context, tx *Transaction, key MutationReceiptKey, lock bool) (MutationReceipt, bool, error) {
	if err := tx.requireMatchingScope(key.Scope); err != nil {
		return MutationReceipt{}, false, err
	}
	if err := key.validate(); err != nil {
		return MutationReceipt{}, false, err
	}
	query := `
		SELECT intent_digest, expected_revision, receipt_state,
		       COALESCE(result_ref, ''), COALESCE(result_revision, 0),
		       COALESCE(response_status, 0), COALESCE(safe_result, 'null'::jsonb),
		       resolved_at, response_expires_at,
		       COALESCE(response_expires_at <= clock_timestamp(), false)
		FROM pm.identity_mutation_keys
		WHERE principal_id = $1 AND operation_id = $2 AND target_id = $3 AND idempotency_key = $4
	`
	if lock {
		query += ` FOR UPDATE`
	}
	receipt := MutationReceipt{Key: key}
	var digest string
	var encoded []byte
	var responseExpired bool
	if err := tx.queryRow(ctx, query, key.PrincipalID, key.OperationID, key.TargetID, key.IdempotencyKey).Scan(
		&digest,
		&receipt.IntentRevision,
		&receipt.State,
		&receipt.ResultRef,
		&receipt.ResultRevision,
		&receipt.ResponseStatus,
		&encoded,
		&receipt.ResolvedAt,
		&receipt.ResponseExpiresAt,
		&responseExpired,
	); errors.Is(err, pgx.ErrNoRows) {
		return MutationReceipt{}, false, ErrNotFound
	} else if err != nil {
		return MutationReceipt{}, false, fmt.Errorf("load identity mutation receipt: %w", err)
	}
	receipt.IntentDigest = contract.SemanticDigest(digest)
	if receipt.State == MutationReceiptResolved {
		receipt.SafeResult = append(json.RawMessage(nil), encoded...)
	}
	normalizeReceiptTimes(&receipt)
	return receipt, responseExpired, nil
}

func normalizeReceiptTimes(receipt *MutationReceipt) {
	if receipt.ResolvedAt != nil {
		value := receipt.ResolvedAt.UTC()
		receipt.ResolvedAt = &value
	}
	if receipt.ResponseExpiresAt != nil {
		value := receipt.ResponseExpiresAt.UTC()
		receipt.ResponseExpiresAt = &value
	}
}

type GrantMembershipRequest struct {
	MembershipID               string
	Scope                      contract.Scope
	Principal                  contract.Principal
	Role                       string
	ResourceGrants             []string
	Permissions                []string
	ConnectionIDs              []string
	OperationIDs               []string
	ExpiresAt                  *time.Time
	ExpectedCollectionRevision uint64
}

func GrantMembership(ctx context.Context, tx *Transaction, request GrantMembershipRequest) (MembershipAuthority, uint64, error) {
	if err := tx.requireMatchingScope(request.Scope); err != nil {
		return MembershipAuthority{}, 0, err
	}
	if !validStoreID(request.MembershipID) || request.Principal.Kind != contract.PrincipalUser ||
		request.Principal.OrgID != request.Scope.OrgID || !validStoreName(request.Role) ||
		!validStoreResourceGrantList(request.ResourceGrants) || !validPermissionList(request.Permissions) ||
		!validStoreIDList(request.ConnectionIDs) || !validStoreOperationList(request.OperationIDs) ||
		request.ExpectedCollectionRevision == 0 {
		return MembershipAuthority{}, 0, errors.New("membership grant is invalid")
	}
	if request.ExpiresAt != nil && (request.ExpiresAt.IsZero() || request.ExpiresAt.Location() != time.UTC) {
		return MembershipAuthority{}, 0, errors.New("membership expiry must be an explicit UTC instant")
	}
	if request.ExpiresAt != nil {
		now, err := readDatabaseTime(ctx, tx)
		if err != nil {
			return MembershipAuthority{}, 0, err
		}
		if !request.ExpiresAt.After(now) {
			return MembershipAuthority{}, 0, errors.New("membership expiry must be in the future")
		}
	}
	identity, err := LoadIdentity(ctx, tx, request.Principal.PrincipalID)
	if err != nil {
		return MembershipAuthority{}, 0, err
	}
	if !identity.Principal.Equal(request.Principal) || identity.Principal.Kind != contract.PrincipalUser {
		return MembershipAuthority{}, 0, ErrNotFound
	}
	nextCollectionRevision, err := advanceIdentityCollection(ctx, tx, IdentityCollectionMemberships, request.ExpectedCollectionRevision)
	if err != nil {
		return MembershipAuthority{}, 0, err
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.memberships (
			org_id, workspace_id, membership_id, identity_id, role,
			resource_grants, permissions, connection_ids, operation_ids,
			policy_epoch, control_revision, expires_at, revoked_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 1, 1, $10, NULL)
	`,
		request.Scope.OrgID,
		request.Scope.WorkspaceID,
		request.MembershipID,
		request.Principal.PrincipalID,
		request.Role,
		request.ResourceGrants,
		request.Permissions,
		request.ConnectionIDs,
		request.OperationIDs,
		request.ExpiresAt,
	); err != nil {
		return MembershipAuthority{}, 0, safeMetadataWriteError("grant workspace membership", err)
	}
	authority, err := LoadMembershipByID(ctx, tx, request.MembershipID)
	return authority, nextCollectionRevision, err
}

type RevokeMembershipRequest struct {
	Scope            contract.Scope
	MembershipID     string
	Principal        contract.Principal
	Role             string
	ResourceGrants   []string
	Permissions      []string
	ConnectionIDs    []string
	OperationIDs     []string
	ExpiresAt        *time.Time
	ExpectedRevision uint64
}

func RevokeMembership(ctx context.Context, tx *Transaction, request RevokeMembershipRequest) (MembershipAuthority, bool, error) {
	if err := tx.requireMatchingScope(request.Scope); err != nil {
		return MembershipAuthority{}, false, err
	}
	if !validStoreID(request.MembershipID) || request.Principal.Kind != contract.PrincipalUser ||
		request.Principal.OrgID != request.Scope.OrgID || !validStoreName(request.Role) ||
		!validStoreResourceGrantList(request.ResourceGrants) || !validPermissionList(request.Permissions) ||
		!validStoreIDList(request.ConnectionIDs) || !validStoreOperationList(request.OperationIDs) ||
		request.ExpectedRevision == 0 {
		return MembershipAuthority{}, false, errors.New("membership revocation intent is invalid")
	}
	if request.ExpiresAt != nil && (request.ExpiresAt.IsZero() || request.ExpiresAt.Location() != time.UTC) {
		return MembershipAuthority{}, false, errors.New("membership revocation expiry is invalid")
	}
	authority, err := lockMembership(ctx, tx, request.MembershipID)
	if err != nil {
		return MembershipAuthority{}, false, err
	}
	if authority.ControlRevision != request.ExpectedRevision ||
		!authority.Principal.Equal(request.Principal) ||
		authority.Role != request.Role ||
		!slices.Equal(authority.ResourceGrants, request.ResourceGrants) ||
		!slices.Equal(authority.Permissions, request.Permissions) ||
		!slices.Equal(authority.ConnectionIDs, request.ConnectionIDs) ||
		!slices.Equal(authority.OperationIDs, request.OperationIDs) ||
		!equalOptionalTime(authority.ExpiresAt, request.ExpiresAt) {
		return MembershipAuthority{}, false, ErrCASConflict
	}
	if authority.RevokedAt != nil {
		return authority, true, nil
	}
	var revokedAt time.Time
	if err := tx.queryRow(ctx, `
		UPDATE pm.memberships
		SET revoked_at = clock_timestamp(), policy_epoch = policy_epoch + 1,
		    control_revision = control_revision + 1
		WHERE membership_id = $1 AND control_revision = $2 AND revoked_at IS NULL
		RETURNING revoked_at
	`, request.MembershipID, request.ExpectedRevision).Scan(&revokedAt); errors.Is(err, pgx.ErrNoRows) {
		return MembershipAuthority{}, false, ErrCASConflict
	} else if err != nil {
		return MembershipAuthority{}, false, safeMetadataWriteError("revoke workspace membership", err)
	}
	if _, err := bumpIdentityCollection(ctx, tx, IdentityCollectionMemberships); err != nil {
		return MembershipAuthority{}, false, err
	}
	authority.ControlRevision++
	authority.PolicyEpoch++
	revokedAt = revokedAt.UTC()
	authority.RevokedAt = &revokedAt
	return authority, false, nil
}

func ListMembershipAuthorities(ctx context.Context, tx *Transaction, afterID string, limit uint32) ([]MembershipAuthority, IdentityCollectionRevisions, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return nil, IdentityCollectionRevisions{}, err
	}
	if limit == 0 || limit > 101 || (afterID != "" && !validStoreID(afterID)) {
		return nil, IdentityCollectionRevisions{}, errors.New("membership list bound is invalid")
	}
	rows, err := tx.query(ctx, `
		SELECT membership_id, identity_id, role, resource_grants, permissions, connection_ids, operation_ids,
		       policy_epoch, control_revision, expires_at, revoked_at
		FROM pm.memberships
		WHERE membership_id > $1
		ORDER BY membership_id
		LIMIT $2
	`, afterID, int64(limit))
	if err != nil {
		return nil, IdentityCollectionRevisions{}, fmt.Errorf("list memberships: %w", err)
	}
	defer rows.Close()
	result := make([]MembershipAuthority, 0, limit)
	for rows.Next() {
		authority := MembershipAuthority{Scope: scope}
		if err := rows.Scan(
			&authority.MembershipID,
			&authority.Principal.PrincipalID,
			&authority.Role,
			&authority.ResourceGrants,
			&authority.Permissions,
			&authority.ConnectionIDs,
			&authority.OperationIDs,
			&authority.PolicyEpoch,
			&authority.ControlRevision,
			&authority.ExpiresAt,
			&authority.RevokedAt,
		); err != nil {
			return nil, IdentityCollectionRevisions{}, fmt.Errorf("scan membership: %w", err)
		}
		authority.Principal.OrgID = scope.OrgID
		authority.Principal.Kind = contract.PrincipalUser
		normalizeAuthorityTimes(&authority.ExpiresAt, &authority.RevokedAt)
		result = append(result, authority)
	}
	if err := rows.Err(); err != nil {
		return nil, IdentityCollectionRevisions{}, fmt.Errorf("iterate memberships: %w", err)
	}
	revisions, err := LoadIdentityCollectionRevisions(ctx, tx)
	return result, revisions, err
}

type CreateServiceAccountRequest struct {
	DeploymentID               string
	ServiceAccountID           string
	CredentialID               string
	CredentialHash             []byte
	Scope                      contract.Scope
	DisplayName                *string
	Permissions                []string
	ConnectionIDs              []string
	OperationIDs               []string
	ExpiresAt                  time.Time
	ExpectedCollectionRevision uint64
}

func CreateServiceAccount(ctx context.Context, tx *Transaction, request CreateServiceAccountRequest) (ServiceAccountAuthority, uint64, error) {
	if err := tx.requireMatchingScope(request.Scope); err != nil {
		return ServiceAccountAuthority{}, 0, err
	}
	if !validStoreID(request.DeploymentID) || !validStoreID(request.ServiceAccountID) ||
		!validStoreID(request.CredentialID) || len(request.CredentialHash) != 32 ||
		!validOptionalStoreDisplayName(request.DisplayName) ||
		!validPermissionList(request.Permissions) || !validStoreIDList(request.ConnectionIDs) ||
		!validStoreOperationList(request.OperationIDs) || request.ExpectedCollectionRevision == 0 ||
		request.ExpiresAt.IsZero() || request.ExpiresAt.Location() != time.UTC {
		return ServiceAccountAuthority{}, 0, errors.New("service account creation is invalid")
	}
	now, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return ServiceAccountAuthority{}, 0, err
	}
	if !request.ExpiresAt.After(now) {
		return ServiceAccountAuthority{}, 0, errors.New("service account expiry must be in the future")
	}
	nextCollectionRevision, err := advanceIdentityCollection(ctx, tx, IdentityCollectionServiceAccounts, request.ExpectedCollectionRevision)
	if err != nil {
		return ServiceAccountAuthority{}, 0, err
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.identities (org_id, identity_id, kind, issuer, subject, revision)
		VALUES ($1, $2, 'service_account', NULL, NULL, 1)
	`, request.Scope.OrgID, request.ServiceAccountID); err != nil {
		return ServiceAccountAuthority{}, 0, safeMetadataWriteError("insert service account identity", err)
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.service_accounts (
			org_id, workspace_id, service_account_id, display_name, permissions, connection_ids,
			operation_ids, policy_epoch, control_revision, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, 1, 1, $8)
	`,
		request.Scope.OrgID,
		request.Scope.WorkspaceID,
		request.ServiceAccountID,
		request.DisplayName,
		request.Permissions,
		request.ConnectionIDs,
		request.OperationIDs,
		request.ExpiresAt,
	); err != nil {
		return ServiceAccountAuthority{}, 0, safeMetadataWriteError("insert service account", err)
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.service_account_credentials (
			deployment_id, credential_id, org_id, workspace_id, service_account_id,
			secret_hash, issued_at, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`,
		request.DeploymentID,
		request.CredentialID,
		request.Scope.OrgID,
		request.Scope.WorkspaceID,
		request.ServiceAccountID,
		request.CredentialHash,
		now,
		request.ExpiresAt,
	); err != nil {
		return ServiceAccountAuthority{}, 0, safeMetadataWriteError("insert service account credential", err)
	}
	authority, err := LoadServiceAccountAuthority(ctx, tx, request.ServiceAccountID)
	return authority, nextCollectionRevision, err
}

type RevokeServiceAccountRequest struct {
	Scope            contract.Scope
	ServiceAccountID string
	Permissions      []string
	ConnectionIDs    []string
	OperationIDs     []string
	ExpiresAt        time.Time
	ExpectedRevision uint64
}

func RevokeServiceAccount(ctx context.Context, tx *Transaction, request RevokeServiceAccountRequest) (ServiceAccountAuthority, bool, error) {
	if err := tx.requireMatchingScope(request.Scope); err != nil {
		return ServiceAccountAuthority{}, false, err
	}
	if !validStoreID(request.ServiceAccountID) || !validPermissionList(request.Permissions) ||
		!validStoreIDList(request.ConnectionIDs) || !validStoreOperationList(request.OperationIDs) ||
		request.ExpiresAt.IsZero() || request.ExpiresAt.Location() != time.UTC ||
		request.ExpectedRevision == 0 {
		return ServiceAccountAuthority{}, false, errors.New("service account revocation intent is invalid")
	}
	authority, err := lockServiceAccount(ctx, tx, request.ServiceAccountID)
	if err != nil {
		return ServiceAccountAuthority{}, false, err
	}
	if authority.ControlRevision != request.ExpectedRevision ||
		!slices.Equal(authority.Permissions, request.Permissions) ||
		!slices.Equal(authority.ConnectionIDs, request.ConnectionIDs) ||
		!slices.Equal(authority.OperationIDs, request.OperationIDs) ||
		!authority.ExpiresAt.Equal(request.ExpiresAt) {
		return ServiceAccountAuthority{}, false, ErrCASConflict
	}
	if authority.RevokedAt != nil {
		return authority, true, nil
	}
	var revokedAt time.Time
	if err := tx.queryRow(ctx, `
		UPDATE pm.service_accounts
		SET revoked_at = clock_timestamp(), policy_epoch = policy_epoch + 1,
		    control_revision = control_revision + 1
		WHERE service_account_id = $1 AND control_revision = $2 AND revoked_at IS NULL
		RETURNING revoked_at
	`, request.ServiceAccountID, request.ExpectedRevision).Scan(&revokedAt); errors.Is(err, pgx.ErrNoRows) {
		return ServiceAccountAuthority{}, false, ErrCASConflict
	} else if err != nil {
		return ServiceAccountAuthority{}, false, safeMetadataWriteError("revoke service account", err)
	}
	if _, err := tx.exec(ctx, `
		UPDATE pm.service_account_credentials
		SET revoked_at = COALESCE(revoked_at, $2)
		WHERE service_account_id = $1
	`, request.ServiceAccountID, revokedAt); err != nil {
		return ServiceAccountAuthority{}, false, safeMetadataWriteError("revoke service account credentials", err)
	}
	if _, err := bumpIdentityCollection(ctx, tx, IdentityCollectionServiceAccounts); err != nil {
		return ServiceAccountAuthority{}, false, err
	}
	authority.ControlRevision++
	authority.PolicyEpoch++
	revokedAt = revokedAt.UTC()
	authority.RevokedAt = &revokedAt
	return authority, false, nil
}

func ListServiceAccountAuthorities(ctx context.Context, tx *Transaction, afterID string, limit uint32) ([]ServiceAccountAuthority, IdentityCollectionRevisions, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return nil, IdentityCollectionRevisions{}, err
	}
	if limit == 0 || limit > 101 || (afterID != "" && !validStoreID(afterID)) {
		return nil, IdentityCollectionRevisions{}, errors.New("service account list bound is invalid")
	}
	rows, err := tx.query(ctx, `
		SELECT service_account_id, display_name, permissions, connection_ids, operation_ids,
		       policy_epoch, control_revision, expires_at, revoked_at
		FROM pm.service_accounts
		WHERE service_account_id > $1
		ORDER BY service_account_id
		LIMIT $2
	`, afterID, int64(limit))
	if err != nil {
		return nil, IdentityCollectionRevisions{}, fmt.Errorf("list service accounts: %w", err)
	}
	defer rows.Close()
	result := make([]ServiceAccountAuthority, 0, limit)
	for rows.Next() {
		authority := ServiceAccountAuthority{Scope: scope}
		if err := rows.Scan(
			&authority.ServiceAccountID,
			&authority.DisplayName,
			&authority.Permissions,
			&authority.ConnectionIDs,
			&authority.OperationIDs,
			&authority.PolicyEpoch,
			&authority.ControlRevision,
			&authority.ExpiresAt,
			&authority.RevokedAt,
		); err != nil {
			return nil, IdentityCollectionRevisions{}, fmt.Errorf("scan service account: %w", err)
		}
		authority.Principal = contract.Principal{
			OrgID: scope.OrgID, PrincipalID: authority.ServiceAccountID, Kind: contract.PrincipalServiceAccount,
		}
		authority.ExpiresAt = authority.ExpiresAt.UTC()
		if authority.RevokedAt != nil {
			value := authority.RevokedAt.UTC()
			authority.RevokedAt = &value
		}
		result = append(result, authority)
	}
	if err := rows.Err(); err != nil {
		return nil, IdentityCollectionRevisions{}, fmt.Errorf("iterate service accounts: %w", err)
	}
	revisions, err := LoadIdentityCollectionRevisions(ctx, tx)
	return result, revisions, err
}

type CreateDelegationRequest struct {
	DelegationID               string
	Scope                      contract.Scope
	RequestedActor             contract.Principal
	EffectiveActor             contract.Principal
	ParentGrantRef             string
	ParentCeiling              contract.GrantCeiling
	ConnectionIDs              []string
	OperationIDs               []string
	ExpiresAt                  time.Time
	ExpectedCollectionRevision uint64
}

func CreateDelegation(ctx context.Context, tx *Transaction, request CreateDelegationRequest) (DelegationAuthority, uint64, error) {
	if err := tx.requireMatchingScope(request.Scope); err != nil {
		return DelegationAuthority{}, 0, err
	}
	if !validStoreID(request.DelegationID) || !validStoreID(request.ParentGrantRef) ||
		request.RequestedActor.OrgID != request.Scope.OrgID || request.EffectiveActor.OrgID != request.Scope.OrgID ||
		request.RequestedActor.Kind != contract.PrincipalServiceAccount ||
		request.EffectiveActor.Kind != contract.PrincipalUser ||
		request.RequestedActor.Equal(request.EffectiveActor) || !validStoreIDList(request.ConnectionIDs) ||
		!validStoreOperationList(request.OperationIDs) || request.ExpectedCollectionRevision == 0 ||
		request.ExpiresAt.IsZero() || request.ExpiresAt.Location() != time.UTC ||
		request.ParentCeiling.Scope != request.Scope || request.ParentCeiling.ExpiresAt.Location() != time.UTC ||
		request.ExpiresAt.After(request.ParentCeiling.ExpiresAt) {
		return DelegationAuthority{}, 0, errors.New("delegation creation is invalid")
	}
	if err := request.RequestedActor.Validate(); err != nil {
		return DelegationAuthority{}, 0, err
	}
	if err := request.EffectiveActor.Validate(); err != nil {
		return DelegationAuthority{}, 0, err
	}
	if err := request.ParentCeiling.Validate(); err != nil {
		return DelegationAuthority{}, 0, err
	}
	nextCollectionRevision, err := advanceIdentityCollection(ctx, tx, IdentityCollectionDelegations, request.ExpectedCollectionRevision)
	if err != nil {
		return DelegationAuthority{}, 0, err
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.delegations (
			org_id, workspace_id, delegation_id,
			requested_actor_id, requested_actor_kind,
			effective_actor_id, effective_actor_kind,
			parent_grant_ref, parent_connection_ids, parent_operation_ids,
			parent_approval_refs, parent_expires_at, connection_ids, operation_ids,
			policy_epoch, control_revision, expires_at, hop
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
			$11, $12, $13, $14, 1, 1, $15, 1
		)
	`,
		request.Scope.OrgID,
		request.Scope.WorkspaceID,
		request.DelegationID,
		request.RequestedActor.PrincipalID,
		string(request.RequestedActor.Kind),
		request.EffectiveActor.PrincipalID,
		string(request.EffectiveActor.Kind),
		request.ParentGrantRef,
		request.ParentCeiling.ConnectionIDs,
		request.ParentCeiling.OperationIDs,
		request.ParentCeiling.ApprovalRefs,
		request.ParentCeiling.ExpiresAt,
		request.ConnectionIDs,
		request.OperationIDs,
		request.ExpiresAt,
	); err != nil {
		return DelegationAuthority{}, 0, safeMetadataWriteError("insert delegation", err)
	}
	authority, err := LoadDelegationAuthority(ctx, tx, request.DelegationID)
	return authority, nextCollectionRevision, err
}

type RevokeDelegationRequest struct {
	Scope            contract.Scope
	DelegationID     string
	RequestedActor   contract.Principal
	EffectiveActor   contract.Principal
	ParentGrantRef   string
	ParentCeiling    contract.GrantCeiling
	ConnectionIDs    []string
	OperationIDs     []string
	ExpiresAt        time.Time
	ExpectedRevision uint64
}

func RevokeDelegation(ctx context.Context, tx *Transaction, request RevokeDelegationRequest) (DelegationAuthority, bool, error) {
	if err := tx.requireMatchingScope(request.Scope); err != nil {
		return DelegationAuthority{}, false, err
	}
	if !validStoreID(request.DelegationID) || !validStoreID(request.ParentGrantRef) ||
		request.RequestedActor.Kind != contract.PrincipalServiceAccount ||
		request.EffectiveActor.Kind != contract.PrincipalUser ||
		request.RequestedActor.OrgID != request.Scope.OrgID ||
		request.EffectiveActor.OrgID != request.Scope.OrgID ||
		!validStoreIDList(request.ConnectionIDs) || !validStoreOperationList(request.OperationIDs) ||
		request.ExpiresAt.IsZero() || request.ExpiresAt.Location() != time.UTC ||
		request.ExpectedRevision == 0 {
		return DelegationAuthority{}, false, errors.New("delegation revocation intent is invalid")
	}
	if err := request.ParentCeiling.Validate(); err != nil || request.ParentCeiling.Scope != request.Scope {
		return DelegationAuthority{}, false, errors.New("delegation parent ceiling is invalid")
	}
	authority, err := lockDelegation(ctx, tx, request.DelegationID)
	if err != nil {
		return DelegationAuthority{}, false, err
	}
	if authority.ControlRevision != request.ExpectedRevision ||
		!authority.RequestedActor.Equal(request.RequestedActor) ||
		!authority.EffectiveActor.Equal(request.EffectiveActor) ||
		authority.ParentGrantRef != request.ParentGrantRef ||
		!authority.ParentCeiling.Equal(request.ParentCeiling) ||
		!slices.Equal(authority.ConnectionIDs, request.ConnectionIDs) ||
		!slices.Equal(authority.OperationIDs, request.OperationIDs) ||
		!authority.ExpiresAt.Equal(request.ExpiresAt) {
		return DelegationAuthority{}, false, ErrCASConflict
	}
	if authority.RevokedAt != nil {
		return authority, true, nil
	}
	var revokedAt time.Time
	if err := tx.queryRow(ctx, `
		UPDATE pm.delegations
		SET revoked_at = clock_timestamp(), policy_epoch = policy_epoch + 1,
		    control_revision = control_revision + 1
		WHERE delegation_id = $1 AND control_revision = $2 AND revoked_at IS NULL
		RETURNING revoked_at
	`, request.DelegationID, request.ExpectedRevision).Scan(&revokedAt); errors.Is(err, pgx.ErrNoRows) {
		return DelegationAuthority{}, false, ErrCASConflict
	} else if err != nil {
		return DelegationAuthority{}, false, safeMetadataWriteError("revoke delegation", err)
	}
	if _, err := bumpIdentityCollection(ctx, tx, IdentityCollectionDelegations); err != nil {
		return DelegationAuthority{}, false, err
	}
	authority.ControlRevision++
	authority.PolicyEpoch++
	revokedAt = revokedAt.UTC()
	authority.RevokedAt = &revokedAt
	return authority, false, nil
}

func ListDelegationAuthorities(ctx context.Context, tx *Transaction, afterID string, limit uint32) ([]DelegationAuthority, IdentityCollectionRevisions, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return nil, IdentityCollectionRevisions{}, err
	}
	if limit == 0 || limit > 101 || (afterID != "" && !validStoreID(afterID)) {
		return nil, IdentityCollectionRevisions{}, errors.New("delegation list bound is invalid")
	}
	rows, err := tx.query(ctx, `
		SELECT delegation_id, requested_actor_id, requested_actor_kind,
		       effective_actor_id, effective_actor_kind, parent_grant_ref,
		       parent_connection_ids, parent_operation_ids, parent_approval_refs, parent_expires_at,
		       connection_ids, operation_ids, policy_epoch, control_revision,
		       expires_at, revoked_at, hop
		FROM pm.delegations
		WHERE delegation_id > $1
		ORDER BY delegation_id
		LIMIT $2
	`, afterID, int64(limit))
	if err != nil {
		return nil, IdentityCollectionRevisions{}, fmt.Errorf("list delegations: %w", err)
	}
	defer rows.Close()
	result := make([]DelegationAuthority, 0, limit)
	for rows.Next() {
		authority := DelegationAuthority{Scope: scope}
		if err := rows.Scan(
			&authority.DelegationID,
			&authority.RequestedActor.PrincipalID,
			&authority.RequestedActor.Kind,
			&authority.EffectiveActor.PrincipalID,
			&authority.EffectiveActor.Kind,
			&authority.ParentGrantRef,
			&authority.ParentCeiling.ConnectionIDs,
			&authority.ParentCeiling.OperationIDs,
			&authority.ParentCeiling.ApprovalRefs,
			&authority.ParentCeiling.ExpiresAt,
			&authority.ConnectionIDs,
			&authority.OperationIDs,
			&authority.PolicyEpoch,
			&authority.ControlRevision,
			&authority.ExpiresAt,
			&authority.RevokedAt,
			&authority.Hop,
		); err != nil {
			return nil, IdentityCollectionRevisions{}, fmt.Errorf("scan delegation: %w", err)
		}
		authority.RequestedActor.OrgID = scope.OrgID
		authority.EffectiveActor.OrgID = scope.OrgID
		authority.ParentCeiling.Scope = scope
		authority.ParentCeiling.ExpiresAt = authority.ParentCeiling.ExpiresAt.UTC()
		authority.ExpiresAt = authority.ExpiresAt.UTC()
		if authority.RevokedAt != nil {
			value := authority.RevokedAt.UTC()
			authority.RevokedAt = &value
		}
		result = append(result, authority)
	}
	if err := rows.Err(); err != nil {
		return nil, IdentityCollectionRevisions{}, fmt.Errorf("iterate delegations: %w", err)
	}
	revisions, err := LoadIdentityCollectionRevisions(ctx, tx)
	return result, revisions, err
}

type CreateConnectionRequest struct {
	Scope                      contract.Scope
	ConnectionID               string
	ConnectorID                string
	ProviderAccountID          string
	CredentialRef              string
	DisplayName                string
	ExpectedCollectionRevision uint64
}

func CreateConnectionControl(ctx context.Context, tx *Transaction, request CreateConnectionRequest) (ConnectionAuthority, uint64, error) {
	if err := tx.requireMatchingScope(request.Scope); err != nil {
		return ConnectionAuthority{}, 0, err
	}
	if !validStoreID(request.ConnectionID) || !validStoreID(request.ConnectorID) ||
		!validStoreID(request.ProviderAccountID) || (request.CredentialRef != "" && !validStoreID(request.CredentialRef)) ||
		!validDisplayName(request.DisplayName) || request.ExpectedCollectionRevision == 0 {
		return ConnectionAuthority{}, 0, errors.New("connection creation is invalid")
	}
	if _, err := LoadProviderAccount(ctx, tx, request.ProviderAccountID); err != nil {
		return ConnectionAuthority{}, 0, err
	}
	nextCollectionRevision, err := advanceIdentityCollection(ctx, tx, IdentityCollectionConnections, request.ExpectedCollectionRevision)
	if err != nil {
		return ConnectionAuthority{}, 0, err
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.connections (
			org_id, workspace_id, connection_id, connector_id, provider_account_id,
			revision, credential_ref, display_name, state
		) VALUES ($1, $2, $3, $4, $5, 1, $6, $7, 'active')
	`,
		request.Scope.OrgID,
		request.Scope.WorkspaceID,
		request.ConnectionID,
		request.ConnectorID,
		request.ProviderAccountID,
		request.CredentialRef,
		request.DisplayName,
	); err != nil {
		return ConnectionAuthority{}, 0, safeMetadataWriteError("create connection", err)
	}
	authority, err := LoadConnectionAuthority(ctx, tx, request.ConnectionID)
	return authority, nextCollectionRevision, err
}

type UpdateConnectionRequest struct {
	ConnectionID     string
	CredentialRef    string
	DisplayName      string
	ExpectedRevision uint64
}

func UpdateConnectionControl(ctx context.Context, tx *Transaction, request UpdateConnectionRequest) (ConnectionAuthority, bool, error) {
	if !validStoreID(request.ConnectionID) || (request.CredentialRef != "" && !validStoreID(request.CredentialRef)) ||
		!validDisplayName(request.DisplayName) || request.ExpectedRevision == 0 {
		return ConnectionAuthority{}, false, errors.New("connection update is invalid")
	}
	authority, err := lockConnection(ctx, tx, request.ConnectionID)
	if err != nil {
		return ConnectionAuthority{}, false, err
	}
	if authority.Revision != request.ExpectedRevision {
		return ConnectionAuthority{}, false, ErrCASConflict
	}
	if authority.State != "active" {
		return ConnectionAuthority{}, false, ErrNotFound
	}
	if authority.CredentialRef == request.CredentialRef && authority.DisplayName == request.DisplayName {
		return authority, true, nil
	}
	if err := tx.queryRow(ctx, `
		UPDATE pm.connections
		SET credential_ref = $2, display_name = $3, revision = revision + 1, updated_at = clock_timestamp()
		WHERE connection_id = $1 AND revision = $4 AND state = 'active'
		RETURNING revision
	`, request.ConnectionID, request.CredentialRef, request.DisplayName, request.ExpectedRevision).Scan(&authority.Revision); errors.Is(err, pgx.ErrNoRows) {
		return ConnectionAuthority{}, false, ErrCASConflict
	} else if err != nil {
		return ConnectionAuthority{}, false, safeMetadataWriteError("update connection", err)
	}
	authority.CredentialRef = request.CredentialRef
	authority.DisplayName = request.DisplayName
	if _, err := bumpIdentityCollection(ctx, tx, IdentityCollectionConnections); err != nil {
		return ConnectionAuthority{}, false, err
	}
	return authority, false, nil
}

func RevokeConnectionControl(ctx context.Context, tx *Transaction, connectionID string, expectedRevision uint64) (ConnectionAuthority, bool, error) {
	if !validStoreID(connectionID) || expectedRevision == 0 {
		return ConnectionAuthority{}, false, errors.New("connection revocation is invalid")
	}
	authority, err := lockConnection(ctx, tx, connectionID)
	if err != nil {
		return ConnectionAuthority{}, false, err
	}
	if authority.Revision != expectedRevision {
		return ConnectionAuthority{}, false, ErrCASConflict
	}
	if authority.State == "revoked" {
		return authority, true, nil
	}
	var revokedAt time.Time
	if err := tx.queryRow(ctx, `
		UPDATE pm.connections
		SET state = 'revoked', revoked_at = clock_timestamp(), revision = revision + 1,
		    updated_at = clock_timestamp()
		WHERE connection_id = $1 AND revision = $2 AND state = 'active'
		RETURNING revision, revoked_at
	`, connectionID, expectedRevision).Scan(&authority.Revision, &revokedAt); errors.Is(err, pgx.ErrNoRows) {
		return ConnectionAuthority{}, false, ErrCASConflict
	} else if err != nil {
		return ConnectionAuthority{}, false, safeMetadataWriteError("revoke connection", err)
	}
	authority.State = "revoked"
	revokedAt = revokedAt.UTC()
	authority.RevokedAt = &revokedAt
	if _, err := bumpIdentityCollection(ctx, tx, IdentityCollectionConnections); err != nil {
		return ConnectionAuthority{}, false, err
	}
	return authority, false, nil
}

func ListConnectionAuthorities(
	ctx context.Context,
	tx *Transaction,
	allowedConnectionIDs []string,
	afterID string,
	limit uint32,
) ([]ConnectionAuthority, IdentityCollectionRevisions, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return nil, IdentityCollectionRevisions{}, err
	}
	if !validStoreIDList(allowedConnectionIDs) || limit == 0 || limit > 101 ||
		(afterID != "" && !validStoreID(afterID)) {
		return nil, IdentityCollectionRevisions{}, errors.New("connection list bound is invalid")
	}
	rows, err := tx.query(ctx, `
		SELECT connection_id, connector_id, provider_account_id, revision, credential_ref,
		       state, COALESCE(display_name, ''), revoked_at
		FROM pm.connections
		WHERE connection_id = ANY($1::text[]) AND connection_id > $2
		ORDER BY connection_id
		LIMIT $3
	`, allowedConnectionIDs, afterID, int64(limit))
	if err != nil {
		return nil, IdentityCollectionRevisions{}, fmt.Errorf("list connections: %w", err)
	}
	defer rows.Close()
	result := make([]ConnectionAuthority, 0, limit)
	for rows.Next() {
		authority := ConnectionAuthority{Connection: Connection{Scope: scope}}
		if err := rows.Scan(
			&authority.ConnectionID,
			&authority.ConnectorID,
			&authority.ProviderAccountID,
			&authority.Revision,
			&authority.CredentialRef,
			&authority.State,
			&authority.DisplayName,
			&authority.RevokedAt,
		); err != nil {
			return nil, IdentityCollectionRevisions{}, fmt.Errorf("scan connection: %w", err)
		}
		if authority.RevokedAt != nil {
			value := authority.RevokedAt.UTC()
			authority.RevokedAt = &value
		}
		result = append(result, authority)
	}
	if err := rows.Err(); err != nil {
		return nil, IdentityCollectionRevisions{}, fmt.Errorf("iterate connections: %w", err)
	}
	revisions, err := LoadIdentityCollectionRevisions(ctx, tx)
	return result, revisions, err
}

func advanceIdentityCollection(ctx context.Context, tx *Transaction, collection IdentityCollection, expected uint64) (uint64, error) {
	if expected == 0 {
		return 0, ErrCASConflict
	}
	if err := ensureIdentityCollection(ctx, tx); err != nil {
		return 0, err
	}
	column, err := identityCollectionColumn(collection)
	if err != nil {
		return 0, err
	}
	query := fmt.Sprintf(`
		UPDATE pm.identity_collection_revisions
		SET %[1]s = %[1]s + 1, updated_at = clock_timestamp()
		WHERE %[1]s = $1
		RETURNING %[1]s
	`, column)
	var revision uint64
	if err := tx.queryRow(ctx, query, expected).Scan(&revision); errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrCASConflict
	} else if err != nil {
		return 0, safeMetadataWriteError("advance identity collection revision", err)
	}
	return revision, nil
}

func bumpIdentityCollection(ctx context.Context, tx *Transaction, collection IdentityCollection) (uint64, error) {
	if err := ensureIdentityCollection(ctx, tx); err != nil {
		return 0, err
	}
	column, err := identityCollectionColumn(collection)
	if err != nil {
		return 0, err
	}
	query := fmt.Sprintf(`
		UPDATE pm.identity_collection_revisions
		SET %[1]s = %[1]s + 1, updated_at = clock_timestamp()
		RETURNING %[1]s
	`, column)
	var revision uint64
	if err := tx.queryRow(ctx, query).Scan(&revision); err != nil {
		return 0, safeMetadataWriteError("bump identity collection revision", err)
	}
	return revision, nil
}

func ensureIdentityCollection(ctx context.Context, tx *Transaction) error {
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	if err := requireActiveWorkspace(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.exec(ctx, `
		INSERT INTO pm.identity_collection_revisions (org_id, workspace_id)
		VALUES ($1, $2)
		ON CONFLICT DO NOTHING
	`, scope.OrgID, scope.WorkspaceID); err != nil {
		return safeMetadataWriteError("ensure identity collection revisions", err)
	}
	return nil
}

func identityCollectionColumn(collection IdentityCollection) (string, error) {
	switch collection {
	case IdentityCollectionMemberships:
		return "membership_revision", nil
	case IdentityCollectionServiceAccounts:
		return "service_account_revision", nil
	case IdentityCollectionDelegations:
		return "delegation_revision", nil
	case IdentityCollectionConnections:
		return "connection_revision", nil
	default:
		return "", errors.New("identity collection is invalid")
	}
}

func lockMembership(ctx context.Context, tx *Transaction, membershipID string) (MembershipAuthority, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return MembershipAuthority{}, err
	}
	if !validStoreID(membershipID) {
		return MembershipAuthority{}, ErrNotFound
	}
	authority := MembershipAuthority{Scope: scope, MembershipID: membershipID}
	if err := tx.queryRow(ctx, `
		SELECT identity_id, role, resource_grants, permissions, connection_ids, operation_ids,
		       policy_epoch, control_revision, expires_at, revoked_at
		FROM pm.memberships
		WHERE membership_id = $1
		FOR UPDATE
	`, membershipID).Scan(
		&authority.Principal.PrincipalID,
		&authority.Role,
		&authority.ResourceGrants,
		&authority.Permissions,
		&authority.ConnectionIDs,
		&authority.OperationIDs,
		&authority.PolicyEpoch,
		&authority.ControlRevision,
		&authority.ExpiresAt,
		&authority.RevokedAt,
	); errors.Is(err, pgx.ErrNoRows) {
		return MembershipAuthority{}, ErrNotFound
	} else if err != nil {
		return MembershipAuthority{}, fmt.Errorf("lock membership: %w", err)
	}
	authority.Principal = contract.Principal{
		OrgID: scope.OrgID, PrincipalID: authority.Principal.PrincipalID, Kind: contract.PrincipalUser,
	}
	normalizeAuthorityTimes(&authority.ExpiresAt, &authority.RevokedAt)
	return authority, nil
}

func lockServiceAccount(ctx context.Context, tx *Transaction, serviceAccountID string) (ServiceAccountAuthority, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return ServiceAccountAuthority{}, err
	}
	if !validStoreID(serviceAccountID) {
		return ServiceAccountAuthority{}, ErrNotFound
	}
	authority := ServiceAccountAuthority{Scope: scope, ServiceAccountID: serviceAccountID}
	if err := tx.queryRow(ctx, `
		SELECT display_name, permissions, connection_ids, operation_ids, policy_epoch,
		       control_revision, expires_at, revoked_at
		FROM pm.service_accounts
		WHERE service_account_id = $1
		FOR UPDATE
	`, serviceAccountID).Scan(
		&authority.DisplayName,
		&authority.Permissions,
		&authority.ConnectionIDs,
		&authority.OperationIDs,
		&authority.PolicyEpoch,
		&authority.ControlRevision,
		&authority.ExpiresAt,
		&authority.RevokedAt,
	); errors.Is(err, pgx.ErrNoRows) {
		return ServiceAccountAuthority{}, ErrNotFound
	} else if err != nil {
		return ServiceAccountAuthority{}, fmt.Errorf("lock service account: %w", err)
	}
	authority.Principal = contract.Principal{
		OrgID: scope.OrgID, PrincipalID: serviceAccountID, Kind: contract.PrincipalServiceAccount,
	}
	authority.ExpiresAt = authority.ExpiresAt.UTC()
	if authority.RevokedAt != nil {
		value := authority.RevokedAt.UTC()
		authority.RevokedAt = &value
	}
	return authority, nil
}

func lockDelegation(ctx context.Context, tx *Transaction, delegationID string) (DelegationAuthority, error) {
	var lockedID string
	if err := tx.queryRow(ctx, `SELECT delegation_id FROM pm.delegations WHERE delegation_id = $1 FOR UPDATE`, delegationID).Scan(&lockedID); errors.Is(err, pgx.ErrNoRows) {
		return DelegationAuthority{}, ErrNotFound
	} else if err != nil {
		return DelegationAuthority{}, fmt.Errorf("lock delegation: %w", err)
	}
	return LoadDelegationAuthority(ctx, tx, delegationID)
}

func lockConnection(ctx context.Context, tx *Transaction, connectionID string) (ConnectionAuthority, error) {
	var lockedID string
	if err := tx.queryRow(ctx, `SELECT connection_id FROM pm.connections WHERE connection_id = $1 FOR UPDATE`, connectionID).Scan(&lockedID); errors.Is(err, pgx.ErrNoRows) {
		return ConnectionAuthority{}, ErrNotFound
	} else if err != nil {
		return ConnectionAuthority{}, fmt.Errorf("lock connection: %w", err)
	}
	return LoadConnectionAuthority(ctx, tx, connectionID)
}

func validOptionalStoreDisplayName(value *string) bool {
	return value == nil || *value == "" || len(*value) <= 256 && validStoreID(*value)
}

func validStoreResourceGrantList(values []string) bool {
	if values == nil || len(values) > 256 {
		return false
	}
	for index, value := range values {
		if value == "" || len(value) > 1024 || !utf8.ValidString(value) ||
			strings.TrimSpace(value) != value || index > 0 && values[index-1] >= value {
			return false
		}
		for _, character := range value {
			if unicode.IsControl(character) {
				return false
			}
		}
	}
	return true
}

func validStoreIDList(values []string) bool {
	if values == nil || len(values) > 256 {
		return false
	}
	for index, value := range values {
		if !validStoreID(value) || index > 0 && values[index-1] >= value {
			return false
		}
	}
	return true
}

func validStoreOperationList(values []string) bool {
	if values == nil || len(values) > 256 {
		return false
	}
	for index, value := range values {
		if value == "" || len(value) > 1024 || !utf8.ValidString(value) ||
			strings.TrimSpace(value) != value || index > 0 && values[index-1] >= value {
			return false
		}
		for _, character := range value {
			if unicode.IsControl(character) {
				return false
			}
		}
	}
	return true
}

func validPermissionList(values []string) bool {
	if values == nil || len(values) > 128 {
		return false
	}
	for index, value := range values {
		if !validStoreName(value) || index > 0 && values[index-1] >= value {
			return false
		}
	}
	return true
}

func validDisplayName(value string) bool {
	return value != "" && len(value) <= 256 && validStoreID(value)
}
