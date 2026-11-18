package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/app"
	"github.com/polymetrics-ai/pm-cli/internal/auth"
)

// ServiceConfig binds the generated transport to the existing application
// authorities. It contains no provider adapter or client-controlled key locator.
// NewService takes an immutable copy; callers must not replace its dependencies.
type ServiceConfig struct {
	PublicOrigin string
	Auth         *auth.Service
	Identity     *app.IdentityService
	Credentials  *app.CredentialIngress
	Catalog      *app.CatalogService
	Admission    *app.AdmissionService
	Runs         *app.RunService
	// Artifacts is optional; absence preserves explicit read unavailability.
	Artifacts app.ArtifactBackend
	// Warehouse is optional; absence preserves explicit warehouse read
	// unavailability.
	Warehouse *WarehouseResources
	// SchemaSelection is optional. Its absence makes the schema selection route
	// answer capability_unavailable. The schema route reads the selection from
	// PM PostgreSQL through Identity and needs no other dependency.
	SchemaSelection       *app.SchemaSelectionService
	InstanceID            string
	OIDCHTTPClient        *http.Client
	ResolveEventCursorKey func(context.Context) ([]byte, error)
	EventBufferSize       uint64
	EventWriteTimeout     time.Duration
	Draining              <-chan struct{}
}
