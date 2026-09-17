package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

var (
	ErrCredentialLifecycleStale   = errors.New("credential lifecycle is stale")
	ErrCredentialRefreshBusy      = errors.New("credential refresh is already in progress")
	ErrCredentialRefreshUncertain = errors.New("credential refresh outcome is uncertain")
)

type CredentialLifecycleState string

const (
	CredentialLifecycleActive                  CredentialLifecycleState = "active"
	CredentialLifecycleRefreshing              CredentialLifecycleState = "refreshing"
	CredentialLifecycleReauthorizationRequired CredentialLifecycleState = "reauthorization_required"
)

type CredentialLifecycle struct {
	Scope              contract.Scope
	ConnectionID       string
	CredentialRef      string
	ProviderAccountID  string
	AccountWitness     string
	ConnectionRevision uint64
	AccountRevision    uint64
	CiphertextVersion  uint64
	Status             CredentialLifecycleState
	AuthEpoch          uint64
	Revision           uint64
	UpdatedAt          time.Time
}

func (lifecycle CredentialLifecycle) validate() error {
	if err := lifecycle.Scope.Validate(); err != nil {
		return fmt.Errorf("validate credential lifecycle scope: %w", err)
	}
	if !validStoreID(lifecycle.ConnectionID) || !validStoreID(lifecycle.CredentialRef) ||
		!validStoreID(lifecycle.ProviderAccountID) || !validStoreID(lifecycle.AccountWitness) {
		return errors.New("credential lifecycle identity is invalid")
	}
	if lifecycle.ConnectionRevision == 0 || lifecycle.ConnectionRevision > maxStoreFence ||
		lifecycle.AccountRevision == 0 || lifecycle.AccountRevision > maxStoreFence ||
		lifecycle.CiphertextVersion == 0 || lifecycle.CiphertextVersion > maxStoreFence ||
		lifecycle.AuthEpoch == 0 || lifecycle.AuthEpoch > maxStoreFence ||
		lifecycle.Revision == 0 || lifecycle.Revision > maxStoreFence {
		return errors.New("credential lifecycle witness, version, authorization epoch, or revision is invalid")
	}
	switch lifecycle.Status {
	case CredentialLifecycleActive, CredentialLifecycleRefreshing, CredentialLifecycleReauthorizationRequired:
	default:
		return errors.New("credential lifecycle status is invalid")
	}
	if lifecycle.UpdatedAt.IsZero() {
		return errors.New("credential lifecycle update time is required")
	}
	return nil
}

type CredentialLifecycleLock struct {
	Connection      Connection
	ConnectionState string
	Account         ProviderAccount
	Lifecycle       *CredentialLifecycle
}

// LockCredentialLifecycle serializes credential changes on the current
// workspace connection and its organization-owned provider account. Historical
// ciphertext is not treated as current authority; callers must compare the
// returned lifecycle with both current records before resolving or refreshing.
func LockCredentialLifecycle(ctx context.Context, tx *Transaction, connectionID string) (CredentialLifecycleLock, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return CredentialLifecycleLock{}, err
	}
	if !validStoreID(connectionID) {
		return CredentialLifecycleLock{}, errors.New("credential connection identity is invalid")
	}

	locked := CredentialLifecycleLock{
		Connection: Connection{Scope: scope, ConnectionID: connectionID},
		Account:    ProviderAccount{OrgID: scope.OrgID},
	}
	err = tx.queryRow(ctx, `
		SELECT connection.connector_id, connection.provider_account_id,
		       connection.revision, connection.credential_ref, connection.state,
		       account.provider_account_id, account.provider,
		       account.account_witness, account.capacity, account.revision
		FROM pm.connections AS connection
		JOIN pm.provider_accounts AS account
		  ON account.org_id = connection.org_id
		 AND account.provider_account_id = connection.provider_account_id
		WHERE connection.org_id = $1 AND connection.workspace_id = $2
		  AND connection.connection_id = $3
		FOR UPDATE OF connection, account
	`, scope.OrgID, scope.WorkspaceID, connectionID).Scan(
		&locked.Connection.ConnectorID,
		&locked.Connection.ProviderAccountID,
		&locked.Connection.Revision,
		&locked.Connection.CredentialRef,
		&locked.ConnectionState,
		&locked.Account.AccountID,
		&locked.Account.Provider,
		&locked.Account.AccountWitness,
		&locked.Account.Capacity,
		&locked.Account.Revision,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return CredentialLifecycleLock{}, ErrNotFound
	}
	if err != nil {
		return CredentialLifecycleLock{}, fmt.Errorf("lock credential connection and account: %w", err)
	}
	if err := locked.Connection.validate(); err != nil {
		return CredentialLifecycleLock{}, fmt.Errorf("validate locked credential connection: %w", err)
	}
	if locked.ConnectionState != "active" && locked.ConnectionState != "revoked" {
		return CredentialLifecycleLock{}, errors.New("locked credential connection state is invalid")
	}
	if err := locked.Account.validate(); err != nil {
		return CredentialLifecycleLock{}, fmt.Errorf("validate locked credential account: %w", err)
	}

	lifecycle, err := loadCredentialLifecycle(ctx, tx, connectionID, true)
	if errors.Is(err, ErrNotFound) {
		return locked, nil
	}
	if err != nil {
		return CredentialLifecycleLock{}, err
	}
	locked.Lifecycle = &lifecycle
	return locked, nil
}

func LoadCredentialLifecycle(ctx context.Context, tx *Transaction, connectionID string) (CredentialLifecycle, error) {
	return loadCredentialLifecycle(ctx, tx, connectionID, false)
}

