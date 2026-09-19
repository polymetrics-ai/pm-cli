package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/polymetrics-ai/pm-cli/internal/connectors"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// connectorBundleByteLimit is the explicit bound for one declared generated
// execution bundle. It equals the strict contract document bound applied while
// decoding, so an oversized declaration is refused before any decode attempt
// and no declared bundle is ever read without a bound.
const connectorBundleByteLimit int64 = contract.MaxContractDocumentBytes

// ErrConnectorUnavailable reports that no bundled operation can be resolved for
// the requested connection in this process. Admission and worker composition
// map it to capability-unavailable, and resolution never substitutes another
// connector identifier.
var ErrConnectorUnavailable = errors.New("bundled connector operation is unavailable")

// ConnectorCompositionConfig declares the generated execution bundles one
// process admits. An absent or empty declaration composes no connector.
type ConnectorCompositionConfig struct {
	BundlePaths []string
}

// ConnectorComposition owns the immutable connector registry loaded from the
// declared generated execution bundles and the single operation resolver shared
// by admission, catalog and worker composition. It reads no source document,
// proof receipt or Atlas projection, and it never executes a provider call.
type ConnectorComposition struct {
	registry *connectors.Registry
	declared bool
}

// OpenConnectorComposition loads every declared bundle exactly once and derives
// the immutable registry revision from the loaded bundles. A missing,
// unreadable, directory, symlinked, oversized, undecodable or duplicated
// declaration fails composition with a bounded error that never contains
// bundle bytes; it never degrades to a smaller catalog.
func OpenConnectorComposition(configuration ConnectorCompositionConfig) (*ConnectorComposition, error) {
	bundles, err := loadConnectorBundles(configuration.BundlePaths)
	if err != nil {
		return nil, err
	}
	registry, err := connectors.NewRegistry(bundles...)
	if err != nil {
		return nil, fmt.Errorf("compose connector bundle registry: %w", err)
	}
	return &ConnectorComposition{registry: registry, declared: len(bundles) > 0}, nil
}

// Configured reports whether at least one generated execution bundle is
// installed. An unconfigured process claims no bundled capability.
func (composition *ConnectorComposition) Configured() bool {
	return composition != nil && composition.declared
}

// Registry returns the immutable bundle registry catalog projection reads. It
// is never nil for a composed instance, so an undeclared deployment projects an
// explicit empty catalog instead of failing composition.
func (composition *ConnectorComposition) Registry() *connectors.Registry {
	if composition == nil {
		return nil
	}
	return composition.registry
}

// RegisterDirectRead installs the shared HTTP direct-read callback for the
// declared connector bundles. An undeclared composition registers nothing, so
// admission can never advertise a bundled lane whose callback is not really
// registered, and an explicit injected registry is never silently duplicated.
func (composition *ConnectorComposition) RegisterDirectRead(
	handlers *HandlerRegistry,
	dependencies HTTPDirectReadDependencies,
) error {
	if composition == nil {
		return errors.New("connector composition is required")
	}
	if handlers == nil {
		return errors.New("connector handler registry is required")
	}
	if !composition.declared {
		return nil
	}
	if err := dependencies.ClientIdentity.Validate(); err != nil {
		return fmt.Errorf("shared HTTP direct-read requires a provider client identity: %w", err)
	}
	if err := handlers.RegisterHTTP(NewHTTPDirectReadRegistration(dependencies)); err != nil {
		return fmt.Errorf("register shared HTTP direct-read handler: %w", err)
	}
	return nil
}

// Resolver returns the single operation resolver shared by admission, catalog
// and worker composition. Without a declared bundle it refuses every operation
// instead of fabricating a connector catalog.
func (composition *ConnectorComposition) Resolver() OperationResolver {
	if !composition.Configured() {
		return unavailableConnectorOperation
	}
	return composition.ResolveOperation
}

// ResolveOperation selects one bundled operation for the connector bound to an
// existing connection. The connection is read only through the caller's
// already-authorized scope transaction: an absent scope transaction, a scope
// that differs from the transaction scope, an unknown connection or a
// connector that is not installed all stay explicitly unavailable.
func (composition *ConnectorComposition) ResolveOperation(
	ctx context.Context,
	scope contract.Scope,
	connectionID string,
	operationID string,
) (OperationSelection, error) {
	if !composition.Configured() || ctx == nil {
		return OperationSelection{}, ErrConnectorUnavailable
	}
	if err := ctx.Err(); err != nil {
		return OperationSelection{}, err
	}
	if err := scope.Validate(); err != nil {
		return OperationSelection{}, fmt.Errorf("%w: %w", ErrConnectorUnavailable, err)
	}
	tx, present := store.ScopedTransaction(ctx)
	if !present || !tx.Scope().Equal(scope) {
		return OperationSelection{}, ErrConnectorUnavailable
	}
	connection, err := store.LoadConnection(ctx, tx, connectionID)
	if err != nil {
		return OperationSelection{}, fmt.Errorf("%w: %w", ErrConnectorUnavailable, err)
	}
	selection, err := ResolveBundledOperation(composition.registry, connection.ConnectorID, operationID)
	if err != nil {
		return OperationSelection{}, fmt.Errorf("%w: %w", ErrConnectorUnavailable, err)
	}
	return selection, nil
}

func unavailableConnectorOperation(
	context.Context,
	contract.Scope,
	string,
	string,
) (OperationSelection, error) {
	return OperationSelection{}, ErrConnectorUnavailable
}

// loadConnectorBundles reads and decodes every declared bundle in declaration
// order. Connector order inside the registry is canonical, so the same declared
// set always derives the same catalog revision.
func loadConnectorBundles(paths []string) ([]connectors.Bundle, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	bundles := make([]connectors.Bundle, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for index, path := range paths {
		document, err := readConnectorBundleFile(path)
		if err != nil {
			return nil, fmt.Errorf("declared connector bundle %d is unavailable: %w", index, err)
		}
		bundle, err := connectors.LoadBundle(bytes.NewReader(document))
		if err != nil {
			return nil, fmt.Errorf("declared connector bundle %d is invalid: %w", index, err)
		}
		if _, duplicate := seen[bundle.ConnectorID]; duplicate {
			return nil, fmt.Errorf("declared connector bundle %d duplicates connector %q", index, bundle.ConnectorID)
		}
		seen[bundle.ConnectorID] = struct{}{}
		bundles = append(bundles, bundle)
	}
	return bundles, nil
}

// readConnectorBundleFile reads one declared bundle as a bounded regular file.
// It never follows a final-component symlink, never opens a directory or FIFO
// as a bundle, and never returns more than connectorBundleByteLimit bytes.
func readConnectorBundleFile(path string) ([]byte, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("declared bundle path must be an absolute canonical path")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("declared bundle file cannot be opened")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, errors.New("declared bundle file cannot be inspected")
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("declared bundle must be a regular file")
	}
	if info.Size() > connectorBundleByteLimit {
		return nil, errors.New("declared bundle exceeds the bundle byte limit")
	}
	document, err := io.ReadAll(io.LimitReader(file, connectorBundleByteLimit+1))
	if err != nil {
		return nil, errors.New("declared bundle file cannot be read")
	}
	if int64(len(document)) > connectorBundleByteLimit {
		return nil, errors.New("declared bundle exceeds the bundle byte limit")
	}
	return document, nil
}
