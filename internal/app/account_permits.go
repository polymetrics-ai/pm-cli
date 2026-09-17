package app

import (
	"context"
	"errors"

	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// AcquirePermit reserves one unit against the connection's canonical provider
// account and performs the database-time expiry/fence check required before a
// new physical attempt is committed. An expired reservation is never reclaimed
// and authorized in the same acquisition: durable retry eligibility must be
// established separately before a later unit may send.
func AcquirePermit(
	ctx context.Context,
	tx *store.Transaction,
	request store.AccountPermitRequest,
	expectedProviderAccountID string,
) (store.AccountPermit, error) {
	if ctx == nil || tx == nil || !validApplicationID(expectedProviderAccountID) {
		return store.AccountPermit{}, invalidRequestFailure()
	}

	permit, err := store.LoadAccountPermit(ctx, tx, request.PermitID)
	if errors.Is(err, store.ErrNotFound) {
		permit, err = store.ReserveAccountPermit(ctx, tx, request)
	}
	if err != nil {
		return store.AccountPermit{}, err
	}
	if permit.ConnectionID != request.ConnectionID ||
		permit.ProviderAccountID != expectedProviderAccountID ||
		permit.OwnerID != request.OwnerID || permit.Units != request.Units {
		return store.AccountPermit{}, workerFailure(
			ErrWorkerConflict,
			errors.New("worker account permit identity conflicts"),
		)
	}

	switch permit.State {
	case store.AccountPermitReserved:
		return store.AuthorizeAccountPermit(ctx, tx, permit)
	case store.AccountPermitDispatched, store.AccountPermitUnknown:
		return store.AccountPermit{}, workerFailure(
			ErrWorkerConflict,
			errors.New("new dispatch has a previously used account permit"),
		)
	case store.AccountPermitResolved:
		return store.AccountPermit{}, workerFailure(
			ErrWorkerConflict,
			errors.New("resolved account permit lacks durable settlement"),
		)
	case store.AccountPermitExpired:
		return store.AccountPermit{}, workerFailure(
			ErrWorkerConflict,
			errors.New("expired account permit cannot authorize the sealed dispatch"),
		)
	default:
		return store.AccountPermit{}, workerFailure(
			ErrWorkerConflict,
			errors.New("account permit state is unsupported"),
		)
	}
}