func loadCredentialLifecycle(ctx context.Context, tx *Transaction, connectionID string, forUpdate bool) (CredentialLifecycle, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return CredentialLifecycle{}, err
	}
	if !validStoreID(connectionID) {
		return CredentialLifecycle{}, errors.New("credential connection identity is invalid")
	}
	query := `
		SELECT credential_ref, provider_account_id, account_witness,
		       connection_revision, account_revision, current_ciphertext_version,
		       status, auth_epoch, revision, updated_at
		FROM pm.credential_lifecycles
		WHERE org_id = $1 AND workspace_id = $2 AND connection_id = $3
	`
	if forUpdate {
		query += " FOR UPDATE"
	}
	lifecycle := CredentialLifecycle{Scope: scope, ConnectionID: connectionID}
	err = tx.queryRow(ctx, query, scope.OrgID, scope.WorkspaceID, connectionID).Scan(
		&lifecycle.CredentialRef,
		&lifecycle.ProviderAccountID,
		&lifecycle.AccountWitness,
		&lifecycle.ConnectionRevision,
		&lifecycle.AccountRevision,
		&lifecycle.CiphertextVersion,
		&lifecycle.Status,
		&lifecycle.AuthEpoch,
		&lifecycle.Revision,
		&lifecycle.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return CredentialLifecycle{}, ErrNotFound
	}
	if err != nil {
		return CredentialLifecycle{}, fmt.Errorf("load credential lifecycle: %w", err)
	}
	lifecycle.UpdatedAt = lifecycle.UpdatedAt.UTC()
	if err := lifecycle.validate(); err != nil {
		return CredentialLifecycle{}, fmt.Errorf("validate stored credential lifecycle: %w", err)
	}
	return lifecycle, nil
}

type CredentialMutationKey struct {
	Principal      contract.Principal
	ConnectionID   string
	IdempotencyKey string
}

func (key CredentialMutationKey) validate(scope contract.Scope) error {
	if err := key.Principal.Validate(); err != nil {
		return fmt.Errorf("validate credential mutation principal: %w", err)
	}
	if key.Principal.OrgID != scope.OrgID {
		return ErrScopeMismatch
	}
	if !validStoreID(key.ConnectionID) || !validStoreID(key.IdempotencyKey) {
		return errors.New("credential mutation connection or idempotency identity is invalid")
	}
	return nil
}

type CredentialMutation struct {
	Scope             contract.Scope
	Key               CredentialMutationKey
	IntentFingerprint string
	CredentialRef     string
	CiphertextVersion uint64
	Status            CredentialLifecycleState
	LifecycleRevision uint64
	RecordedAt        time.Time
}

func (mutation CredentialMutation) validate() error {
	if err := mutation.Scope.Validate(); err != nil {
		return err
	}
	if err := mutation.Key.validate(mutation.Scope); err != nil {
		return err
	}
	if !validFingerprint(mutation.IntentFingerprint) || !validStoreID(mutation.CredentialRef) {
		return errors.New("credential mutation fingerprint or reference is invalid")
	}
	if mutation.CiphertextVersion == 0 || mutation.CiphertextVersion > maxStoreFence ||
		mutation.LifecycleRevision == 0 || mutation.LifecycleRevision > maxStoreFence {
		return errors.New("credential mutation result version or revision is invalid")
	}
	if mutation.Status != CredentialLifecycleActive {
		return errors.New("credential mutation result status is invalid")
	}
	if mutation.RecordedAt.IsZero() {
		return errors.New("credential mutation record time is required")
	}
	return nil
}

