package compile

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/getkin/kin-openapi/openapi2"
	"github.com/getkin/kin-openapi/openapi2conv"
	"github.com/getkin/kin-openapi/openapi3"
)

type ConversionProvenance struct {
	SourceID          string  `json:"source_id"`
	SourceDialect     Dialect `json:"source_dialect"`
	SourceVersion     string  `json:"source_version"`
	OriginalPointer   string  `json:"original_pointer"`
	NormalizedVersion string  `json:"normalized_version"`
	Converted         bool    `json:"converted"`
}

type LoadedDocument struct {
	SourceID   string
	Document   *openapi3.T
	Provenance ConversionProvenance
}

type LoadedSource struct {
	Resolved  *ResolvedSource
	Documents []LoadedDocument
	byID      map[string]*LoadedDocument
}

func LoadDialects(ctx context.Context, source *ResolvedSource) (*LoadedSource, error) {
	if source == nil || source.Captured == nil {
		return nil, sourceError(ErrInvalidSource, "", "", "resolved source is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	loaded := &LoadedSource{
		Resolved: source,
		byID:     make(map[string]*LoadedDocument),
	}
	for _, resolvedDocument := range source.Documents {
		if !resolvedDocument.Root {
			continue
		}
		loader := closedLoader(ctx, source)
		location := captureURI(resolvedDocument.Metadata.SourceID)
		var document *openapi3.T
		provenance := ConversionProvenance{
			SourceID:        resolvedDocument.Metadata.SourceID,
			SourceDialect:   source.Captured.Lock.SourceSet.Dialect,
			SourceVersion:   sourceRootDialect(source.byID[resolvedDocument.Metadata.SourceID].root),
			OriginalPointer: "#",
		}
		switch source.Captured.Lock.SourceSet.Dialect {
		case DialectOpenAPI30:
			if !supportedOpenAPI30(provenance.SourceVersion) {
				return nil, sourceError(ErrUnsupportedDialect, provenance.SourceID, "/openapi", "source version is not an admitted OpenAPI 3.0 revision")
			}
			var err error
			document, err = loader.LoadFromDataWithPath(resolvedDocument.RewrittenJSON, location)
			if err != nil {
				return nil, sourceError(ErrInvalidSource, provenance.SourceID, "", fmt.Sprintf("OpenAPI 3.0 load failed: %v", err))
			}
			provenance.NormalizedVersion = document.OpenAPI
		case DialectOpenAPI31:
			if !supportedOpenAPI31(provenance.SourceVersion) {
				return nil, sourceError(ErrUnsupportedDialect, provenance.SourceID, "/openapi", "source version is not an admitted OpenAPI 3.1 revision")
			}
			var err error
			document, err = loader.LoadFromDataWithPath(resolvedDocument.RewrittenJSON, location)
			if err != nil {
				return nil, sourceError(ErrInvalidSource, provenance.SourceID, "", fmt.Sprintf("OpenAPI 3.1 load failed: %v", err))
			}
			provenance.NormalizedVersion = document.OpenAPI
		case DialectSwagger20:
			if provenance.SourceVersion != "2.0" {
				return nil, sourceError(ErrUnsupportedDialect, provenance.SourceID, "/swagger", "source version must be exactly Swagger 2.0")
			}
			var swagger openapi2.T
			if err := json.Unmarshal(resolvedDocument.RewrittenJSON, &swagger); err != nil {
				return nil, sourceError(ErrInvalidSource, provenance.SourceID, "", fmt.Sprintf("Swagger 2.0 load failed: %v", err))
			}
			var err error
			document, err = openapi2conv.ToV3WithLoader(&swagger, loader, location)
			if err != nil {
				return nil, sourceError(ErrUnsupportedShape, provenance.SourceID, "", fmt.Sprintf("Swagger 2.0 conversion failed: %v", err))
			}
			provenance.Converted = true
			provenance.NormalizedVersion = document.OpenAPI
		default:
			return nil, sourceError(ErrUnsupportedDialect, provenance.SourceID, "", "source dialect is unsupported")
		}
		if document == nil {
			return nil, sourceError(ErrInvalidSource, provenance.SourceID, "", "dialect loader returned no document")
		}
		if err := document.Validate(ctx); err != nil {
			return nil, sourceError(ErrInvalidSource, provenance.SourceID, "", fmt.Sprintf("source document validation failed: %v", err))
		}
		loaded.Documents = append(loaded.Documents, LoadedDocument{
			SourceID:   provenance.SourceID,
			Document:   document,
			Provenance: provenance,
		})
		loaded.byID[provenance.SourceID] = &loaded.Documents[len(loaded.Documents)-1]
	}
	if len(loaded.Documents) == 0 {
		return nil, sourceError(ErrInvalidSource, "", "", "captured closure contains no OpenAPI root document")
	}
	return loaded, nil
}

func closedLoader(ctx context.Context, source *ResolvedSource) *openapi3.Loader {
	loader := openapi3.NewLoader()
	loader.Context = ctx
	loader.IncludeOrigin = true
	loader.IsExternalRefsAllowed = true
	loader.ReadFromURIFunc = func(_ *openapi3.Loader, location *url.URL) ([]byte, error) {
		if location == nil || location.Scheme != "pmcapture" || location.Host != "source" || location.User != nil {
			return nil, fmt.Errorf("%w: URI is outside the captured source closure", openapi3.ErrURINotSupported)
		}
		sourceID := strings.TrimPrefix(location.Path, "/")
		if sourceID == "" {
			return nil, fmt.Errorf("%w: malformed captured source identity", openapi3.ErrURINotSupported)
		}
		document, ok := source.Document(sourceID)
		if !ok {
			return nil, fmt.Errorf("%w: captured source identity is unknown", openapi3.ErrURINotSupported)
		}
		return document.RewrittenJSON, nil
	}
	return loader
}

func captureURI(sourceID string) *url.URL {
	return &url.URL{Scheme: "pmcapture", Host: "source", Path: "/" + sourceID}
}

func supportedOpenAPI30(version string) bool {
	switch version {
	case "3.0.0", "3.0.1", "3.0.2", "3.0.3":
		return true
	default:
		return false
	}
}

func supportedOpenAPI31(version string) bool {
	return version == "3.1.0" || version == "3.1.1"
}
