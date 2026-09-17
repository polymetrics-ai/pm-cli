package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

type ScopedTransactionFunc func(context.Context, *Transaction) error

func WithinScope(
	ctx context.Context,
	pool *Pool,
	scope contract.Scope,
	actor contract.Actor,
	fn ScopedTransactionFunc,
) error {
	if err := scope.Validate(); err != nil {
		return fmt.Errorf("validate transaction scope: %w", err)
	}
	if err := actor.Validate(); err != nil {
		return fmt.Errorf("validate transaction actor: %w", err)
	}
	if actor.RequestedActor.OrgID != scope.OrgID || actor.EffectiveActor.OrgID != scope.OrgID {
		return ErrScopeMismatch
	}
	if fn == nil {
		return errors.New("scoped transaction callback is required")
	}
	return WithinTransaction(ctx, pool, func(ctx context.Context, tx *Transaction) error {
		if err := installScope(ctx, tx, scope, actor); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}

func installScope(ctx context.Context, tx *Transaction, scope contract.Scope, actor contract.Actor) error {
	if tx == nil || tx.tx == nil || tx.scoped {
		return errors.New("unscoped store transaction is required")
	}
	if _, err := tx.exec(ctx, `
		SELECT set_config('pm.org_id', $1, true),
		       set_config('pm.workspace_id', $2, true),
		       set_config('pm.requested_actor_id', $3, true),
		       set_config('pm.requested_actor_kind', $4, true),
		       set_config('pm.effective_actor_id', $5, true),
		       set_config('pm.effective_actor_kind', $6, true)
	`,
		scope.OrgID,
		scope.WorkspaceID,
		actor.RequestedActor.PrincipalID,
		string(actor.RequestedActor.Kind),
		actor.EffectiveActor.PrincipalID,
		string(actor.EffectiveActor.Kind),
	); err != nil {
		return fmt.Errorf("install transaction scope: %w", err)
	}
	tx.scope = scope
	tx.actor = actor
	tx.scoped = true
	return nil
}

func ClearScope(ctx context.Context, tx *Transaction) error {
	if tx == nil || tx.tx == nil {
		return errors.New("store transaction is required")
	}
	if _, err := tx.exec(ctx, `
		SELECT set_config('pm.org_id', '', true),
		       set_config('pm.workspace_id', '', true),
		       set_config('pm.requested_actor_id', '', true),
		       set_config('pm.requested_actor_kind', '', true),
		       set_config('pm.effective_actor_id', '', true),
		       set_config('pm.effective_actor_kind', '', true)
	`); err != nil {
		return fmt.Errorf("clear transaction scope: %w", err)
	}
	tx.scope = contract.Scope{}
	tx.actor = contract.Actor{}
	tx.scoped = false
	return nil
}