func validFingerprint(value string) bool {
	const prefix = "hmac-sha256:"
	if len(value) != len(prefix)+64 || value[:len(prefix)] != prefix {
		return false
	}
	for _, character := range value[len(prefix):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func LoadCredentialMutation(ctx context.Context, tx *Transaction, key CredentialMutationKey) (CredentialMutation, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return CredentialMutation{}, err
	}
	if err := key.validate(scope); err != nil {
		return CredentialMutation{}, err
	}
	mutation := CredentialMutation{Scope: scope, Key: key}
	err = tx.queryRow(ctx, `
		SELECT intent_fingerprint, credential_ref, result_ciphertext_version,
		       result_status, result_lifecycle_revision, recorded_at
		FROM pm.credential_mutation_receipts
		WHERE org_id = $1 AND workspace_id = $2
		  AND principal_id = $3 AND principal_kind = $4
		  AND connection_id = $5 AND idempotency_key = $6
	`,
		scope.OrgID,
		scope.WorkspaceID,
		key.Principal.PrincipalID,
		string(key.Principal.Kind),
		key.ConnectionID,
		key.IdempotencyKey,
	).Scan(
		&mutation.IntentFingerprint,
		&mutation.CredentialRef,
		&mutation.CiphertextVersion,
		&mutation.Status,
		&mutation.LifecycleRevision,
		&mutation.RecordedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return CredentialMutation{}, ErrNotFound
	}
	if err != nil {
		return CredentialMutation{}, fmt.Errorf("load credential mutation receipt: %w", err)
	}
	mutation.RecordedAt = mutation.RecordedAt.UTC()
	if err := mutation.validate(); err != nil {
		return CredentialMutation{}, fmt.Errorf("validate stored credential mutation receipt: %w", err)
	}
	return mutation, nil
}

type CredentialVersionCommit struct {
	ExpectedCiphertextVersion uint64
	ExpectedLifecycleRevision uint64
	Next                      CredentialLifecycle
	Mutation                  CredentialMutation
}

// CommitCredentialVersion advances current status and records the safe replay
// result in the caller's transaction. The encrypted version must already have
// been registered with RegisterEncryptedSecret in that same transaction.
func CommitCredentialVersion(ctx context.Context, tx *Transaction, change CredentialVersionCommit) (CredentialLifecycle, CredentialMutation, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return CredentialLifecycle{}, CredentialMutation{}, err
	}
	if !change.Next.Scope.Equal(scope) || !change.Mutation.Scope.Equal(scope) {
		return CredentialLifecycle{}, CredentialMutation{}, ErrScopeMismatch
	}
	if change.ExpectedCiphertextVersion >= maxStoreFence ||
		change.ExpectedLifecycleRevision >= maxStoreFence {
		return CredentialLifecycle{}, CredentialMutation{}, ErrCASConflict
	}
	if change.Next.CiphertextVersion != change.ExpectedCiphertextVersion+1 ||
		change.Next.Revision != change.ExpectedLifecycleRevision+1 ||
		change.Next.Status != CredentialLifecycleActive {
		return CredentialLifecycle{}, CredentialMutation{}, ErrCASConflict
	}
	if change.Mutation.Key.ConnectionID != change.Next.ConnectionID ||
		change.Mutation.CredentialRef != change.Next.CredentialRef ||
		change.Mutation.CiphertextVersion != change.Next.CiphertextVersion ||
		change.Mutation.Status != change.Next.Status ||
		change.Mutation.LifecycleRevision != change.Next.Revision {
		return CredentialLifecycle{}, CredentialMutation{}, ErrScopeMismatch
	}
	if err := change.Mutation.Key.validate(scope); err != nil {
		return CredentialLifecycle{}, CredentialMutation{}, err
	}
	if !validFingerprint(change.Mutation.IntentFingerprint) {
		return CredentialLifecycle{}, CredentialMutation{}, errors.New("credential mutation fingerprint is invalid")
	}

	next := change.Next
	if change.ExpectedCiphertextVersion == 0 {
		if change.ExpectedLifecycleRevision != 0 || next.Revision != 1 {
			return CredentialLifecycle{}, CredentialMutation{}, ErrCASConflict
		}
		err = tx.queryRow(ctx, `
			INSERT INTO pm.credential_lifecycles (
				org_id, workspace_id, connection_id, credential_ref,
				provider_account_id, account_witness,
				connection_revision, account_revision, current_ciphertext_version,
				status, auth_epoch, revision, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 1, clock_timestamp())
			ON CONFLICT DO NOTHING
			RETURNING updated_at
		`,
			next.Scope.OrgID,
			next.Scope.WorkspaceID,
			next.ConnectionID,
			next.CredentialRef,
			next.ProviderAccountID,
			next.AccountWitness,
			next.ConnectionRevision,
			next.AccountRevision,
			next.CiphertextVersion,
			string(next.Status),
			next.AuthEpoch,
		).Scan(&next.UpdatedAt)
	} else {
		if change.ExpectedLifecycleRevision == 0 {
			return CredentialLifecycle{}, CredentialMutation{}, ErrCASConflict
		}
		err = tx.queryRow(ctx, `
			UPDATE pm.credential_lifecycles
			SET credential_ref = $4,
			    provider_account_id = $5,
			    account_witness = $6,
			    connection_revision = $7,
			    account_revision = $8,
			    current_ciphertext_version = $9,
			    status = $10,
			    auth_epoch = $11,
			    revision = $12,
			    updated_at = clock_timestamp()
			WHERE org_id = $1 AND workspace_id = $2 AND connection_id = $3
			  AND current_ciphertext_version = $13 AND revision = $14
			RETURNING updated_at
		`,
			next.Scope.OrgID,
			next.Scope.WorkspaceID,
			next.ConnectionID,
			next.CredentialRef,
			next.ProviderAccountID,
			next.AccountWitness,
			next.ConnectionRevision,
			next.AccountRevision,
			next.CiphertextVersion,
			string(next.Status),
			next.AuthEpoch,
			next.Revision,
			change.ExpectedCiphertextVersion,
			change.ExpectedLifecycleRevision,
		).Scan(&next.UpdatedAt)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return CredentialLifecycle{}, CredentialMutation{}, ErrCASConflict
	}
	if err != nil {
		return CredentialLifecycle{}, CredentialMutation{}, safeMetadataWriteError("advance credential lifecycle", err)
	}
	next.UpdatedAt = next.UpdatedAt.UTC()
	if err := next.validate(); err != nil {
		return CredentialLifecycle{}, CredentialMutation{}, err
	}

	mutation := change.Mutation
	err = tx.queryRow(ctx, `
		INSERT INTO pm.credential_mutation_receipts (
			org_id, workspace_id, principal_id, principal_kind,
			connection_id, idempotency_key, intent_fingerprint,
			credential_ref, result_ciphertext_version, result_status,
			result_lifecycle_revision, recorded_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, clock_timestamp())
		ON CONFLICT DO NOTHING
		RETURNING recorded_at
	`,
		mutation.Scope.OrgID,
		mutation.Scope.WorkspaceID,
		mutation.Key.Principal.PrincipalID,
		string(mutation.Key.Principal.Kind),
		mutation.Key.ConnectionID,
		mutation.Key.IdempotencyKey,
		mutation.IntentFingerprint,
		mutation.CredentialRef,
		mutation.CiphertextVersion,
		string(mutation.Status),
		mutation.LifecycleRevision,
	).Scan(&mutation.RecordedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CredentialLifecycle{}, CredentialMutation{}, ErrIdempotencyConflict
	}
	if err != nil {
		return CredentialLifecycle{}, CredentialMutation{}, safeMetadataWriteError("record credential mutation receipt", err)
	}
	mutation.RecordedAt = mutation.RecordedAt.UTC()
	if err := mutation.validate(); err != nil {
		return CredentialLifecycle{}, CredentialMutation{}, err
	}
	return next, mutation, nil
}

type CredentialRefreshState string

const (
	CredentialRefreshClaimed    CredentialRefreshState = "claimed"
	CredentialRefreshDispatched CredentialRefreshState = "dispatched"
	CredentialRefreshCompleted  CredentialRefreshState = "completed"
	CredentialRefreshFailed     CredentialRefreshState = "failed"
	CredentialRefreshUncertain  CredentialRefreshState = "uncertain"
)

type CredentialRefreshLease struct {
	Scope                   contract.Scope
	ProviderAccountID       string
	LeaseID                 string
	ConnectionID            string
	CredentialRef           string
	AccountWitness          string
	CiphertextVersion       uint64
	OwnerID                 string
	Fence                   uint64
	State                   CredentialRefreshState
	ClaimedAt               time.Time
	ExpiresAt               time.Time
	ResolvedAt              *time.Time
	ResultCiphertextVersion uint64
}

