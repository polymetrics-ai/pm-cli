package compile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const (
	SourceLockSchemaVersion = "pm.connector-source/v1"
	ReportSchemaVersion     = "pm.compile-report/v1"
	CommandsSchemaVersion   = "pm.commands/v1"
	MaxSourceDocumentBytes  = contract.MaxContractDocumentBytes
)

var (
	ErrInvalidSource      = errors.New("invalid connector source")
	ErrUnsupportedDialect = errors.New("unsupported source dialect")
	ErrUnsafeReference    = errors.New("unsafe source reference")
	ErrUnsupportedShape   = errors.New("unsupported source shape")
	ErrSourceFidelity     = errors.New("source fidelity disagreement")
	ErrUnsafeAmendment    = errors.New("unsafe semantic amendment")
)

type Error struct {
	Kind     error
	SourceID string
	Pointer  string
	Detail   string
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	location := e.SourceID
	if e.Pointer != "" {
		location += e.Pointer
	}
	if location == "" {
		return fmt.Sprintf("%v: %s", e.Kind, e.Detail)
	}
	return fmt.Sprintf("%v at %s: %s", e.Kind, location, e.Detail)
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Kind
}

func sourceError(kind error, sourceID, pointer, detail string) error {
	return &Error{Kind: kind, SourceID: sourceID, Pointer: pointer, Detail: detail}
}

type Dialect string

const (
	DialectOpenAPI30 Dialect = "openapi-3.0"
	DialectOpenAPI31 Dialect = "openapi-3.1"
	DialectSwagger20 Dialect = "swagger-2.0"
	// DialectPostgreSQLDocs16 describes connector behavior through the pinned
	// official PostgreSQL 16 protocol, SQL and metadata documentation. The
	// identity names the documented family and its selected supported version,
	// exactly as openapi-3.0 names a document format and its version; the
	// connector branch itself stays in the connector root.
	DialectPostgreSQLDocs16 Dialect = "postgresql-docs-16"
)

type SourceLock struct {
	SchemaVersion     string            `json:"schema_version"`
	ConnectorID       string            `json:"connector_id"`
	SourceSet         SourceSet         `json:"source_set"`
	Operations        []SourceOperation `json:"operations"`
	SemanticsRevision string            `json:"semantics_revision"`
}

type SourceSet struct {
	Dialect          Dialect          `json:"dialect"`
	Documents        []SourceDocument `json:"documents"`
	ReferencesClosed bool             `json:"references_closed"`
}

type SourceDocument struct {
	SourceID    string `json:"source_id"`
	OfficialURL string `json:"official_url"`
	Repository  string `json:"repository,omitempty"`
	Revision    string `json:"revision,omitempty"`
	Path        string `json:"path"`
	CapturedAt  string `json:"captured_at"`
	Pointer     string `json:"pointer,omitempty"`
	// GitBlob, UpstreamByteLength and UpstreamSHA256 are one explicit triple
	// recording the accepted upstream document's own identity. ByteLength and
	// SHA256 below are a different fact: the compiler-verified identity of the
	// committed capture the compiler lowers, which is never a restatement of
	// the upstream file when only a reference closure was captured.
	GitBlob            string            `json:"git_blob,omitempty"`
	UpstreamByteLength *int64            `json:"upstream_byte_length,omitempty"`
	UpstreamSHA256     string            `json:"upstream_sha256,omitempty"`
	ByteLength         int64             `json:"byte_length"`
	SHA256             string            `json:"sha256"`
	License            SourceLicense     `json:"license"`
	References         []SourceReference `json:"references,omitempty"`
}

type SourceLicense struct {
	SPDX           string `json:"spdx"`
	Path           string `json:"path"`
	SHA256         string `json:"sha256"`
	Copyright      string `json:"copyright,omitempty"`
	NoticeRequired bool   `json:"notice_required"`
}

type SourceReference struct {
	FromPointer    string `json:"from_pointer"`
	Reference      string `json:"reference"`
	TargetSourceID string `json:"target_source_id"`
	TargetPointer  string `json:"target_pointer"`
}

// SourceOperation is one declared source operation. An interface-description
// dialect declares request and response variants; a documentation dialect
// declares the typed PostgreSQL statement facts instead. StatementKind and
// RelationBinding are pointers so an absent fact stays distinct from an
// explicitly empty one.
type SourceOperation struct {
	SourceOperationID string       `json:"source_operation_id"`
	SourceRefs        []SourceRef  `json:"source_refs"`
	RequestVariants   []VariantRef `json:"request_variants"`
	ResponseVariants  []VariantRef `json:"response_variants"`
	StatementKind     *contract.PostgreSQLStatementKind `json:"statement_kind,omitempty"`
	RelationBinding   *string                           `json:"relation_binding,omitempty"`
}

