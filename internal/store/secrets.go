package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const maxEncryptedSecretBytes = 1 << 20

type EncryptedSecret struct {
	Scope             contract.Scope
	Binding           contract.CredentialRef
	ProviderAccountID string
	Ciphertext        []byte
}

func (secret EncryptedSecret) validate() error {
	if err := secret.Scope.Validate(); err != nil {
		return fmt.Errorf("validate encrypted secret scope: %w", err)
	}
	if err := secret.Binding.Validate(); err != nil {
		return fmt.Errorf("validate encrypted secret binding: %w", err)
	}
	if secret.Binding.WorkspaceID != secret.Scope.WorkspaceID {
		return ErrScopeMismatch
	}
	if !validStoreID(secret.ProviderAccountID) {
		return errors.New("encrypted secret provider account is invalid")
	}
	if len(secret.Ciphertext) == 0 || len(secret.Ciphertext) > maxEncryptedSecretBytes {
		return errors.New("encrypted secret payload is outside the allowed bound")
	}
	return nil
}

func RegisterEncryptedSecret(ctx context.Context, tx *Transaction, secret EncryptedSecret) error {
	if err := tx.requireMatchingScope(secret.Scope); err != nil {
		return err
	}
	if err := secret.validate(); err != nil {
		return err
	}
	if err := requireActiveWorkspace(ctx, tx); err != nil {
		return err
	}
	connection, err := LoadConnection(ctx, tx, secret.Binding.ConnectionID)
	if err != nil {
		return err
	}
	if connection.ProviderAccountID != secret.ProviderAccountID || connection.CredentialRef != secret.Binding.CredentialRef {
		return ErrScopeMismatch
	}
	account, err := LoadProviderAccount(ctx, tx, secret.ProviderAccountID)
	if err != nil {
		return err
	}
	if account.AccountWitness != secret.Binding.ProviderAccountWitness {
		return ErrScopeMismatch
	}
	scopes, err := json.Marshal(secret.Binding.Scopes)
	if err != nil {
		return errors.New("encode encrypted secret scopes")
	}
	tag, err := tx.exec(ctx, `
		INSERT INTO pm.encrypted_secrets (
			org_id, workspace_id, credential_ref, ciphertext_version,
			connection_id, provider_account_id, account_witness, key_ref,
			scopes, auth_epoch, ciphertext
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT DO NOTHING
	`,
		secret.Scope.OrgID,
		secret.Scope.WorkspaceID,
		secret.Binding.CredentialRef,
		secret.Binding.CiphertextVersion,
		secret.Binding.ConnectionID,
		secret.ProviderAccountID,
		secret.Binding.ProviderAccountWitness,
		secret.Binding.KeyRef,
		json.RawMessage(scopes),
		secret.Binding.AuthEpoch,
		secret.Ciphertext,
	)
	if err != nil {
		return safeMetadataWriteError("register encrypted secret metadata", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	stored, err := LoadEncryptedSecret(ctx, tx, secret.Binding.CredentialRef, secret.Binding.CiphertextVersion)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrImmutableConflict
		}
		return err
	}
	if !equalEncryptedSecret(stored, secret) {
		return ErrImmutableConflict
	}
	return nil
}

func LoadEncryptedSecret(ctx context.Context, tx *Transaction, credentialRef string, ciphertextVersion uint64) (EncryptedSecret, error) {
	scope, err := tx.requireScope()
	if err != nil {
		return EncryptedSecret{}, err
	}
	if !validStoreID(credentialRef) || ciphertextVersion == 0 {
		return EncryptedSecret{}, errors.New("encrypted secret identity or version is invalid")
	}
	secret := EncryptedSecret{Scope: scope}
	secret.Binding.CredentialRef = credentialRef
	secret.Binding.WorkspaceID = scope.WorkspaceID
	secret.Binding.CiphertextVersion = ciphertextVersion
	var encodedScopes []byte
	if err := tx.queryRow(ctx, `
		SELECT connection_id, provider_account_id, account_witness, key_ref,
		       scopes, auth_epoch, ciphertext
		FROM pm.encrypted_secrets
		WHERE credential_ref = $1 AND ciphertext_version = $2
	`, credentialRef, ciphertextVersion).Scan(
		&secret.Binding.ConnectionID,
		&secret.ProviderAccountID,
		&secret.Binding.ProviderAccountWitness,
		&secret.Binding.KeyRef,
		&encodedScopes,
		&secret.Binding.AuthEpoch,
		&secret.Ciphertext,
	); errors.Is(err, pgx.ErrNoRows) {
		return EncryptedSecret{}, ErrNotFound
	} else if err != nil {
		return EncryptedSecret{}, fmt.Errorf("load encrypted secret metadata: %w", err)
	}
	if err := json.Unmarshal(encodedScopes, &secret.Binding.Scopes); err != nil {
		return EncryptedSecret{}, errors.New("decode encrypted secret scopes")
	}
	secret.Ciphertext = bytes.Clone(secret.Ciphertext)
	if err := secret.validate(); err != nil {
		return EncryptedSecret{}, fmt.Errorf("validate stored encrypted secret metadata: %w", err)
	}
	return secret, nil
}

func equalEncryptedSecret(left, right EncryptedSecret) bool {
	return left.Scope.Equal(right.Scope) &&
		left.ProviderAccountID == right.ProviderAccountID &&
		left.Binding.CredentialRef == right.Binding.CredentialRef &&
		left.Binding.WorkspaceID == right.Binding.WorkspaceID &&
		left.Binding.ConnectionID == right.Binding.ConnectionID &&
		left.Binding.ProviderAccountWitness == right.Binding.ProviderAccountWitness &&
		left.Binding.CiphertextVersion == right.Binding.CiphertextVersion &&
		left.Binding.KeyRef == right.Binding.KeyRef &&
		left.Binding.AuthEpoch == right.Binding.AuthEpoch &&
		slices.Equal(left.Binding.Scopes, right.Binding.Scopes) &&
		bytes.Equal(left.Ciphertext, right.Ciphertext)
}