func (lease CredentialRefreshLease) validate() error {
	if err := lease.Scope.Validate(); err != nil {
		return err
	}
	if !validStoreID(lease.ProviderAccountID) || !validStoreID(lease.LeaseID) ||
		!validStoreID(lease.ConnectionID) || !validStoreID(lease.CredentialRef) ||
		!validStoreID(lease.AccountWitness) || !validStoreID(lease.OwnerID) {
		return errors.New("credential refresh lease identity is invalid")
	}
	if lease.CiphertextVersion == 0 || lease.CiphertextVersion > maxStoreFence ||
		lease.Fence == 0 || lease.Fence > maxStoreFence ||
		lease.ResultCiphertextVersion > maxStoreFence {
		return errors.New("credential refresh lease version or fence is invalid")
	}
	if lease.ClaimedAt.IsZero() || lease.ExpiresAt.IsZero() || !lease.ExpiresAt.After(lease.ClaimedAt) {
		return errors.New("credential refresh lease window is invalid")
	}
	if lease.ResolvedAt != nil && lease.ResolvedAt.Before(lease.ClaimedAt) {
		return errors.New("credential refresh resolution predates its claim")
	}
	switch lease.State {
	case CredentialRefreshClaimed, CredentialRefreshDispatched:
		if lease.ResolvedAt != nil || lease.ResultCiphertextVersion != 0 {
			return errors.New("active credential refresh lease carries a terminal result")
		}
	case CredentialRefreshCompleted:
		if lease.ResolvedAt == nil || lease.ResolvedAt.IsZero() ||
			lease.CiphertextVersion >= maxStoreFence ||
			lease.ResultCiphertextVersion != lease.CiphertextVersion+1 {
			return errors.New("completed credential refresh lease has no valid result")
		}
	case CredentialRefreshFailed, CredentialRefreshUncertain:
		if lease.ResolvedAt == nil || lease.ResolvedAt.IsZero() || lease.ResultCiphertextVersion != 0 {
			return errors.New("terminal credential refresh lease is invalid")
		}
	default:
		return errors.New("credential refresh lease state is invalid")
	}
	return nil
}

func (lease *CredentialRefreshLease) normalizeTimes() {
	lease.ClaimedAt = lease.ClaimedAt.UTC()
	lease.ExpiresAt = lease.ExpiresAt.UTC()
	if lease.ResolvedAt != nil {
		value := lease.ResolvedAt.UTC()
		lease.ResolvedAt = &value
	}
}

type CredentialRefreshClaimRequest struct {
	LeaseID                   string
	ConnectionID              string
	OwnerID                   string
	ExpectedFence             uint64
	ExpectedCiphertextVersion uint64
	ExpectedAccountWitness    string
	TTL                       time.Duration
}

func (request CredentialRefreshClaimRequest) validate() error {
	if !validStoreID(request.LeaseID) || !validStoreID(request.ConnectionID) ||
		!validStoreID(request.OwnerID) || !validStoreID(request.ExpectedAccountWitness) {
		return errors.New("credential refresh claim identity is invalid")
	}
	if request.ExpectedCiphertextVersion == 0 ||
		request.ExpectedCiphertextVersion >= maxStoreFence ||
		request.ExpectedFence > maxStoreFence {
		return errors.New("credential refresh expected version or fence is invalid")
	}
	if _, err := leaseMicroseconds(request.TTL, "credential refresh"); err != nil {
		return err
	}
	return nil
}

