package store

import (
	"context"
	"errors"
	"fmt"

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
