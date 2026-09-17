package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

var ErrWorkspaceRetired = errors.New("workspace namespace is retired")

type WorkspaceIdentity struct {
	Scope contract.Scope
	Name  string
}

type WorkspaceRecord struct {
	Identity     WorkspaceIdentity
	CreatedAt    time.Time
	TombstonedAt *time.Time
}

func RegisterWorkspace(ctx context.Context, tx *Transaction, workspace WorkspaceIdentity) error {
	if err := tx.requireMatchingScope(workspace.Scope); err != nil {
		return err
	}
	if !validStoreID(workspace.Name) {
		return errors.New("workspace name is invalid")
	}
	tag, err := tx.exec(ctx, `
		INSERT INTO pm.workspace_namespaces (org_id, workspace_id, name)
		VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING
	`, workspace.Scope.OrgID, workspace.Scope.WorkspaceID, workspace.Name)
	if err != nil {
		return safeMetadataWriteError("register workspace namespace", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	stored, err := LoadWorkspace(ctx, tx)
	if err != nil {
		return err
	}
	if stored.TombstonedAt != nil {
		return ErrWorkspaceRetired
	}
	if stored.Identity != workspace {
		return ErrImmutableConflict
	}
	return nil
}

func LoadWorkspace(ctx context.Context, tx *Transaction) (WorkspaceRecord, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return WorkspaceRecord{}, err
	}
	record := WorkspaceRecord{Identity: WorkspaceIdentity{Scope: scope}}
	if err := tx.queryRow(ctx, `
		SELECT name, created_at, tombstoned_at
		FROM pm.workspace_namespaces
		WHERE org_id = $1 AND workspace_id = $2
	`, scope.OrgID, scope.WorkspaceID).Scan(
		&record.Identity.Name,
		&record.CreatedAt,
		&record.TombstonedAt,
	); errors.Is(err, pgx.ErrNoRows) {
		return WorkspaceRecord{}, ErrNotFound
	} else if err != nil {
		return WorkspaceRecord{}, fmt.Errorf("load workspace namespace: %w", err)
	}
	record.CreatedAt = record.CreatedAt.UTC()
	if record.TombstonedAt != nil {
		value := record.TombstonedAt.UTC()
		record.TombstonedAt = &value
	}
	return record, nil
}

func RetireWorkspace(ctx context.Context, tx *Transaction, workspace WorkspaceIdentity) error {
	if err := tx.requireMatchingScope(workspace.Scope); err != nil {
		return err
	}
	if !validStoreID(workspace.Name) {
		return errors.New("workspace name is invalid")
	}
	var storedName string
	if err := tx.queryRow(ctx, `
		UPDATE pm.workspace_namespaces
		SET tombstoned_at = COALESCE(tombstoned_at, clock_timestamp())
		WHERE org_id = $1 AND workspace_id = $2 AND name = $3
		RETURNING name
	`, workspace.Scope.OrgID, workspace.Scope.WorkspaceID, workspace.Name).Scan(&storedName); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return safeMetadataWriteError("retire workspace namespace", err)
	}
	return nil
}

func requireActiveWorkspace(ctx context.Context, tx *Transaction) error {
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	var tombstonedAt *time.Time
	err = tx.queryRow(ctx, `
		SELECT tombstoned_at
		FROM pm.workspace_namespaces
		WHERE org_id = $1 AND workspace_id = $2
		FOR SHARE
	`, scope.OrgID, scope.WorkspaceID).Scan(&tombstonedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock active workspace namespace: %w", err)
	}
	if tombstonedAt != nil {
		return ErrWorkspaceRetired
	}
	return nil
}

type Identity struct {
	Principal contract.Principal
	Issuer    string
	Subject   string
	Revision  uint64
}

func (identity Identity) validate() error {
	if err := identity.Principal.Validate(); err != nil {
		return fmt.Errorf("validate principal identity: %w", err)
	}
	if identity.Revision == 0 {
		return errors.New("identity revision must be positive")
	}
	switch identity.Principal.Kind {
	case contract.PrincipalUser:
		if !validStoreID(identity.Issuer) || !validStoreID(identity.Subject) {
			return errors.New("user identity issuer or subject is invalid")
		}
	case contract.PrincipalServiceAccount:
		if identity.Issuer != "" || identity.Subject != "" {
			return errors.New("service account identity cannot carry an external issuer or subject")
		}
	default:
		return errors.New("identity kind is invalid")
	}
	return nil
}

func RegisterIdentity(ctx context.Context, tx *Transaction, identity Identity) error {
	scope, err := tx.requireScope()
	if err != nil {
		return err
	}
	if err := identity.validate(); err != nil {
		return err
	}
	if identity.Principal.OrgID != scope.OrgID {
		return ErrScopeMismatch
	}
	if err := requireActiveWorkspace(ctx, tx); err != nil {
		return err
	}
	tag, err := tx.exec(ctx, `
		INSERT INTO pm.identities (
			org_id, identity_id, kind, issuer, subject, revision
		) VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), $6)
		ON CONFLICT DO NOTHING
	`,
		identity.Principal.OrgID,
		identity.Principal.PrincipalID,
		string(identity.Principal.Kind),
		identity.Issuer,
		identity.Subject,
		identity.Revision,
	)
	if err != nil {
		return safeMetadataWriteError("register identity", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	stored, err := LoadIdentity(ctx, tx, identity.Principal.PrincipalID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrImmutableConflict
		}
		return err
	}
	if stored != identity {
		return ErrImmutableConflict
	}
	return nil
}

func LoadIdentity(ctx context.Context, tx *Transaction, identityID string) (Identity, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return Identity{}, err
	}
	if !validStoreID(identityID) {
		return Identity{}, errors.New("identity is invalid")
	}
	identity := Identity{Principal: contract.Principal{OrgID: scope.OrgID, PrincipalID: identityID}}
	if err := tx.queryRow(ctx, `
		SELECT kind, COALESCE(issuer, ''), COALESCE(subject, ''), revision
		FROM pm.identities
		WHERE org_id = $1 AND identity_id = $2
	`, scope.OrgID, identityID).Scan(
		&identity.Principal.Kind,
		&identity.Issuer,
		&identity.Subject,
		&identity.Revision,
	); errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, ErrNotFound
	} else if err != nil {
		return Identity{}, fmt.Errorf("load identity: %w", err)
	}
	if err := identity.validate(); err != nil {
		return Identity{}, fmt.Errorf("validate stored identity: %w", err)
	}
	return identity, nil
}

func safeMetadataWriteError(action string, err error) error {
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) {
		return fmt.Errorf("%s: %w", action, err)
	}
	switch databaseError.Code {
	case "23503":
		return fmt.Errorf("%s: %w", action, ErrNotFound)
	case "23505", "23514", "23502":
		return fmt.Errorf("%s: %w", action, ErrImmutableConflict)
	case "42501":
		return fmt.Errorf("%s: %w", action, ErrScopeMismatch)
	default:
		return fmt.Errorf("%s: %w", action, err)
	}
}