// ClaimCredentialRefresh claims the canonical provider-account row, not a
// process-local or connection-alias key. An expired pre-dispatch claim can be
// fenced to a new owner. An uncertain claim remains account-wide unless that
// same owning connection has advanced to a replacement ciphertext. A dispatched
// claim additionally remains exclusive until its bounded provider window ends.
func ClaimCredentialRefresh(ctx context.Context, tx *Transaction, request CredentialRefreshClaimRequest) (CredentialRefreshLease, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return CredentialRefreshLease{}, err
	}
	if err := request.validate(); err != nil {
		return CredentialRefreshLease{}, err
	}
	locked, err := LockCredentialLifecycle(ctx, tx, request.ConnectionID)
	if err != nil {
		return CredentialRefreshLease{}, err
	}
	if locked.Lifecycle == nil || locked.Lifecycle.Status != CredentialLifecycleActive ||
		locked.ConnectionState != "active" ||
		locked.Connection.ProviderAccountID != locked.Account.AccountID ||
		locked.Connection.CredentialRef != locked.Lifecycle.CredentialRef ||
		locked.Connection.Revision != locked.Lifecycle.ConnectionRevision ||
		locked.Account.AccountID != locked.Lifecycle.ProviderAccountID ||
		locked.Account.AccountWitness != locked.Lifecycle.AccountWitness ||
		locked.Account.Revision != locked.Lifecycle.AccountRevision ||
		locked.Lifecycle.CiphertextVersion != request.ExpectedCiphertextVersion ||
		locked.Lifecycle.AccountWitness != request.ExpectedAccountWitness {
		return CredentialRefreshLease{}, ErrCredentialLifecycleStale
	}

	existing, err := loadCredentialRefreshLease(ctx, tx, locked.Account.AccountID, true)
	if errors.Is(err, ErrNotFound) {
		if request.ExpectedFence != 0 {
			return CredentialRefreshLease{}, ErrStaleFence
		}
		microseconds, _ := leaseMicroseconds(request.TTL, "credential refresh")
		lease := CredentialRefreshLease{
			Scope:             scope,
			ProviderAccountID: locked.Account.AccountID,
			LeaseID:           request.LeaseID,
			ConnectionID:      request.ConnectionID,
			CredentialRef:     locked.Lifecycle.CredentialRef,
			AccountWitness:    locked.Lifecycle.AccountWitness,
			CiphertextVersion: locked.Lifecycle.CiphertextVersion,
			OwnerID:           request.OwnerID,
			Fence:             1,
			State:             CredentialRefreshClaimed,
		}
		err = tx.queryRow(ctx, `
			WITH refresh_clock AS (SELECT clock_timestamp() AS claimed_at)
			INSERT INTO pm.credential_refresh_leases (
				org_id, workspace_id, provider_account_id, lease_id,
				connection_id, credential_ref, account_witness,
				ciphertext_version, owner_id, fence, state,
				claimed_at, expires_at, resolved_at, result_ciphertext_version
			)
			SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, 1, $10,
			       refresh_clock.claimed_at,
			       refresh_clock.claimed_at + make_interval(secs => $11::double precision / 1000000.0),
			       NULL, NULL
			FROM refresh_clock
			ON CONFLICT DO NOTHING
			RETURNING claimed_at, expires_at
		`,
			lease.Scope.OrgID,
			lease.Scope.WorkspaceID,
			lease.ProviderAccountID,
			lease.LeaseID,
			lease.ConnectionID,
			lease.CredentialRef,
			lease.AccountWitness,
			lease.CiphertextVersion,
			lease.OwnerID,
			string(lease.State),
			microseconds,
		).Scan(&lease.ClaimedAt, &lease.ExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return CredentialRefreshLease{}, ErrCredentialRefreshBusy
		}
		if err != nil {
			return CredentialRefreshLease{}, safeMetadataWriteError("claim credential refresh", err)
		}
		lease.normalizeTimes()
		if err := lease.validate(); err != nil {
			return CredentialRefreshLease{}, err
		}
		return lease, nil
	}
	if err != nil {
		return CredentialRefreshLease{}, err
	}

	if existing.LeaseID == request.LeaseID {
		if existing.ConnectionID != request.ConnectionID || existing.OwnerID != request.OwnerID ||
			existing.CredentialRef != locked.Lifecycle.CredentialRef ||
			existing.AccountWitness != request.ExpectedAccountWitness ||
			existing.CiphertextVersion != request.ExpectedCiphertextVersion {
			return CredentialRefreshLease{}, ErrImmutableConflict
		}
		switch existing.State {
		case CredentialRefreshClaimed:
			now, timeErr := readDatabaseTime(ctx, tx)
			if timeErr != nil {
				return CredentialRefreshLease{}, timeErr
			}
			if !existing.ExpiresAt.After(now) {
				return CredentialRefreshLease{}, ErrStaleFence
			}
			return existing, nil
		case CredentialRefreshDispatched, CredentialRefreshUncertain:
			return CredentialRefreshLease{}, ErrCredentialRefreshUncertain
		case CredentialRefreshCompleted, CredentialRefreshFailed:
			return CredentialRefreshLease{}, ErrImmutableConflict
		}
	}

	now, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return CredentialRefreshLease{}, err
	}
	allowReplacement := false
	switch existing.State {
	case CredentialRefreshClaimed:
		if existing.ExpiresAt.After(now) {
			return CredentialRefreshLease{}, ErrCredentialRefreshBusy
		}
		if request.ExpectedFence != existing.Fence {
			return CredentialRefreshLease{}, ErrStaleFence
		}
		allowReplacement = true
	case CredentialRefreshDispatched:
		sameOwnerReplacement := existing.Scope.Equal(locked.Lifecycle.Scope) &&
			existing.ConnectionID == locked.Lifecycle.ConnectionID &&
			existing.CiphertextVersion != locked.Lifecycle.CiphertextVersion
		if !sameOwnerReplacement || existing.ExpiresAt.After(now) {
			return CredentialRefreshLease{}, ErrCredentialRefreshUncertain
		}
		allowReplacement = true
	case CredentialRefreshUncertain:
		if !existing.Scope.Equal(locked.Lifecycle.Scope) ||
			existing.ConnectionID != locked.Lifecycle.ConnectionID ||
			existing.CiphertextVersion == locked.Lifecycle.CiphertextVersion {
			return CredentialRefreshLease{}, ErrCredentialRefreshUncertain
		}
		allowReplacement = true
	case CredentialRefreshCompleted, CredentialRefreshFailed:
		if request.ExpectedFence != 0 && request.ExpectedFence != existing.Fence {
			return CredentialRefreshLease{}, ErrStaleFence
		}
		allowReplacement = true
	}
	if !allowReplacement || existing.Fence == maxStoreFence {
		return CredentialRefreshLease{}, ErrStaleFence
	}

	microseconds, _ := leaseMicroseconds(request.TTL, "credential refresh")
	lease := CredentialRefreshLease{
		Scope:             scope,
		ProviderAccountID: locked.Account.AccountID,
		LeaseID:           request.LeaseID,
		ConnectionID:      request.ConnectionID,
		CredentialRef:     locked.Lifecycle.CredentialRef,
		AccountWitness:    locked.Lifecycle.AccountWitness,
		CiphertextVersion: locked.Lifecycle.CiphertextVersion,
		OwnerID:           request.OwnerID,
		Fence:             existing.Fence + 1,
		State:             CredentialRefreshClaimed,
	}
	err = tx.queryRow(ctx, `
		WITH refresh_clock AS (SELECT clock_timestamp() AS claimed_at)
		UPDATE pm.credential_refresh_leases AS lease
		SET workspace_id = $2,
		    lease_id = $4,
		    connection_id = $5,
		    credential_ref = $6,
		    account_witness = $7,
		    ciphertext_version = $8,
		    owner_id = $9,
		    fence = lease.fence + 1,
		    state = $10,
		    claimed_at = refresh_clock.claimed_at,
		    expires_at = refresh_clock.claimed_at + make_interval(secs => $11::double precision / 1000000.0),
		    resolved_at = NULL,
		    result_ciphertext_version = NULL
		FROM refresh_clock
		WHERE lease.org_id = $1 AND lease.provider_account_id = $3
		  AND lease.fence = $12
		RETURNING lease.fence, lease.claimed_at, lease.expires_at
	`,
		lease.Scope.OrgID,
		lease.Scope.WorkspaceID,
		lease.ProviderAccountID,
		lease.LeaseID,
		lease.ConnectionID,
		lease.CredentialRef,
		lease.AccountWitness,
		lease.CiphertextVersion,
		lease.OwnerID,
		string(lease.State),
		microseconds,
		existing.Fence,
	).Scan(&lease.Fence, &lease.ClaimedAt, &lease.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CredentialRefreshLease{}, ErrStaleFence
	}
	if err != nil {
		return CredentialRefreshLease{}, safeMetadataWriteError("reclaim credential refresh", err)
	}
	lease.normalizeTimes()
	if err := lease.validate(); err != nil {
		return CredentialRefreshLease{}, err
	}
	return lease, nil
}

