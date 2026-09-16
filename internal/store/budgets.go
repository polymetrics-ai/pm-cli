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
	ErrAccountCapacity    = errors.New("provider account capacity unavailable")
	ErrAccountPermitStale = errors.New("account permit is stale")
)

const maxStoreFence = uint64(1<<63 - 1)

type AccountPermitState string

const (
	AccountPermitReserved   AccountPermitState = "reserved"
	AccountPermitDispatched AccountPermitState = "dispatched"
	AccountPermitUnknown    AccountPermitState = "unknown"
	AccountPermitResolved   AccountPermitState = "resolved"
	AccountPermitExpired    AccountPermitState = "expired"
)

type AccountPermitRequest struct {
	PermitID     string
	ConnectionID string
	OwnerID      string
	Units        int64
	TTL          time.Duration
}

type AccountPermit struct {
	Scope             contract.Scope
	PermitID          string
	ConnectionID      string
	ProviderAccountID string
	Units             int64
	State             AccountPermitState
	Fence             uint64
	OwnerID           string
	ReservedAt        time.Time
	ExpiresAt         time.Time
	ResolvedAt        *time.Time
}

func (request AccountPermitRequest) validate() error {
	if !validStoreID(request.PermitID) || !validStoreID(request.ConnectionID) || !validStoreID(request.OwnerID) {
		return errors.New("account permit request contains an invalid identity")
	}
	if request.Units <= 0 {
		return errors.New("account permit units must be positive")
	}
	if _, err := leaseMicroseconds(request.TTL, "account permit"); err != nil {
		return err
	}
	return nil
}

func (permit AccountPermit) validate() error {
	if err := permit.Scope.Validate(); err != nil {
		return err
	}
	if !validStoreID(permit.PermitID) || !validStoreID(permit.ConnectionID) || !validStoreID(permit.ProviderAccountID) || !validStoreID(permit.OwnerID) {
		return errors.New("account permit contains an invalid identity")
	}
	if permit.Units <= 0 || permit.Fence == 0 || permit.Fence > maxStoreFence {
		return errors.New("account permit contains invalid units or fence")
	}
	if permit.ReservedAt.IsZero() || permit.ExpiresAt.IsZero() || !permit.ExpiresAt.After(permit.ReservedAt) {
		return errors.New("account permit contains an invalid reservation window")
	}
	switch permit.State {
	case AccountPermitReserved, AccountPermitDispatched, AccountPermitUnknown:
		if permit.ResolvedAt != nil {
			return errors.New("active account permit has a resolution time")
		}
	case AccountPermitResolved, AccountPermitExpired:
		if permit.ResolvedAt == nil || permit.ResolvedAt.IsZero() {
			return errors.New("terminal account permit has no resolution time")
		}
	default:
		return errors.New("account permit has an invalid state")
	}
	return nil
}

func leaseMicroseconds(ttl time.Duration, subject string) (int64, error) {
	microseconds := ttl.Microseconds()
	if microseconds <= 0 {
		return 0, fmt.Errorf("%s TTL must be at least one microsecond", subject)
	}
	return microseconds, nil
}