type SourceRef = contract.SourceRef

type VariantRef struct {
	Pointer   string `json:"pointer"`
	Status    string `json:"status,omitempty"`
	MediaType string `json:"media_type"`
}

type Captures struct {
	Documents map[string][]byte
	Licenses  map[string][]byte
}

type CapturedDocument struct {
	Metadata SourceDocument
	Bytes    []byte
	License  []byte
}

type CapturedSource struct {
	Lock      SourceLock
	Digest    string
	Documents []CapturedDocument
	byID      map[string]int
}

func DecodeSourceLock(document []byte) (SourceLock, error) {
	var lock SourceLock
	if err := contract.DecodeStrictRequiredObject(
		document,
		&lock,
		"schema_version",
		"connector_id",
		"source_set",
		"operations",
		"semantics_revision",
	); err != nil {
		return SourceLock{}, sourceError(ErrInvalidSource, "", "", err.Error())
	}
	if err := lock.Validate(); err != nil {
		return SourceLock{}, err
	}
	return lock, nil
}

func (lock SourceLock) Validate() error {
	if lock.SchemaVersion != SourceLockSchemaVersion {
		return sourceError(ErrInvalidSource, "", "/schema_version", "source schema revision is unsupported")
	}
	if !validIdentity(lock.ConnectorID) {
		return sourceError(ErrInvalidSource, "", "/connector_id", "must be a nonempty canonical identity")
	}
	switch lock.SourceSet.Dialect {
	case DialectOpenAPI30, DialectOpenAPI31, DialectSwagger20, DialectPostgreSQLDocs16:
	default:
		return sourceError(ErrUnsupportedDialect, "", "/source_set/dialect", "dialect is not admitted by the source compiler")
	}
	if !lock.SourceSet.ReferencesClosed {
		return sourceError(ErrUnsafeReference, "", "/source_set/references_closed", "reference closure must be explicit and closed")
	}
	if len(lock.SourceSet.Documents) == 0 {
		return sourceError(ErrInvalidSource, "", "/source_set/documents", "at least one captured document is required")
	}
	if !validDigest(lock.SemanticsRevision) {
		return sourceError(ErrInvalidSource, "", "/semantics_revision", "must be a lowercase sha256 digest")
	}

	documents := make(map[string]struct{}, len(lock.SourceSet.Documents))
	for index, document := range lock.SourceSet.Documents {
		pointer := fmt.Sprintf("/source_set/documents/%d", index)
		if err := validateSourceDocument(document, lock.SourceSet.Dialect, pointer); err != nil {
			return err
		}
		if _, duplicate := documents[document.SourceID]; duplicate {
			return sourceError(ErrInvalidSource, document.SourceID, pointer+"/source_id", "source identity is duplicated")
		}
		documents[document.SourceID] = struct{}{}
	}
	if isDocumentationDialect(lock.SourceSet.Dialect) {
		for index, document := range lock.SourceSet.Documents {
			if len(document.References) != 0 {
				return sourceError(ErrUnsafeReference, document.SourceID, fmt.Sprintf("/source_set/documents/%d/references", index), "documentation dialects declare no reference closure; captured pages are self-contained")
			}
		}
	}
	for index, document := range lock.SourceSet.Documents {
		seen := make(map[string]struct{}, len(document.References))
		for refIndex, reference := range document.References {
			pointer := fmt.Sprintf("/source_set/documents/%d/references/%d", index, refIndex)
			if !validJSONPointer(reference.FromPointer) || reference.FromPointer == "" {
				return sourceError(ErrInvalidSource, document.SourceID, pointer+"/from_pointer", "must identify the exact $ref field")
			}
			if reference.Reference == "" {
				return sourceError(ErrInvalidSource, document.SourceID, pointer+"/reference", "reference text must be explicit")
			}
			if _, ok := documents[reference.TargetSourceID]; !ok {
				return sourceError(ErrUnsafeReference, document.SourceID, reference.FromPointer, "target source is absent from the captured closure")
			}
			if !validJSONPointer(reference.TargetPointer) {
				return sourceError(ErrInvalidSource, document.SourceID, pointer+"/target_pointer", "target pointer is malformed")
			}
			key := reference.FromPointer + "\x00" + reference.Reference
			if _, duplicate := seen[key]; duplicate {
				return sourceError(ErrInvalidSource, document.SourceID, reference.FromPointer, "reference declaration is duplicated")
			}
			seen[key] = struct{}{}
		}
	}

	if len(lock.Operations) == 0 {
		return sourceError(ErrInvalidSource, "", "/operations", "at least one source operation is required")
	}
	operationIDs := make(map[string]struct{}, len(lock.Operations))
	for index, operation := range lock.Operations {
		pointer := fmt.Sprintf("/operations/%d", index)
		if !validIdentity(operation.SourceOperationID) {
			return sourceError(ErrInvalidSource, "", pointer+"/source_operation_id", "must be a stable source operation identity")
		}
		if _, duplicate := operationIDs[operation.SourceOperationID]; duplicate {
			return sourceError(ErrInvalidSource, "", pointer+"/source_operation_id", "source operation identity is duplicated")
		}
		operationIDs[operation.SourceOperationID] = struct{}{}
		if len(operation.SourceRefs) == 0 {
			return sourceError(ErrInvalidSource, "", pointer+"/source_refs", "at least one exact source reference is required")
		}
		for refIndex, reference := range operation.SourceRefs {
			if _, ok := documents[reference.SourceID]; !ok {
				return sourceError(ErrInvalidSource, reference.SourceID, fmt.Sprintf("%s/source_refs/%d", pointer, refIndex), "operation source is absent from the captured closure")
			}
			if !validSourceSelector(lock.SourceSet.Dialect, reference.Pointer) || reference.Pointer == "" {
				return sourceError(ErrInvalidSource, reference.SourceID, reference.Pointer, "operation source selector is malformed")
			}
		}
		if isDocumentationDialect(lock.SourceSet.Dialect) {
			if err := validateDocumentationOperation(operation, pointer); err != nil {
				return err
			}
			continue
		}
		if operation.StatementKind != nil {
			return sourceError(ErrInvalidSource, "", pointer+"/statement_kind", "an interface-description operation cannot declare a PostgreSQL statement kind")
		}
		if operation.RelationBinding != nil {
			return sourceError(ErrInvalidSource, "", pointer+"/relation_binding", "an interface-description operation cannot declare a PostgreSQL relation binding")
		}
		if len(operation.RequestVariants) == 0 || len(operation.ResponseVariants) == 0 {
			return sourceError(ErrInvalidSource, "", pointer, "request and response variants must both be explicit")
		}
		if err := validateVariantRefs(operation.RequestVariants, false, pointer+"/request_variants"); err != nil {
			return err
		}
		if err := validateVariantRefs(operation.ResponseVariants, true, pointer+"/response_variants"); err != nil {
			return err
		}
	}
	return nil
}