func LoadCredentialRefreshLease(ctx context.Context, tx *Transaction, connectionID string) (CredentialRefreshLease, error) {
	connection, err := LoadConnection(ctx, tx, connectionID)
	if err != nil {
		return CredentialRefreshLease{}, err
	}
	return loadCredentialRefreshLease(ctx, tx, connection.ProviderAccountID, false)
}

func loadCredentialRefreshLease(ctx context.Context, tx *Transaction, providerAccountID string, forUpdate bool) (CredentialRefreshLease, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return CredentialRefreshLease{}, err
	}
	if !validStoreID(providerAccountID) {
		return CredentialRefreshLease{}, errors.New("credential refresh account identity is invalid")
	}
	query := `
		SELECT workspace_id, lease_id, connection_id, credential_ref,
		       account_witness, ciphertext_version, owner_id, fence, state,
		       claimed_at, expires_at, resolved_at,
		       COALESCE(result_ciphertext_version, 0)
		FROM pm.credential_refresh_leases
		WHERE org_id = $1 AND provider_account_id = $2
	`
	if forUpdate {
		query += " FOR UPDATE"
	}
	lease := CredentialRefreshLease{ProviderAccountID: providerAccountID}
	lease.Scope.OrgID = scope.OrgID
	err = tx.queryRow(ctx, query, scope.OrgID, providerAccountID).Scan(
		&lease.Scope.WorkspaceID,
		&lease.LeaseID,
		&lease.ConnectionID,
		&lease.CredentialRef,
		&lease.AccountWitness,
		&lease.CiphertextVersion,
		&lease.OwnerID,
		&lease.Fence,
		&lease.State,
		&lease.ClaimedAt,
		&lease.ExpiresAt,
		&lease.ResolvedAt,
		&lease.ResultCiphertextVersion,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return CredentialRefreshLease{}, ErrNotFound
	}
	if err != nil {
		return CredentialRefreshLease{}, fmt.Errorf("load credential refresh lease: %w", err)
	}
	lease.normalizeTimes()
	if err := lease.validate(); err != nil {
		return CredentialRefreshLease{}, fmt.Errorf("validate stored credential refresh lease: %w", err)
	}
	return lease, nil
}

// AuthorizeCredentialRefresh consumes a live claim and marks the current
// ciphertext unavailable for new resolutions before the caller performs the
// one admitted remote refresh operation.
func AuthorizeCredentialRefresh(ctx context.Context, tx *Transaction, lease CredentialRefreshLease) (CredentialLifecycle, CredentialRefreshLease, error) {
	if err := lease.validate(); err != nil {
		return CredentialLifecycle{}, CredentialRefreshLease{}, err
	}
	locked, err := LockCredentialLifecycle(ctx, tx, lease.ConnectionID)
	if err != nil {
		return CredentialLifecycle{}, CredentialRefreshLease{}, err
	}
	if locked.Lifecycle == nil || locked.Lifecycle.Status != CredentialLifecycleActive ||
		locked.ConnectionState != "active" ||
		!locked.Lifecycle.Scope.Equal(lease.Scope) ||
		locked.Connection.Revision != locked.Lifecycle.ConnectionRevision ||
		locked.Account.AccountID != lease.ProviderAccountID ||
		locked.Account.AccountWitness != lease.AccountWitness ||
		locked.Account.Revision != locked.Lifecycle.AccountRevision ||
		locked.Lifecycle.ProviderAccountID != lease.ProviderAccountID ||
		locked.Lifecycle.AccountWitness != lease.AccountWitness ||
		locked.Lifecycle.CredentialRef != lease.CredentialRef ||
		locked.Lifecycle.CiphertextVersion != lease.CiphertextVersion ||
		locked.Lifecycle.Revision >= maxStoreFence {
		return CredentialLifecycle{}, CredentialRefreshLease{}, ErrCredentialLifecycleStale
	}
	current, err := loadCredentialRefreshLease(ctx, tx, lease.ProviderAccountID, true)
	if err != nil {
		return CredentialLifecycle{}, CredentialRefreshLease{}, err
	}
	if !sameCredentialRefreshFence(current, lease) || current.State != CredentialRefreshClaimed {
		return CredentialLifecycle{}, CredentialRefreshLease{}, ErrStaleFence
	}
	now, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return CredentialLifecycle{}, CredentialRefreshLease{}, err
	}
	if !current.ExpiresAt.After(now) {
		return CredentialLifecycle{}, CredentialRefreshLease{}, ErrStaleFence
	}

	tag, err := tx.exec(ctx, `
		UPDATE pm.credential_refresh_leases
		SET state = $8
		WHERE org_id = $1 AND provider_account_id = $2
		  AND lease_id = $3 AND owner_id = $4 AND fence = $5
		  AND state = $6 AND expires_at > clock_timestamp()
		  AND ciphertext_version = $7
	`,
		lease.Scope.OrgID,
		lease.ProviderAccountID,
		lease.LeaseID,
		lease.OwnerID,
		lease.Fence,
		string(CredentialRefreshClaimed),
		lease.CiphertextVersion,
		string(CredentialRefreshDispatched),
	)
	if err != nil {
		return CredentialLifecycle{}, CredentialRefreshLease{}, fmt.Errorf("dispatch credential refresh lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return CredentialLifecycle{}, CredentialRefreshLease{}, ErrStaleFence
	}

	updated := *locked.Lifecycle
	err = tx.queryRow(ctx, `
		UPDATE pm.credential_lifecycles
		SET status = $6, revision = revision + 1, updated_at = clock_timestamp()
		WHERE org_id = $1 AND workspace_id = $2 AND connection_id = $3
		  AND current_ciphertext_version = $4 AND revision = $5 AND status = $7
		RETURNING revision, updated_at
	`,
		updated.Scope.OrgID,
		updated.Scope.WorkspaceID,
		updated.ConnectionID,
		updated.CiphertextVersion,
		updated.Revision,
		string(CredentialLifecycleRefreshing),
		string(CredentialLifecycleActive),
	).Scan(&updated.Revision, &updated.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CredentialLifecycle{}, CredentialRefreshLease{}, ErrCredentialLifecycleStale
	}
	if err != nil {
		return CredentialLifecycle{}, CredentialRefreshLease{}, fmt.Errorf("mark credential refresh in progress: %w", err)
	}
	updated.Status = CredentialLifecycleRefreshing
	updated.UpdatedAt = updated.UpdatedAt.UTC()
	current.State = CredentialRefreshDispatched
	return updated, current, nil
}