// ReserveAccountPermit resolves the organization-owned account only through the
// current workspace's connection, then serializes on that shared account row.
// An exact replay returns the original permit and never extends its authority.
func ReserveAccountPermit(ctx context.Context, tx *Transaction, request AccountPermitRequest) (AccountPermit, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return AccountPermit{}, err
	}
	if err := request.validate(); err != nil {
		return AccountPermit{}, err
	}
	microseconds, _ := leaseMicroseconds(request.TTL, "account permit")
	connection, err := LoadConnection(ctx, tx, request.ConnectionID)
	if err != nil {
		return AccountPermit{}, err
	}
	if !connection.Scope.Equal(scope) || connection.ConnectionID != request.ConnectionID {
		return AccountPermit{}, ErrScopeMismatch
	}

	capacity, reserved, err := lockProviderAccount(ctx, tx, scope, connection.ProviderAccountID)
	if err != nil {
		return AccountPermit{}, err
	}
	existing, err := loadAccountPermit(ctx, tx, scope, request.PermitID, true)
	if err == nil {
		if existing.ConnectionID != request.ConnectionID ||
			existing.ProviderAccountID != connection.ProviderAccountID ||
			existing.Units != request.Units || existing.OwnerID != request.OwnerID ||
			existing.Fence != 1 ||
			existing.ExpiresAt.Sub(existing.ReservedAt) != time.Duration(microseconds)*time.Microsecond {
			return AccountPermit{}, ErrImmutableConflict
		}
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return AccountPermit{}, err
	}
	if request.Units > capacity-reserved {
		return AccountPermit{}, ErrAccountCapacity
	}

	tag, err := tx.exec(ctx, `
		UPDATE pm.provider_accounts
		SET reserved_units = reserved_units + $3
		WHERE org_id = $1 AND provider_account_id = $2
		  AND reserved_units = $4 AND reserved_units + $3 <= capacity
	`, scope.OrgID, connection.ProviderAccountID, request.Units, reserved)
	if err != nil {
		return AccountPermit{}, fmt.Errorf("reserve provider account capacity: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return AccountPermit{}, ErrCASConflict
	}

	permit := AccountPermit{
		Scope:             scope,
		PermitID:          request.PermitID,
		ConnectionID:      request.ConnectionID,
		ProviderAccountID: connection.ProviderAccountID,
		Units:             request.Units,
		State:             AccountPermitReserved,
		Fence:             1,
		OwnerID:           request.OwnerID,
	}
	err = tx.queryRow(ctx, `
		WITH permit_clock AS (
			SELECT clock_timestamp() AS reserved_at
		)
		INSERT INTO pm.account_permits (
			org_id, workspace_id, permit_id, connection_id, provider_account_id,
			units, state, fence, owner_id, reserved_at, expires_at, resolved_at
		)
		SELECT $1, $2, $3, $4, $5, $6, $7, 1, $8,
		       permit_clock.reserved_at,
		       permit_clock.reserved_at + make_interval(secs => $9::double precision / 1000000.0),
		       NULL
		FROM permit_clock
		RETURNING reserved_at, expires_at
	`,
		scope.OrgID,
		scope.WorkspaceID,
		request.PermitID,
		request.ConnectionID,
		connection.ProviderAccountID,
		request.Units,
		string(AccountPermitReserved),
		request.OwnerID,
		microseconds,
	).Scan(&permit.ReservedAt, &permit.ExpiresAt)
	if err != nil {
		return AccountPermit{}, safeMetadataWriteError("insert account permit", err)
	}
	permit.normalizeTimes()
	return permit, nil
}

// AuthorizeAccountPermit performs the expiry and fence CAS that a later
// physical-I/O owner must use immediately before one new send. The committed
// dispatched state is conservative durable intent, not proof of provider I/O.
func AuthorizeAccountPermit(ctx context.Context, tx *Transaction, permit AccountPermit) (AccountPermit, error) {
	if err := requirePermitState(tx, permit, AccountPermitReserved); err != nil {
		return AccountPermit{}, err
	}
	updated := permit
	err := tx.queryRow(ctx, `
		WITH permit_clock AS (
			SELECT clock_timestamp() AS checked_at
		)
		UPDATE pm.account_permits AS permit
		SET state = $10
		FROM permit_clock
		WHERE permit.org_id = $1 AND permit.workspace_id = $2 AND permit.permit_id = $3
		  AND permit.connection_id = $4 AND permit.provider_account_id = $5 AND permit.units = $6
		  AND permit.state = $7 AND permit.owner_id = $8 AND permit.fence = $9
		  AND permit.resolved_at IS NULL AND permit.expires_at > permit_clock.checked_at
		RETURNING permit.reserved_at, permit.expires_at
	`,
		permit.Scope.OrgID,
		permit.Scope.WorkspaceID,
		permit.PermitID,
		permit.ConnectionID,
		permit.ProviderAccountID,
		permit.Units,
		string(AccountPermitReserved),
		permit.OwnerID,
		permit.Fence,
		string(AccountPermitDispatched),
	).Scan(&updated.ReservedAt, &updated.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccountPermit{}, ErrAccountPermitStale
	}
	if err != nil {
		return AccountPermit{}, fmt.Errorf("authorize account permit: %w", err)
	}
	updated.State = AccountPermitDispatched
	updated.normalizeTimes()
	return updated, nil
}

