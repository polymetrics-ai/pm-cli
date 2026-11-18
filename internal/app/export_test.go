package app

import (
	"context"

	"github.com/polymetrics-ai/pm-cli/internal/objects"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// SetResultPublicationCommit replaces how the settlement of service commits a
// prepared result publication. A developer check uses it to supply a receipt
// that a real store commit cannot produce. Production code cannot call it,
// because Go builds this file only for the tests of this package.
func SetResultPublicationCommit(
	service *WorkerService,
	commit func(context.Context, *store.Transaction, *objects.PreparedPublication) (store.ArtifactPublicationReceipt, error),
) {
	service.commitResultPublication = commit
}