func sameCredentialRefreshFence(left, right CredentialRefreshLease) bool {
	return left.Scope.Equal(right.Scope) &&
		left.ProviderAccountID == right.ProviderAccountID &&
		left.LeaseID == right.LeaseID &&
		left.ConnectionID == right.ConnectionID &&
		left.CredentialRef == right.CredentialRef &&
		left.AccountWitness == right.AccountWitness &&
		left.CiphertextVersion == right.CiphertextVersion &&
		left.OwnerID == right.OwnerID &&
		left.Fence == right.Fence
}

func CompleteCredentialRefresh(ctx context.Context, tx *Transaction, lease CredentialRefreshLease, next contract.CredentialRef) (CredentialLifecycle, CredentialRefreshLease, error) {
	if err := lease.validate(); err != nil {
		return CredentialLifecycle{}, CredentialRefreshLease{}, err
	}
	if err := tx.requireMatchingScope(lease.Scope); err != nil {
		return CredentialLifecycle{}, CredentialRefreshLease{}, err
	}
	if err := next.Validate(); err != nil {
		return CredentialLifecycle{}, CredentialRefreshLease{}, err
	}
	if next.CiphertextVersion != lease.CiphertextVersion+1 ||
		next.WorkspaceID != lease.Scope.WorkspaceID ||
		next.ConnectionID != lease.ConnectionID ||
		next.CredentialRef != lease.CredentialRef ||
		next.ProviderAccountWitness != lease.AccountWitness {
		return CredentialLifecycle{}, CredentialRefreshLease{}, ErrCredentialLifecycleStale
	}
	var preservesBinding bool
	err := tx.queryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pm.encrypted_secrets
			WHERE org_id = $1 AND workspace_id = $2
			  AND credential_ref = $3 AND ciphertext_version = $4
			  AND connection_id = $5 AND provider_account_id = $6
			  AND account_witness = $7 AND key_ref = $8
			  AND scopes = to_jsonb($9::text[]) AND auth_epoch = $10
		)
	`,
		lease.Scope.OrgID,
		lease.Scope.WorkspaceID,
		lease.CredentialRef,
		lease.CiphertextVersion,
		lease.ConnectionID,
		lease.ProviderAccountID,
		lease.AccountWitness,
		next.KeyRef,
		next.Scopes,
		next.AuthEpoch,
	).Scan(&preservesBinding)
	if err != nil {
		return CredentialLifecycle{}, CredentialRefreshLease{}, fmt.Errorf("verify refreshed credential binding: %w", err)
	}
	if !preservesBinding {
		return CredentialLifecycle{}, CredentialRefreshLease{}, ErrCredentialLifecycleStale
	}
	return finishCredentialRefresh(ctx, tx, lease, CredentialRefreshCompleted, next)
}

func FailCredentialRefresh(ctx context.Context, tx *Transaction, lease CredentialRefreshLease) (CredentialLifecycle, CredentialRefreshLease, error) {
	return finishCredentialRefresh(ctx, tx, lease, CredentialRefreshFailed, contract.CredentialRef{})
}

func finishCredentialRefresh(ctx context.Context, tx *Transaction, lease CredentialRefreshLease, terminal CredentialRefreshState, next contract.CredentialRef) (CredentialLifecycle, CredentialRefreshLease, error) {
	if err := lease.validate(); err != nil {
		return CredentialLifecycle{}, CredentialRefreshLease{}, err
	}
	if lease.State != CredentialRefreshDispatched || (terminal != CredentialRefreshCompleted && terminal != CredentialRefreshFailed) {
		return CredentialLifecycle{}, CredentialRefreshLease{}, ErrStaleFence
	}
	locked, err := LockCredentialLifecycle(ctx, tx, lease.ConnectionID)
	if err != nil {
		return CredentialLifecycle{}, CredentialRefreshLease{}, err
	}
	if locked.Lifecycle == nil || locked.Lifecycle.Status != CredentialLifecycleRefreshing ||
		locked.ConnectionState != "active" ||
		!locked.Lifecycle.Scope.Equal(lease.Scope) ||
		locked.Connection.CredentialRef != lease.CredentialRef ||
		locked.Connection.Revision != locked.Lifecycle.ConnectionRevision ||
		locked.Account.AccountID != lease.ProviderAccountID ||
		locked.Account.AccountWitness != lease.AccountWitness ||
		locked.Account.Revision != locked.Lifecycle.AccountRevision ||
		locked.Lifecycle.ProviderAccountID != lease.ProviderAccountID ||
		locked.Lifecycle.AccountWitness != lease.AccountWitness ||
		locked.Lifecycle.CiphertextVersion != lease.CiphertextVersion ||
		locked.Lifecycle.Revision >= maxStoreFence {
		return CredentialLifecycle{}, CredentialRefreshLease{}, ErrCredentialLifecycleStale
	}
	current, err := loadCredentialRefreshLease(ctx, tx, lease.ProviderAccountID, true)
	if err != nil {
		return CredentialLifecycle{}, CredentialRefreshLease{}, err
	}
	if !sameCredentialRefreshFence(current, lease) || current.State != CredentialRefreshDispatched {
		return CredentialLifecycle{}, CredentialRefreshLease{}, ErrStaleFence
	}
	now, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return CredentialLifecycle{}, CredentialRefreshLease{}, err
	}
	if !current.ExpiresAt.After(now) {
		return CredentialLifecycle{}, CredentialRefreshLease{}, ErrStaleFence
	}

	updated := *locked.Lifecycle
	if terminal == CredentialRefreshCompleted {
		updated.CiphertextVersion = next.CiphertextVersion
		updated.AuthEpoch = next.AuthEpoch
	}
	updated.Status = CredentialLifecycleActive
	err = tx.queryRow(ctx, `
		UPDATE pm.credential_lifecycles
		SET current_ciphertext_version = $6,
		    status = $7,
		    auth_epoch = $8,
		    revision = revision + 1,
		    updated_at = clock_timestamp()
		WHERE org_id = $1 AND workspace_id = $2 AND connection_id = $3
		  AND current_ciphertext_version = $4 AND revision = $5
		  AND status = $9
		RETURNING revision, updated_at
	`,
		updated.Scope.OrgID,
		updated.Scope.WorkspaceID,
		updated.ConnectionID,
		lease.CiphertextVersion,
		locked.Lifecycle.Revision,
		updated.CiphertextVersion,
		string(updated.Status),
		updated.AuthEpoch,
		string(CredentialLifecycleRefreshing),
	).Scan(&updated.Revision, &updated.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CredentialLifecycle{}, CredentialRefreshLease{}, ErrCredentialLifecycleStale
	}
	if err != nil {
		return CredentialLifecycle{}, CredentialRefreshLease{}, fmt.Errorf("finish credential lifecycle refresh: %w", err)
	}
	updated.UpdatedAt = updated.UpdatedAt.UTC()

	resultVersion := any(nil)
	if terminal == CredentialRefreshCompleted {
		resultVersion = next.CiphertextVersion
	}
	err = tx.queryRow(ctx, `
		UPDATE pm.credential_refresh_leases
		SET state = $6, resolved_at = clock_timestamp(), result_ciphertext_version = $7
		WHERE org_id = $1 AND provider_account_id = $2
		  AND lease_id = $3 AND owner_id = $4 AND fence = $5
		  AND state = $8 AND expires_at > clock_timestamp()
		RETURNING resolved_at, COALESCE(result_ciphertext_version, 0)
	`,
		lease.Scope.OrgID,
		lease.ProviderAccountID,
		lease.LeaseID,
		lease.OwnerID,
		lease.Fence,
		string(terminal),
		resultVersion,
		string(CredentialRefreshDispatched),
	).Scan(&current.ResolvedAt, &current.ResultCiphertextVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return CredentialLifecycle{}, CredentialRefreshLease{}, ErrStaleFence
	}
	if err != nil {
		return CredentialLifecycle{}, CredentialRefreshLease{}, fmt.Errorf("finish credential refresh lease: %w", err)
	}
	current.State = terminal
	current.normalizeTimes()
	return updated, current, nil
}

// MarkCredentialRefreshUncertain is deliberately allowed after lease expiry or
// a current connection/account revision change: it can only narrow authority
// and never changes ciphertext. A dispatched refresh that lost its rotating
// response remains blocked until an authorized replacement credential advances
// the lifecycle version.
func MarkCredentialRefreshUncertain(ctx context.Context, tx *Transaction, lease CredentialRefreshLease) (CredentialLifecycle, CredentialRefreshLease, error) {
	if err := lease.validate(); err != nil {
		return CredentialLifecycle{}, CredentialRefreshLease{}, err
	}
	locked, err := LockCredentialLifecycle(ctx, tx, lease.ConnectionID)
	if err != nil {
		return CredentialLifecycle{}, CredentialRefreshLease{}, err
	}
	if locked.Lifecycle == nil || locked.Lifecycle.Status != CredentialLifecycleRefreshing ||
		!locked.Lifecycle.Scope.Equal(lease.Scope) ||
		locked.Lifecycle.ProviderAccountID != lease.ProviderAccountID ||
		locked.Lifecycle.CredentialRef != lease.CredentialRef ||
		locked.Lifecycle.CiphertextVersion != lease.CiphertextVersion ||
		locked.Lifecycle.Revision >= maxStoreFence {
		return CredentialLifecycle{}, CredentialRefreshLease{}, ErrCredentialLifecycleStale
	}
	current, err := loadCredentialRefreshLease(ctx, tx, lease.ProviderAccountID, true)
	if err != nil {
		return CredentialLifecycle{}, CredentialRefreshLease{}, err
	}
	if !sameCredentialRefreshFence(current, lease) || current.State != CredentialRefreshDispatched {
		return CredentialLifecycle{}, CredentialRefreshLease{}, ErrStaleFence
	}

	updated := *locked.Lifecycle
	err = tx.queryRow(ctx, `
		UPDATE pm.credential_lifecycles
		SET status = $6, revision = revision + 1, updated_at = clock_timestamp()
		WHERE org_id = $1 AND workspace_id = $2 AND connection_id = $3
		  AND current_ciphertext_version = $4 AND revision = $5 AND status = $7
		RETURNING revision, updated_at
	`,
		updated.Scope.OrgID,
		updated.Scope.WorkspaceID,
		updated.ConnectionID,
		updated.CiphertextVersion,
		updated.Revision,
		string(CredentialLifecycleReauthorizationRequired),
		string(CredentialLifecycleRefreshing),
	).Scan(&updated.Revision, &updated.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CredentialLifecycle{}, CredentialRefreshLease{}, ErrCredentialLifecycleStale
	}
	if err != nil {
		return CredentialLifecycle{}, CredentialRefreshLease{}, fmt.Errorf("mark credential lifecycle uncertain: %w", err)
	}
	updated.Status = CredentialLifecycleReauthorizationRequired
	updated.UpdatedAt = updated.UpdatedAt.UTC()

	err = tx.queryRow(ctx, `
		UPDATE pm.credential_refresh_leases
		SET state = $6, resolved_at = clock_timestamp(), result_ciphertext_version = NULL
		WHERE org_id = $1 AND provider_account_id = $2
		  AND lease_id = $3 AND owner_id = $4 AND fence = $5 AND state = $7
		RETURNING resolved_at
	`,
		lease.Scope.OrgID,
		lease.ProviderAccountID,
		lease.LeaseID,
		lease.OwnerID,
		lease.Fence,
		string(CredentialRefreshUncertain),
		string(CredentialRefreshDispatched),
	).Scan(&current.ResolvedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CredentialLifecycle{}, CredentialRefreshLease{}, ErrStaleFence
	}
	if err != nil {
		return CredentialLifecycle{}, CredentialRefreshLease{}, fmt.Errorf("mark credential refresh lease uncertain: %w", err)
	}
	current.State = CredentialRefreshUncertain
	current.ResultCiphertextVersion = 0
	current.normalizeTimes()
	return updated, current, nil
}