// MarkAccountPermitUnknown preserves capacity after an attempted send whose
// provider outcome is not known. Expiry cannot release dispatched work.
func MarkAccountPermitUnknown(ctx context.Context, tx *Transaction, permit AccountPermit) (AccountPermit, error) {
	if err := requirePermitState(tx, permit, AccountPermitDispatched); err != nil {
		return AccountPermit{}, err
	}
	updated := permit
	err := tx.queryRow(ctx, `
		UPDATE pm.account_permits
		SET state = $10
		WHERE org_id = $1 AND workspace_id = $2 AND permit_id = $3
		  AND connection_id = $4 AND provider_account_id = $5 AND units = $6
		  AND state = $7 AND owner_id = $8 AND fence = $9 AND resolved_at IS NULL
		RETURNING reserved_at, expires_at
	`,
		permit.Scope.OrgID,
		permit.Scope.WorkspaceID,
		permit.PermitID,
		permit.ConnectionID,
		permit.ProviderAccountID,
		permit.Units,
		string(AccountPermitDispatched),
		permit.OwnerID,
		permit.Fence,
		string(AccountPermitUnknown),
	).Scan(&updated.ReservedAt, &updated.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccountPermit{}, ErrAccountPermitStale
	}
	if err != nil {
		return AccountPermit{}, fmt.Errorf("mark account permit unknown: %w", err)
	}
	updated.State = AccountPermitUnknown
	updated.normalizeTimes()
	return updated, nil
}

// ReclaimAccountPermit transfers an expired, still-unused reservation by an
// exact owner/fence CAS. The reservation remains charged and the old fence can
// never authorize a send.
func ReclaimAccountPermit(ctx context.Context, tx *Transaction, permit AccountPermit, ownerID string, ttl time.Duration) (AccountPermit, error) {
	if err := requirePermitState(tx, permit, AccountPermitReserved); err != nil {
		return AccountPermit{}, err
	}
	if !validStoreID(ownerID) {
		return AccountPermit{}, errors.New("account permit owner identity is invalid")
	}
	microseconds, err := leaseMicroseconds(ttl, "account permit")
	if err != nil {
		return AccountPermit{}, err
	}
	if permit.Fence == maxStoreFence {
		return AccountPermit{}, ErrAccountPermitStale
	}
	if _, _, err := lockProviderAccount(ctx, tx, permit.Scope, permit.ProviderAccountID); err != nil {
		return AccountPermit{}, normalizePermitTransitionError(err)
	}
	updated := permit
	err = tx.queryRow(ctx, `
		WITH permit_clock AS (
			SELECT clock_timestamp() AS reclaimed_at
		)
		UPDATE pm.account_permits AS permit
		SET owner_id = $10,
		    fence = permit.fence + 1,
		    expires_at = permit_clock.reclaimed_at + make_interval(secs => $11::double precision / 1000000.0)
		FROM permit_clock
		WHERE permit.org_id = $1 AND permit.workspace_id = $2 AND permit.permit_id = $3
		  AND permit.connection_id = $4 AND permit.provider_account_id = $5 AND permit.units = $6
		  AND permit.state = $7 AND permit.owner_id = $8 AND permit.fence = $9
		  AND permit.resolved_at IS NULL AND permit.expires_at <= permit_clock.reclaimed_at
		RETURNING permit.owner_id, permit.fence, permit.reserved_at, permit.expires_at
	`,
		permit.Scope.OrgID,
		permit.Scope.WorkspaceID,
		permit.PermitID,
		permit.ConnectionID,
		permit.ProviderAccountID,
		permit.Units,
		string(AccountPermitReserved),
		permit.OwnerID,
		permit.Fence,
		ownerID,
		microseconds,
	).Scan(&updated.OwnerID, &updated.Fence, &updated.ReservedAt, &updated.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccountPermit{}, ErrAccountPermitStale
	}
	if err != nil {
		return AccountPermit{}, fmt.Errorf("reclaim account permit: %w", err)
	}
	updated.normalizeTimes()
	return updated, nil
}

