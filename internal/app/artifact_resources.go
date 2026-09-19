package app

import (
	"context"
	"crypto/x509"
	"errors"
	"os"
	"sync"

	"github.com/polymetrics-ai/pm-cli/internal/config"
	"github.com/polymetrics-ai/pm-cli/internal/credentials"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

// ArtifactResources owns one process-local adapter and verification scratch.
// Shared objects remain remote; the scratch directory is never authoritative.
// API and worker processes construct independent instances from the same typed
// operator policy and approved secret reference.
type ArtifactResources struct {
	Backend  *objects.S3
	Limits   store.ObjectCapacityLimits
	scratch  string
	identity os.FileInfo
	once     sync.Once
	closeErr error
}

// OpenArtifactResources returns nil without I/O when byte execution is absent.
// It never discovers credentials, endpoint, region, trust, limits or local state
// from ambient configuration. The caller owns the supplied operator secret root.
func OpenArtifactResources(configuration config.ArtifactsConfiguration, secrets *credentials.OperatorSecrets) (*ArtifactResources, error) {
	execution := configuration.Execution
	if !execution.Configured() {
		return nil, nil
	}
	if secrets == nil {
		return nil, errors.New("artifact operator authority is unavailable")
	}
	ca, err := secrets.Read(configuration.CredentialRef, "ca.pem")
	if err != nil {
		return nil, errors.New("artifact trust root is unavailable")
	}
	roots := x509.NewCertPool()
	validCA := roots.AppendCertsFromPEM(ca)
	clear(ca)
	if !validCA {
		return nil, errors.New("artifact trust root is invalid")
	}
	scratch, err := os.MkdirTemp("", "pm-artifact-verification-")
	if err != nil {
		return nil, errors.New("artifact verification scratch is unavailable")
	}
	identity, err := os.Lstat(scratch)
	if err != nil {
		_ = os.Remove(scratch)
		return nil, errors.New("artifact verification scratch identity is unavailable")
	}
	backend, err := objects.NewS3(objects.S3Configuration{
		Endpoint:         configuration.Endpoint,
		Bucket:           configuration.Bucket,
		Region:           execution.Region,
		RootCAs:          roots,
		ScratchDirectory: scratch,
		Bounds: objects.Bounds{
			MaxObjectBytes: int64(execution.MaxObjectBytes),
			MaxInFlight:    int(execution.MaxInFlight),
			RequestTimeout: execution.RequestTimeout,
		},
		LoadCredentials: func(ctx context.Context) (objects.Credentials, error) {
			if ctx == nil || ctx.Err() != nil {
				return objects.Credentials{}, objects.ErrObjectUnavailable
			}
			access, err := secrets.Text(configuration.CredentialRef, "access-key")
			if err != nil {
				return objects.Credentials{}, objects.ErrObjectUnavailable
			}
			secret, err := secrets.Text(configuration.CredentialRef, "secret-key")
			if err != nil {
				return objects.Credentials{}, objects.ErrObjectUnavailable
			}
			return objects.Credentials{AccessKeyID: access, SecretAccessKey: secret}, nil
		},
	})
	if err != nil {
		_ = removeArtifactScratch(scratch, identity)
		return nil, errors.New("artifact byte backend configuration is invalid")
	}
	return &ArtifactResources{
		Backend: backend,
		Limits: store.ObjectCapacityLimits{
			OrganizationBytes:    execution.OrganizationReservedBytes,
			WorkspaceBytes:       execution.WorkspaceReservedBytes,
			OrganizationAttempts: execution.OrganizationReservedAttempts,
			WorkspaceAttempts:    execution.WorkspaceReservedAttempts,
		},
		scratch:  scratch,
		identity: identity,
	}, nil
}

// Close follows cancellation and closure of the owner's active byte operations.
// It removes only the same empty private directory this resource created, never
// recursively deletes a pathname that may have been replaced.
func (resources *ArtifactResources) Close() error {
	if resources == nil {
		return nil
	}
	resources.once.Do(func() {
		resources.closeErr = errors.Join(resources.Backend.Close(), removeArtifactScratch(resources.scratch, resources.identity))
	})
	return resources.closeErr
}

func removeArtifactScratch(path string, identity os.FileInfo) error {
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || identity == nil || !current.IsDir() || !os.SameFile(identity, current) {
		return errors.New("artifact scratch cleanup identity mismatch")
	}
	if err := os.Remove(path); err != nil {
		return errors.New("artifact scratch cleanup failed")
	}
	return nil
}