// validateDocumentationOperation admits one documentation-dialect operation:
// the typed statement facts this dialect needs, and no interface-description
// variant inventory. Every member is closed and explicit, and an absent member
// stays distinct from an explicitly empty one.
func validateDocumentationOperation(operation SourceOperation, pointer string) error {
	if operation.StatementKind == nil {
		return sourceError(ErrInvalidSource, "", pointer+"/statement_kind", "a documentation operation must declare its PostgreSQL statement kind explicitly")
	}
	if !validStatementKind(*operation.StatementKind) {
		return sourceError(ErrInvalidSource, "", pointer+"/statement_kind", "PostgreSQL statement kind is unsupported")
	}
	if operation.RelationBinding == nil {
		return sourceError(ErrInvalidSource, "", pointer+"/relation_binding", "a documentation operation must declare its stable relation binding explicitly")
	}
	if !validRelationBinding(*operation.RelationBinding) {
		return sourceError(ErrInvalidSource, "", pointer+"/relation_binding", "relation binding must be a bounded stable identity")
	}
	if operation.RequestVariants == nil || len(operation.RequestVariants) != 0 {
		return sourceError(ErrInvalidSource, "", pointer+"/request_variants", "a documentation operation declares no request variants; the array must be explicitly empty")
	}
	if operation.ResponseVariants == nil || len(operation.ResponseVariants) != 0 {
		return sourceError(ErrInvalidSource, "", pointer+"/response_variants", "a documentation operation declares no response variants; the array must be explicitly empty")
	}
	return nil
}

// validStatementKind admits the PostgreSQL statement kinds this compiler can
// lower. It mirrors the transport contract so a lock-admitted statement can
// never fail contract validation at emission.
func validStatementKind(kind contract.PostgreSQLStatementKind) bool {
	switch kind {
	case contract.PostgreSQLStatementMetadata, contract.PostgreSQLStatementSelect, contract.PostgreSQLStatementMappedUpsert:
		return true
	default:
		return false
	}
}