// ExpireAccountPermit releases only an unused expired reservation. An exact
// retry observes the terminal row without decrementing shared capacity again.
func ExpireAccountPermit(ctx context.Context, tx *Transaction, permit AccountPermit) (AccountPermit, error) {
	if err := requirePermitState(tx, permit, AccountPermitReserved); err != nil {
		return AccountPermit{}, err
	}
	_, reserved, err := lockProviderAccount(ctx, tx, permit.Scope, permit.ProviderAccountID)
	if err != nil {
		return AccountPermit{}, normalizePermitTransitionError(err)
	}
	current, err := loadAccountPermit(ctx, tx, permit.Scope, permit.PermitID, true)
	if err != nil {
		return AccountPermit{}, normalizePermitTransitionError(err)
	}
	if !samePermitCAS(current, permit) {
		return AccountPermit{}, ErrAccountPermitStale
	}
	if current.State == AccountPermitExpired {
		return current, nil
	}
	if current.State != AccountPermitReserved || current.ResolvedAt != nil {
		return AccountPermit{}, ErrAccountPermitStale
	}
	now, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return AccountPermit{}, err
	}
	if current.ExpiresAt.After(now) {
		return AccountPermit{}, ErrAccountPermitStale
	}
	current.State = AccountPermitExpired
	current.ResolvedAt = &now
	tag, err := tx.exec(ctx, `
		UPDATE pm.account_permits
		SET state = $10, resolved_at = $11
		WHERE org_id = $1 AND workspace_id = $2 AND permit_id = $3
		  AND connection_id = $4 AND provider_account_id = $5 AND units = $6
		  AND state = $7 AND owner_id = $8 AND fence = $9
		  AND resolved_at IS NULL AND expires_at <= $11
	`,
		current.Scope.OrgID,
		current.Scope.WorkspaceID,
		current.PermitID,
		current.ConnectionID,
		current.ProviderAccountID,
		current.Units,
		string(AccountPermitReserved),
		current.OwnerID,
		current.Fence,
		string(AccountPermitExpired),
		now,
	)
	if err != nil {
		return AccountPermit{}, fmt.Errorf("expire account permit: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return AccountPermit{}, ErrAccountPermitStale
	}
	if err := releaseProviderAccountUnits(ctx, tx, current, reserved); err != nil {
		return AccountPermit{}, err
	}
	return current, nil
}

// ResolveAccountPermit releases dispatched or unknown work only after its
// owning caller has an explicit safe terminal settlement. An exact retry is
// idempotent; no lease expiry can settle possibly dispatched work.
func ResolveAccountPermit(ctx context.Context, tx *Transaction, permit AccountPermit) (AccountPermit, error) {
	if permit.State != AccountPermitDispatched && permit.State != AccountPermitUnknown {
		return AccountPermit{}, ErrAccountPermitStale
	}
	if err := requirePermitScope(tx, permit); err != nil {
		return AccountPermit{}, err
	}
	_, reserved, err := lockProviderAccount(ctx, tx, permit.Scope, permit.ProviderAccountID)
	if err != nil {
		return AccountPermit{}, normalizePermitTransitionError(err)
	}
	current, err := loadAccountPermit(ctx, tx, permit.Scope, permit.PermitID, true)
	if err != nil {
		return AccountPermit{}, normalizePermitTransitionError(err)
	}
	if !samePermitCAS(current, permit) {
		return AccountPermit{}, ErrAccountPermitStale
	}
	if current.State == AccountPermitResolved {
		return current, nil
	}
	if current.State != permit.State || current.ResolvedAt != nil {
		return AccountPermit{}, ErrAccountPermitStale
	}
	resolvedAt, err := readDatabaseTime(ctx, tx)
	if err != nil {
		return AccountPermit{}, err
	}
	current.State = AccountPermitResolved
	current.ResolvedAt = &resolvedAt
	tag, err := tx.exec(ctx, `
		UPDATE pm.account_permits
		SET state = $10, resolved_at = $11
		WHERE org_id = $1 AND workspace_id = $2 AND permit_id = $3
		  AND connection_id = $4 AND provider_account_id = $5 AND units = $6
		  AND state = $7 AND owner_id = $8 AND fence = $9 AND resolved_at IS NULL
	`,
		current.Scope.OrgID,
		current.Scope.WorkspaceID,
		current.PermitID,
		current.ConnectionID,
		current.ProviderAccountID,
		current.Units,
		string(permit.State),
		current.OwnerID,
		current.Fence,
		string(AccountPermitResolved),
		resolvedAt,
	)
	if err != nil {
		return AccountPermit{}, fmt.Errorf("resolve account permit: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return AccountPermit{}, ErrAccountPermitStale
	}
	if err := releaseProviderAccountUnits(ctx, tx, current, reserved); err != nil {
		return AccountPermit{}, err
	}
	return current, nil
}

func LoadAccountPermit(ctx context.Context, tx *Transaction, permitID string) (AccountPermit, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return AccountPermit{}, err
	}
	if !validStoreID(permitID) {
		return AccountPermit{}, errors.New("account permit identity is invalid")
	}
	return loadAccountPermit(ctx, tx, scope, permitID, false)
}

