package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type ProviderAccount struct {
	OrgID          string
	AccountID      string
	Provider       string
	AccountWitness string
	Capacity       int64
	Revision       uint64
}

func (account ProviderAccount) validate() error {
	if !validStoreID(account.OrgID) || !validStoreID(account.AccountID) ||
		!validStoreName(account.Provider) || !validStoreID(account.AccountWitness) {
		return errors.New("provider account identity or provider is invalid")
	}
	if account.Capacity <= 0 || account.Revision == 0 {
		return errors.New("provider account capacity and revision must be positive")
	}
	return nil
}

func RegisterProviderAccount(ctx context.Context, tx *Transaction, account ProviderAccount) error {
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	if err := account.validate(); err != nil {
		return err
	}
	if account.OrgID != scope.OrgID {
		return ErrScopeMismatch
	}
	if err := requireActiveWorkspace(ctx, tx); err != nil {
		return err
	}
	tag, err := tx.exec(ctx, `
		INSERT INTO pm.provider_accounts (
			org_id, provider_account_id, provider, account_witness, capacity, revision
		) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT DO NOTHING
	`, account.OrgID, account.AccountID, account.Provider, account.AccountWitness, account.Capacity, account.Revision)
	if err != nil {
		return safeMetadataWriteError("register provider account", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	stored, err := LoadProviderAccount(ctx, tx, account.AccountID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrImmutableConflict
		}
		return err
	}
	if stored != account {
		return ErrImmutableConflict
	}
	return nil
}

func LoadProviderAccount(ctx context.Context, tx *Transaction, accountID string) (ProviderAccount, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return ProviderAccount{}, err
	}
	if !validStoreID(accountID) {
		return ProviderAccount{}, errors.New("provider account identity is invalid")
	}
	account := ProviderAccount{OrgID: scope.OrgID, AccountID: accountID}
	if err := tx.queryRow(ctx, `
		SELECT provider, account_witness, capacity, revision
		FROM pm.provider_accounts
		WHERE org_id = $1 AND provider_account_id = $2
	`, scope.OrgID, accountID).Scan(
		&account.Provider,
		&account.AccountWitness,
		&account.Capacity,
		&account.Revision,
	); errors.Is(err, pgx.ErrNoRows) {
		return ProviderAccount{}, ErrNotFound
	} else if err != nil {
		return ProviderAccount{}, fmt.Errorf("load provider account: %w", err)
	}
	if err := account.validate(); err != nil {
		return ProviderAccount{}, fmt.Errorf("validate stored provider account: %w", err)
	}
	return account, nil
}

// AccountCooldownError carries the database-owned deadline that another worker
// must respect. It does not authorize replay or release an existing reservation.
type AccountCooldownError struct {
	NotBefore time.Time
}

func (err *AccountCooldownError) Error() string { return ErrAccountCooldown.Error() }

func (err *AccountCooldownError) Unwrap() error { return ErrAccountCooldown }

// RecordAccountCooldown retains a provider observation against the exact
// dispatched permit. A late response may extend the shared deadline after the
// permit expires, but cannot shorten it or change another account's authority.
func RecordAccountCooldown(ctx context.Context, tx *Transaction, permit AccountPermit, notBefore time.Time) error {
	if err := requirePermitScope(tx, permit); err != nil {
		return err
	}
	if permit.State != AccountPermitDispatched && permit.State != AccountPermitUnknown {
		return ErrAccountPermitStale
	}
	if notBefore.IsZero() || notBefore.Location() != time.UTC || notBefore.Year() < 1 || notBefore.Year() > 9999 {
		return errors.New("provider cooldown must be a finite UTC instant")
	}
	// PostgreSQL timestamps have microsecond resolution. Round upwards so
	// serialization can never make a received delay expire early.
	if remainder := notBefore.Nanosecond() % 1000; remainder != 0 {
		notBefore = notBefore.Add(time.Microsecond - time.Duration(remainder))
	}
	if notBefore.Year() > 9999 {
		return errors.New("provider cooldown exceeds the supported timestamp range")
	}
	if _, _, err := lockProviderAccount(ctx, tx, permit.Scope, permit.ProviderAccountID); err != nil {
		return normalizePermitTransitionError(err)
	}
	current, err := loadAccountPermit(ctx, tx, permit.Scope, permit.PermitID, true)
	if err != nil {
		return normalizePermitTransitionError(err)
	}
	if !samePermitCAS(current, permit) ||
		!current.ReservedAt.Equal(permit.ReservedAt) || !current.ExpiresAt.Equal(permit.ExpiresAt) ||
		(current.State != AccountPermitDispatched && current.State != AccountPermitUnknown) {
		return ErrAccountPermitStale
	}
	tag, err := tx.exec(ctx, `
		UPDATE pm.provider_accounts
		SET cooldown_until = GREATEST(cooldown_until, $3)
		WHERE org_id = $1 AND provider_account_id = $2
	`, permit.Scope.OrgID, permit.ProviderAccountID, notBefore)
	if err != nil {
		return fmt.Errorf("record provider account cooldown: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrAccountPermitStale
	}
	return nil
}

// CheckAccountCooldown uses database time and the same canonical account lock
// as permit reservation. The caller still owns current permit, grant and fence
// checks; an elapsed cooldown is not physical-send authority.
func CheckAccountCooldown(ctx context.Context, tx *Transaction, permit AccountPermit) error {
	if err := requirePermitScope(tx, permit); err != nil {
		return err
	}
	if _, _, err := lockProviderAccount(ctx, tx, permit.Scope, permit.ProviderAccountID); err != nil {
		return normalizePermitTransitionError(err)
	}
	return requireAccountCooldownElapsed(ctx, tx, permit.Scope.OrgID, permit.ProviderAccountID)
}

// The caller holds the provider-account row lock until its transaction commits.
func requireAccountCooldownElapsed(ctx context.Context, tx *Transaction, orgID, accountID string) error {
	var notBefore *time.Time
	var now time.Time
	if err := tx.queryRow(ctx, `
		SELECT cooldown_until, clock_timestamp()
		FROM pm.provider_accounts
		WHERE org_id = $1 AND provider_account_id = $2
	`, orgID, accountID).Scan(&notBefore, &now); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return fmt.Errorf("read provider account cooldown: %w", err)
	}
	if notBefore != nil && notBefore.After(now) {
		return &AccountCooldownError{NotBefore: notBefore.UTC()}
	}
	return nil
}
