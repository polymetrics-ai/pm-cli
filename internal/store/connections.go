package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type Connection struct {
	Scope             contract.Scope
	ConnectionID      string
	ConnectorID       string
	ProviderAccountID string
	Revision          uint64
	CredentialRef     string
}

func (connection Connection) validate() error {
	if err := connection.Scope.Validate(); err != nil {
		return fmt.Errorf("validate connection scope: %w", err)
	}
	if !validStoreID(connection.ConnectionID) || !validStoreID(connection.ConnectorID) ||
		!validStoreID(connection.ProviderAccountID) {
		return errors.New("connection identity, connector, or provider account is invalid")
	}
	if connection.Revision == 0 {
		return errors.New("connection revision must be positive")
	}
	if connection.CredentialRef != "" && !validStoreID(connection.CredentialRef) {
		return errors.New("connection credential reference is invalid")
	}
	return nil
}

func RegisterConnection(ctx context.Context, tx *Transaction, connection Connection) error {
	if err := tx.requireMatchingScope(connection.Scope); err != nil {
		return err
	}
	if err := connection.validate(); err != nil {
		return err
	}
	if err := requireActiveWorkspace(ctx, tx); err != nil {
		return err
	}
	if _, err := LoadProviderAccount(ctx, tx, connection.ProviderAccountID); err != nil {
		return err
	}
	tag, err := tx.exec(ctx, `
		INSERT INTO pm.connections (
			org_id, workspace_id, connection_id, connector_id,
			provider_account_id, revision, credential_ref
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT DO NOTHING
	`,
		connection.Scope.OrgID,
		connection.Scope.WorkspaceID,
		connection.ConnectionID,
		connection.ConnectorID,
		connection.ProviderAccountID,
		connection.Revision,
		connection.CredentialRef,
	)
	if err != nil {
		return safeMetadataWriteError("register connection", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	stored, err := LoadConnection(ctx, tx, connection.ConnectionID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrImmutableConflict
		}
		return err
	}
	if stored != connection {
		return ErrImmutableConflict
	}
	return nil
}

func LoadConnection(ctx context.Context, tx *Transaction, connectionID string) (Connection, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return Connection{}, err
	}
	if !validStoreID(connectionID) {
		return Connection{}, errors.New("connection identity is invalid")
	}
	connection := Connection{Scope: scope, ConnectionID: connectionID}
	if err := tx.queryRow(ctx, `
		SELECT connector_id, provider_account_id, revision, credential_ref
		FROM pm.connections
		WHERE connection_id = $1
	`, connectionID).Scan(
		&connection.ConnectorID,
		&connection.ProviderAccountID,
		&connection.Revision,
		&connection.CredentialRef,
	); errors.Is(err, pgx.ErrNoRows) {
		return Connection{}, ErrNotFound
	} else if err != nil {
		return Connection{}, fmt.Errorf("load connection: %w", err)
	}
	if err := connection.validate(); err != nil {
		return Connection{}, fmt.Errorf("validate stored connection: %w", err)
	}
	return connection, nil
}
