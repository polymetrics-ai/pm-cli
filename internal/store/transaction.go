package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

var (
	ErrNotFound             = errors.New("store record not found")
	ErrScopeRequired        = errors.New("transaction scope required")
	ErrScopeMismatch        = errors.New("transaction scope mismatch")
	ErrCASConflict          = errors.New("compare-and-swap conflict")
	ErrImmutableConflict    = errors.New("immutable store record conflict")
	ErrIdempotencyConflict  = errors.New("idempotency conflict")
	ErrClaimLost            = errors.New("task claim lost")
	ErrStaleFence           = errors.New("stale task fence")
	ErrObjectNotVerified    = errors.New("object reference was not preverified")
	ErrUnsupportedDowngrade = errors.New("store schema downgrade is unsupported")
)

const cleanupTimeout = 5 * time.Second

type Transaction struct {
	tx     pgx.Tx
	scope  contract.Scope
	actor  contract.Actor
	scoped bool
}

type TransactionFunc func(context.Context, *Transaction) error

func WithinTransaction(ctx context.Context, pool *Pool, fn TransactionFunc) (err error) {
	if ctx == nil {
		return errors.New("transaction context is required")
	}
	if pool == nil || pool.inner == nil {
		return errors.New("store pool is required")
	}
	if fn == nil {
		return errors.New("transaction callback is required")
	}
	connection, err := pool.inner.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire store connection: %w", err)
	}
	destroy := false
	defer func() {
		if destroy {
			raw := connection.Hijack()
			closeCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
			defer cancel()
			_ = raw.Close(closeCtx)
			return
		}
		connection.Release()
	}()

	pgxTx, err := connection.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite})
	if err != nil {
		return fmt.Errorf("begin store transaction: %w", err)
	}
	tx := &Transaction{tx: pgxTx}
	defer func() {
		if recovered := recover(); recovered != nil {
			cleanupCtx, cancel := detachedCleanupContext()
			defer cancel()
			if tx.scoped {
				if clearErr := ClearScope(cleanupCtx, tx); clearErr != nil {
					destroy = true
				}
			}
			if rollbackErr := rollbackStoreTransaction(cleanupCtx, pool, pgxTx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
				destroy = true
			}
			panic(recovered)
		}
	}()

	callbackErr := fn(ctx, tx)
	cleanupCtx, cancelCleanup := detachedCleanupContext()
	defer cancelCleanup()
	var clearErr error
	if tx.scoped {
		clearErr = ClearScope(cleanupCtx, tx)
		if clearErr != nil {
			destroy = true
		}
	}
	contextErr := ctx.Err()
	if callbackErr != nil || clearErr != nil || contextErr != nil {
		rollbackErr := rollbackStoreTransaction(cleanupCtx, pool, pgxTx)
		if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			destroy = true
		}
		return joinStoreErrors(callbackErr, clearErr, contextErr, normalizeClosedRollback(rollbackErr))
	}
	if commitErr := pgxTx.Commit(ctx); commitErr != nil {
		destroy = true
		rollbackErr := rollbackStoreTransaction(cleanupCtx, pool, pgxTx)
		return joinStoreErrors(
			fmt.Errorf("commit store transaction: %w", commitErr),
			normalizeClosedRollback(rollbackErr),
		)
	}
	return nil
}

func detachedCleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), cleanupTimeout)
}

func rollbackStoreTransaction(ctx context.Context, pool *Pool, tx pgx.Tx) error {
	if pool.rollbackCleanup != nil {
		return pool.rollbackCleanup(ctx, tx)
	}
	return tx.Rollback(ctx)
}

func normalizeClosedRollback(err error) error {
	if errors.Is(err, pgx.ErrTxClosed) {
		return nil
	}
	return err
}

func joinStoreErrors(values ...error) error {
	filtered := make([]error, 0, len(values))
	for _, value := range values {
		if value != nil {
			filtered = append(filtered, value)
		}
	}
	return errors.Join(filtered...)
}

func (tx *Transaction) exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	if tx == nil || tx.tx == nil {
		return pgconn.CommandTag{}, errors.New("store transaction is required")
	}
	return tx.tx.Exec(ctx, sql, arguments...)
}

func (tx *Transaction) query(ctx context.Context, sql string, arguments ...any) (pgx.Rows, error) {
	if tx == nil || tx.tx == nil {
		return nil, errors.New("store transaction is required")
	}
	return tx.tx.Query(ctx, sql, arguments...)
}

func (tx *Transaction) queryRow(ctx context.Context, sql string, arguments ...any) pgx.Row {
	if tx == nil || tx.tx == nil {
		return errorRow{err: errors.New("store transaction is required")}
	}
	return tx.tx.QueryRow(ctx, sql, arguments...)
}

func (tx *Transaction) requireScope() (contract.Scope, error) {
	if tx == nil || !tx.scoped {
		return contract.Scope{}, ErrScopeRequired
	}
	return tx.scope, nil
}

func (tx *Transaction) requireMatchingScope(scope contract.Scope) error {
	current, err := tx.requireScope()
	if err != nil {
		return err
	}
	if !current.Equal(scope) {
		return ErrScopeMismatch
	}
	return nil
}

type errorRow struct {
	err error
}

func (r errorRow) Scan(...any) error {
	return r.err
}

func validStoreID(value string) bool {
	if value == "" || len(value) > 512 || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validStoreName(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for index, character := range value {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' || character == '-' || character == '.' && index > 0 {
			continue
		}
		return false
	}
	return true
}
