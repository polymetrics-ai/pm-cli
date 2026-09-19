package app

import (
	"context"
	"errors"
	"io"

	"github.com/polymetrics-ai/pm-cli/internal/auth"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// ArtifactBackend is the byte capability consumed by the application service.
// It owns neither authorization nor PostgreSQL artifact membership.
type ArtifactBackend interface {
	Verify(context.Context, contract.ObjectRef) error
	OpenVerified(context.Context, contract.ObjectRef, objects.ByteRange) (io.ReadCloser, error)
}

// ArtifactRangeKind identifies one supported single-range form. The zero value
// is intentionally invalid so omitted parser state cannot become a full read.
type ArtifactRangeKind uint8

const (
	ArtifactRangeInvalid ArtifactRangeKind = iota
	ArtifactRangeFull
	ArtifactRangeClosed
	ArtifactRangeFrom
	ArtifactRangeSuffix
)

// ArtifactRangeSelection is parsed by the transport without resolving it
// against private metadata. Only the fields selected by Kind may be nonzero.
type ArtifactRangeSelection struct {
	Kind         ArtifactRangeKind
	Start        int64
	End          int64
	SuffixLength int64
}

// ArtifactMetadata is the verified public projection of a registered object.
// Backend location, attempt key, and generation remain private.
type ArtifactMetadata struct {
	ArtifactID string
	SHA256     string
	ByteLength int64
	MediaType  string
}

// ArtifactContent owns one verified reader and its exact public interval. The
// caller must close Body; cancellation of the supplied context also owns it.
type ArtifactContent struct {
	Metadata ArtifactMetadata
	Offset   int64
	Length   int64
	Partial  bool
	Body     io.ReadCloser
}

// ArtifactRangeFailure carries only the registered total length needed for an
// authorized HTTP 416 response. It is created after backend availability is
// established and before any backend I/O.
type ArtifactRangeFailure struct {
	ByteLength int64
	failure    *Failure
}

func (failure *ArtifactRangeFailure) Error() string {
	if failure == nil || failure.failure == nil {
		return "artifact range is not satisfiable"
	}
	return failure.failure.Error()
}

func (failure *ArtifactRangeFailure) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.failure
}

// ArtifactService composes current scoped identity authority, durable artifact
// membership, and an optional verified byte backend.
type ArtifactService struct {
	identity *IdentityService
	backend  ArtifactBackend
}

// NewArtifactService binds one immutable optional byte capability to the
// existing identity service.
func NewArtifactService(identity *IdentityService, backend ArtifactBackend) (*ArtifactService, error) {
	if identity == nil || identity.pool == nil || identity.authorize == nil {
		return nil, errors.New("artifact service identity authority is required")
	}
	return &ArtifactService{identity: identity, backend: backend}, nil
}

// AuthorizeArtifactRead establishes current scoped artifact authority without
// claiming that an object-byte backend is installed or reachable.
func (service *IdentityService) AuthorizeArtifactRead(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	artifactID string,
) error {
	_, err := service.loadAuthorizedArtifact(ctx, actor, scope, artifactID)
	return err
}

// AuthorizeArtifactWrite establishes current workspace authority before an
// upload capability is consulted. Actual artifact bytes remain backend-owned.
func (service *IdentityService) AuthorizeArtifactWrite(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
) error {
	if service == nil || ctx == nil {
		return invalidRequestFailure()
	}
	return store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		_, err := service.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:           scope,
			Actor:           actor,
			Permission:      "artifact.write",
			ResourceKind:    auth.ResourceWorkspace,
			ResourceID:      scope.WorkspaceID,
			AutomationScope: actor.RequestedActor.Kind == contract.PrincipalServiceAccount,
		})
		return mapAuthorizationFailure(err)
	})
}

// Inspect verifies the complete registered object before exposing its public
// metadata. Authorization and explicit backend absence precede object I/O.
func (service *ArtifactService) Inspect(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	artifactID string,
) (ArtifactMetadata, error) {
	ref, err := service.authorizedReference(ctx, actor, scope, artifactID)
	if err != nil {
		return ArtifactMetadata{}, err
	}
	if service.backend == nil {
		return ArtifactMetadata{}, capabilityUnavailableFailure(objects.ErrObjectUnavailable)
	}
	if err := service.backend.Verify(ctx, ref); err != nil {
		return ArtifactMetadata{}, mapArtifactBackendFailure(err)
	}
	return artifactMetadata(ref), nil
}