func requirePermitState(tx *Transaction, permit AccountPermit, state AccountPermitState) error {
	if permit.State != state {
		return ErrAccountPermitStale
	}
	return requirePermitScope(tx, permit)
}

func requirePermitScope(tx *Transaction, permit AccountPermit) error {
	if err := permit.validate(); err != nil {
		return err
	}
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	if !scope.Equal(permit.Scope) {
		return ErrScopeMismatch
	}
	return nil
}

func lockProviderAccount(ctx context.Context, tx *Transaction, scope contract.Scope, accountID string) (int64, int64, error) {
	var capacity, reserved int64
	err := tx.queryRow(ctx, `
		SELECT capacity, reserved_units
		FROM pm.provider_accounts
		WHERE org_id = $1 AND provider_account_id = $2
		FOR UPDATE
	`, scope.OrgID, accountID).Scan(&capacity, &reserved)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, ErrNotFound
	}
	if err != nil {
		return 0, 0, fmt.Errorf("lock provider account capacity: %w", err)
	}
	if capacity <= 0 || reserved < 0 || reserved > capacity {
		return 0, 0, errors.New("stored provider account capacity is invalid")
	}
	return capacity, reserved, nil
}

func releaseProviderAccountUnits(ctx context.Context, tx *Transaction, permit AccountPermit, reserved int64) error {
	if reserved < permit.Units {
		return errors.New("stored provider account reservation is inconsistent")
	}
	tag, err := tx.exec(ctx, `
		UPDATE pm.provider_accounts
		SET reserved_units = reserved_units - $3
		WHERE org_id = $1 AND provider_account_id = $2
		  AND reserved_units = $4 AND reserved_units >= $3
	`, permit.Scope.OrgID, permit.ProviderAccountID, permit.Units, reserved)
	if err != nil {
		return fmt.Errorf("release provider account capacity: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrCASConflict
	}
	return nil
}

func loadAccountPermit(ctx context.Context, tx *Transaction, scope contract.Scope, permitID string, forUpdate bool) (AccountPermit, error) {
	query := `
		SELECT org_id, workspace_id, permit_id, connection_id, provider_account_id,
		       units, state, fence, owner_id, reserved_at, expires_at, resolved_at
		FROM pm.account_permits
		WHERE org_id = $1 AND workspace_id = $2 AND permit_id = $3
	`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	var permit AccountPermit
	var state string
	err := tx.queryRow(ctx, query, scope.OrgID, scope.WorkspaceID, permitID).Scan(
		&permit.Scope.OrgID,
		&permit.Scope.WorkspaceID,
		&permit.PermitID,
		&permit.ConnectionID,
		&permit.ProviderAccountID,
		&permit.Units,
		&state,
		&permit.Fence,
		&permit.OwnerID,
		&permit.ReservedAt,
		&permit.ExpiresAt,
		&permit.ResolvedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccountPermit{}, ErrNotFound
	}
	if err != nil {
		return AccountPermit{}, fmt.Errorf("load account permit: %w", err)
	}
	permit.State = AccountPermitState(state)
	permit.normalizeTimes()
	if err := permit.validate(); err != nil {
		return AccountPermit{}, fmt.Errorf("validate stored account permit: %w", err)
	}
	if !permit.Scope.Equal(scope) || permit.PermitID != permitID {
		return AccountPermit{}, ErrScopeMismatch
	}
	return permit, nil
}

func (permit *AccountPermit) normalizeTimes() {
	permit.ReservedAt = permit.ReservedAt.UTC()
	permit.ExpiresAt = permit.ExpiresAt.UTC()
	if permit.ResolvedAt != nil {
		value := permit.ResolvedAt.UTC()
		permit.ResolvedAt = &value
	}
}

func samePermitCAS(left, right AccountPermit) bool {
	return left.Scope.Equal(right.Scope) &&
		left.PermitID == right.PermitID &&
		left.ConnectionID == right.ConnectionID &&
		left.ProviderAccountID == right.ProviderAccountID &&
		left.Units == right.Units &&
		left.OwnerID == right.OwnerID &&
		left.Fence == right.Fence
}

func normalizePermitTransitionError(err error) error {
	if errors.Is(err, ErrNotFound) {
		return ErrAccountPermitStale
	}
	return err
}