// validRelationBinding mirrors contract's identity rule for the transport's
// relation binding: one bounded, trimmed, control-free UTF-8 identity.
func validRelationBinding(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || !utf8.ValidString(value) || len(value) > 512 {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validateSourceDocument(document SourceDocument, dialect Dialect, pointer string) error {
	if !validIdentity(document.SourceID) {
		return sourceError(ErrInvalidSource, document.SourceID, pointer+"/source_id", "must be a stable source identity")
	}
	if !validHTTPSURL(document.OfficialURL) {
		return sourceError(ErrInvalidSource, document.SourceID, pointer+"/official_url", "must be an absolute HTTPS source URL")
	}
	if document.Repository != "" && !validHTTPSURL(document.Repository) {
		return sourceError(ErrInvalidSource, document.SourceID, pointer+"/repository", "must be an absolute HTTPS repository URL")
	}
	if (document.Repository == "") != (document.Revision == "") {
		return sourceError(ErrInvalidSource, document.SourceID, pointer, "repository and revision must be declared together")
	}
	if err := validateUpstreamIdentity(document, pointer); err != nil {
		return err
	}
	if document.Path == "" || strings.ContainsRune(document.Path, '\x00') {
		return sourceError(ErrInvalidSource, document.SourceID, pointer+"/path", "source path must be explicit")
	}
	if _, err := time.Parse(time.RFC3339, document.CapturedAt); err != nil {
		return sourceError(ErrInvalidSource, document.SourceID, pointer+"/captured_at", "must be an RFC3339 timestamp")
	}
	if document.Pointer != "" && !validSourceSelector(dialect, document.Pointer) {
		return sourceError(ErrInvalidSource, document.SourceID, pointer+"/pointer", "source selector is malformed")
	}
	if document.ByteLength < 0 || document.ByteLength > MaxSourceDocumentBytes {
		return sourceError(ErrInvalidSource, document.SourceID, pointer+"/byte_length", "source byte length is outside the compiler bound")
	}
	if !validHex(document.SHA256, 64) {
		return sourceError(ErrInvalidSource, document.SourceID, pointer+"/sha256", "must be a lowercase SHA-256")
	}
	if !validIdentity(document.License.SPDX) || document.License.Path == "" || !validHex(document.License.SHA256, 64) {
		return sourceError(ErrInvalidSource, document.SourceID, pointer+"/license", "license identity, path and lowercase SHA-256 are required")
	}
	return nil
}

// validateUpstreamIdentity keeps the accepted upstream document's identity in
// one explicit triple. The compiler lowers the committed capture and verifies
// it against byte_length/sha256, so a partial triple would let the upstream
// identity read as the capture identity of a derived closure excerpt: a
// mismatch that no later step could detect.
func validateUpstreamIdentity(document SourceDocument, pointer string) error {
	declared := 0
	for _, present := range []bool{document.GitBlob != "", document.UpstreamByteLength != nil, document.UpstreamSHA256 != ""} {
		if present {
			declared++
		}
	}
	if declared == 0 {
		return nil
	}
	if declared != 3 {
		return sourceError(ErrInvalidSource, document.SourceID, pointer, "git_blob, upstream_byte_length and upstream_sha256 must be declared together")
	}
	if document.Repository == "" || !validHex(document.GitBlob, 40) {
		return sourceError(ErrInvalidSource, document.SourceID, pointer+"/git_blob", "git blob requires repository/revision and a lowercase SHA-1")
	}
	if *document.UpstreamByteLength < 0 {
		return sourceError(ErrInvalidSource, document.SourceID, pointer+"/upstream_byte_length", "upstream byte length cannot be negative")
	}
	if !validHex(document.UpstreamSHA256, 64) {
		return sourceError(ErrInvalidSource, document.SourceID, pointer+"/upstream_sha256", "must be a lowercase SHA-256")
	}
	return nil
}

func validateVariantRefs(variants []VariantRef, responses bool, pointer string) error {
	seen := make(map[string]struct{}, len(variants))
	for index, variant := range variants {
		if !validJSONPointer(strings.TrimPrefix(variant.Pointer, "#")) || variant.Pointer == "" {
			return sourceError(ErrInvalidSource, "", fmt.Sprintf("%s/%d/pointer", pointer, index), "variant pointer is malformed")
		}
		if variant.MediaType == "" {
			return sourceError(ErrInvalidSource, "", fmt.Sprintf("%s/%d/media_type", pointer, index), "media type must be explicit")
		}
		if responses {
			if !validResponseStatus(variant.Status) {
				return sourceError(ErrInvalidSource, "", fmt.Sprintf("%s/%d/status", pointer, index), "response status is unsupported")
			}
		} else if variant.Status != "" {
			return sourceError(ErrInvalidSource, "", fmt.Sprintf("%s/%d/status", pointer, index), "request variant cannot declare response status")
		}
		key := variant.Pointer + "\x00" + variant.Status + "\x00" + variant.MediaType
		if _, duplicate := seen[key]; duplicate {
			return sourceError(ErrInvalidSource, "", fmt.Sprintf("%s/%d", pointer, index), "variant is duplicated")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func CaptureSource(lock SourceLock, captures Captures) (*CapturedSource, error) {
	if err := lock.Validate(); err != nil {
		return nil, err
	}
	if captures.Documents == nil || captures.Licenses == nil {
		return nil, sourceError(ErrInvalidSource, "", "", "document and license capture maps must both be explicit")
	}
	if len(captures.Documents) != len(lock.SourceSet.Documents) || len(captures.Licenses) != len(lock.SourceSet.Documents) {
		return nil, sourceError(ErrInvalidSource, "", "", "captures must contain exactly the admitted source and license identities")
	}

	captured := &CapturedSource{
		Lock:      lock,
		Documents: make([]CapturedDocument, 0, len(lock.SourceSet.Documents)),
		byID:      make(map[string]int, len(lock.SourceSet.Documents)),
	}
	for _, metadata := range lock.SourceSet.Documents {
		document, ok := captures.Documents[metadata.SourceID]
		if !ok {
			return nil, sourceError(ErrInvalidSource, metadata.SourceID, "", "captured source bytes are missing")
		}
		license, ok := captures.Licenses[metadata.SourceID]
		if !ok {
			return nil, sourceError(ErrInvalidSource, metadata.SourceID, "", "captured license bytes are missing")
		}
		if int64(len(document)) != metadata.ByteLength {
			return nil, sourceError(ErrSourceFidelity, metadata.SourceID, "", "captured byte length does not match the source lock")
		}
		if digestHex(document) != metadata.SHA256 {
			return nil, sourceError(ErrSourceFidelity, metadata.SourceID, "", "captured bytes do not match the source lock SHA-256")
		}
		if digestHex(license) != metadata.License.SHA256 {
			return nil, sourceError(ErrSourceFidelity, metadata.SourceID, "", "captured license does not match the source lock SHA-256")
		}
		captured.byID[metadata.SourceID] = len(captured.Documents)
		captured.Documents = append(captured.Documents, CapturedDocument{
			Metadata: metadata,
			Bytes:    bytes.Clone(document),
			License:  bytes.Clone(license),
		})
	}
	for sourceID := range captures.Documents {
		if _, ok := captured.byID[sourceID]; !ok {
			return nil, sourceError(ErrInvalidSource, sourceID, "", "unadmitted source capture was supplied")
		}
	}
	for sourceID := range captures.Licenses {
		if _, ok := captured.byID[sourceID]; !ok {
			return nil, sourceError(ErrInvalidSource, sourceID, "", "unadmitted license capture was supplied")
		}
	}
	captured.Digest = capturedSourceDigest(lock)
	return captured, nil
}

func (source *CapturedSource) Document(sourceID string) (CapturedDocument, bool) {
	if source == nil {
		return CapturedDocument{}, false
	}
	index, ok := source.byID[sourceID]
	if !ok {
		return CapturedDocument{}, false
	}
	document := source.Documents[index]
	document.Bytes = bytes.Clone(document.Bytes)
	document.License = bytes.Clone(document.License)
	return document, true
}

func capturedSourceDigest(lock SourceLock) string {
	encoded, err := json.Marshal(lock)
	if err != nil {
		panic(err)
	}
	return "sha256:" + digestHex(encoded)
}

func digestHex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func validIdentity(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && len(value) <= 1024
}

func validHTTPSURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == ""
}

func validHex(value string, size int) bool {
	if len(value) != size || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && validHex(strings.TrimPrefix(value, "sha256:"), 64)
}

func validJSONPointer(value string) bool {
	if value == "" {
		return true
	}
	if !strings.HasPrefix(value, "/") {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] != '~' {
			continue
		}
		if index+1 >= len(value) || (value[index+1] != '0' && value[index+1] != '1') {
			return false
		}
		index++
	}
	return true
}

func validResponseStatus(value string) bool {
	if value == "default" {
		return true
	}
	return len(value) == 3 && value[0] >= '1' && value[0] <= '5' && value[1] >= '0' && value[1] <= '9' && value[2] >= '0' && value[2] <= '9'
}