// Open authorizes and loads the registered reference, refuses an absent
// backend, resolves one typed range, and only then asks the backend for a fully
// verified reader.
func (service *ArtifactService) Open(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	artifactID string,
	selection ArtifactRangeSelection,
) (ArtifactContent, error) {
	ref, err := service.authorizedReference(ctx, actor, scope, artifactID)
	if err != nil {
		return ArtifactContent{}, err
	}
	if service.backend == nil {
		return ArtifactContent{}, capabilityUnavailableFailure(objects.ErrObjectUnavailable)
	}
	resolved, partial, err := resolveArtifactRange(selection, ref.ByteLength)
	if err != nil {
		return ArtifactContent{}, &ArtifactRangeFailure{
			ByteLength: ref.ByteLength,
			failure: &Failure{
				HTTPStatus:  416,
				Code:        "artifact_range_not_satisfiable",
				RetryAction: "correct_input",
				cause:       objects.ErrInvalidRange,
			},
		}
	}
	body, err := service.backend.OpenVerified(ctx, ref, resolved)
	if err != nil {
		if body != nil {
			_ = body.Close()
		}
		return ArtifactContent{}, mapArtifactBackendFailure(err)
	}
	if body == nil {
		return ArtifactContent{}, errArtifactBackendFailure
	}
	return ArtifactContent{
		Metadata: artifactMetadata(ref),
		Offset:   resolved.Offset,
		Length:   resolved.Length,
		Partial:  partial,
		Body:     body,
	}, nil
}

func (service *ArtifactService) authorizedReference(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	artifactID string,
) (contract.ObjectRef, error) {
	if service == nil || service.identity == nil {
		return contract.ObjectRef{}, invalidRequestFailure()
	}
	return service.identity.loadAuthorizedArtifact(ctx, actor, scope, artifactID)
}

func (service *IdentityService) loadAuthorizedArtifact(
	ctx context.Context,
	actor contract.Actor,
	scope contract.Scope,
	artifactID string,
) (contract.ObjectRef, error) {
	if service == nil || ctx == nil || !validApplicationID(artifactID) {
		return contract.ObjectRef{}, invalidRequestFailure()
	}
	var ref contract.ObjectRef
	err := store.WithinScope(ctx, service.pool, scope, actor, func(ctx context.Context, tx *store.Transaction) error {
		if _, err := service.authorize(ctx, tx, auth.AuthorizationRequest{
			Scope:           scope,
			Actor:           actor,
			Permission:      "artifact.read",
			ResourceKind:    auth.ResourceArtifact,
			ResourceID:      artifactID,
			AutomationScope: actor.RequestedActor.Kind == contract.PrincipalServiceAccount,
		}); err != nil {
			return mapAuthorizationFailure(err)
		}
		loaded, err := store.LoadArtifact(ctx, tx, artifactID)
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrScopeMismatch) {
			return resourceNotFoundFailure(err)
		}
		if err != nil {
			return err
		}
		ref = loaded
		return nil
	})
	return ref, err
}

func resolveArtifactRange(selection ArtifactRangeSelection, byteLength int64) (objects.ByteRange, bool, error) {
	switch selection.Kind {
	case ArtifactRangeFull:
		if selection.Start != 0 || selection.End != 0 || selection.SuffixLength != 0 {
			break
		}
		return objects.ByteRange{Length: byteLength}, false, nil
	case ArtifactRangeClosed:
		if selection.Start < 0 || selection.End < selection.Start || selection.SuffixLength != 0 ||
			selection.Start >= byteLength {
			break
		}
		end := min(selection.End, byteLength-1)
		return objects.ByteRange{Offset: selection.Start, Length: end - selection.Start + 1}, true, nil
	case ArtifactRangeFrom:
		if selection.Start < 0 || selection.End != 0 || selection.SuffixLength != 0 ||
			selection.Start >= byteLength {
			break
		}
		return objects.ByteRange{Offset: selection.Start, Length: byteLength - selection.Start}, true, nil
	case ArtifactRangeSuffix:
		if selection.Start != 0 || selection.End != 0 || selection.SuffixLength <= 0 || byteLength == 0 {
			break
		}
		length := min(selection.SuffixLength, byteLength)
		return objects.ByteRange{Offset: byteLength - length, Length: length}, true, nil
	}
	return objects.ByteRange{}, false, objects.ErrInvalidRange
}

func artifactMetadata(ref contract.ObjectRef) ArtifactMetadata {
	return ArtifactMetadata{
		ArtifactID: ref.ArtifactID,
		SHA256:     ref.SHA256,
		ByteLength: ref.ByteLength,
		MediaType:  ref.MediaType,
	}
}

var errArtifactBackendFailure = errors.New("artifact backend failed to produce verified bytes")

func mapArtifactBackendFailure(err error) error {
	if errors.Is(err, objects.ErrObjectUnavailable) {
		return capabilityUnavailableFailure(objects.ErrObjectUnavailable)
	}
	return errArtifactBackendFailure
}
