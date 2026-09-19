package warehouse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
)

// A warehouse segment is one immutable Parquet document produced by the
// restricted child. This package materializes it through the child, verifies
// the actual bytes it left behind, and hands those bytes to the shared object
// store. It never imports the native engine and never writes a shared DuckDB
// file: the only engine that ever touches Parquet is the bounded child.
const (
	// ParquetSegmentMediaType is the sealed media type of every segment.
	ParquetSegmentMediaType = "application/vnd.apache.parquet"
	// parquetMagic opens and closes every Parquet document.
	parquetMagic = "PAR1"
	// maximumSegmentSchemaBytes bounds one canonical schema document.
	maximumSegmentSchemaBytes = 64 << 10
)

// SegmentSchema is the canonical, ordered column declaration of one segment. Its
// fingerprint is what the ledger records, so a later read can prove that the
// schema it serves is the schema that was registered.
type SegmentSchema struct {
	columns []QueryColumn
}

// NewSegmentSchema validates and seals one ordered column declaration.
func NewSegmentSchema(columns []QueryColumn) (SegmentSchema, error) {
	if len(columns) == 0 || len(columns) > MaximumQueryColumns {
		return SegmentSchema{}, refusal(RefusalBoundExceeded, "a segment schema must declare 1..%d columns", MaximumQueryColumns)
	}
	declared := make(map[string]struct{}, len(columns))
	for _, column := range columns {
		if err := column.Validate(); err != nil {
			return SegmentSchema{}, err
		}
		if _, duplicate := declared[column.Name]; duplicate {
			return SegmentSchema{}, refusal(RefusalSchemaMismatch, "segment column %q is declared more than once", column.Name)
		}
		declared[column.Name] = struct{}{}
	}
	return SegmentSchema{columns: append([]QueryColumn(nil), columns...)}, nil
}

// Columns hands out the ordered declaration without sharing its backing array.
func (schema SegmentSchema) Columns() []QueryColumn {
	return append([]QueryColumn(nil), schema.columns...)
}

// Empty reports whether no column was declared.
func (schema SegmentSchema) Empty() bool { return len(schema.columns) == 0 }

// Encode renders the canonical schema document. Field order is fixed by the
// document type, so two equal declarations always render identical bytes.
func (schema SegmentSchema) Encode() ([]byte, error) {
	document := segmentSchemaDocument{Columns: make([]segmentSchemaColumn, 0, len(schema.columns))}
	for _, column := range schema.columns {
		document.Columns = append(document.Columns, segmentSchemaColumn{
			Name:      column.Name,
			Kind:      string(column.Kind),
			Precision: column.Precision,
			Scale:     column.Scale,
			Nullable:  column.Nullable,
		})
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, refusal(RefusalSchemaMismatch, "encode the declared segment schema")
	}
	return encoded, nil
}

// Fingerprint is the canonical identity of this declaration.
func (schema SegmentSchema) Fingerprint() string {
	encoded, err := schema.Encode()
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// DecodeSegmentSchema reads one stored schema document and refuses a document
// that is not a complete, well-formed declaration.
func DecodeSegmentSchema(document []byte) (SegmentSchema, error) {
	if len(document) == 0 || len(document) > maximumSegmentSchemaBytes {
		return SegmentSchema{}, refusal(RefusalSchemaMismatch, "a stored segment schema document is absent or oversized")
	}
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	var stored segmentSchemaDocument
	if err := decoder.Decode(&stored); err != nil {
		return SegmentSchema{}, refusal(RefusalSchemaMismatch, "the stored segment schema is not a readable declaration")
	}
	if decoder.More() {
		return SegmentSchema{}, refusal(RefusalSchemaMismatch, "the stored segment schema carries trailing content")
	}
	columns := make([]QueryColumn, 0, len(stored.Columns))
	for _, column := range stored.Columns {
		columns = append(columns, QueryColumn{
			Name:      column.Name,
			Kind:      QueryKind(column.Kind),
			Precision: column.Precision,
			Scale:     column.Scale,
			Nullable:  column.Nullable,
		})
	}
	schema, err := NewSegmentSchema(columns)
	if err != nil {
		return SegmentSchema{}, err
	}
	// The database re-renders a stored jsonb document, so the canonical
	// identity of this declaration is its fingerprint. Every reader compares
	// that fingerprint with the registered one before serving a row.
	return schema, nil
}

type segmentSchemaDocument struct {
	Columns []segmentSchemaColumn `json:"columns"`
}

type segmentSchemaColumn struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Precision int    `json:"precision"`
	Scale     int    `json:"scale"`
	Nullable  bool   `json:"nullable"`
}

// SegmentBytes is the verified byte identity of one materialized segment beside
// the declaration it was materialized for. Every field is an observation of the
// admitted local file, not of the request that produced it.
type SegmentBytes struct {
	Path   string
	SHA256 string
	Bytes  int64
	Schema SegmentSchema
}

// Validate refuses a segment identity that cannot be published or read.
func (segment SegmentBytes) Validate() error {
	if segment.Path == "" || !filepath.IsAbs(segment.Path) || filepath.Clean(segment.Path) != segment.Path ||
		len(segment.SHA256) != len("sha256:")+64 || segment.SHA256[:len("sha256:")] != "sha256:" {
		return refusal(RefusalUnadmittedPath, "a segment must name one absolute verified local file with a canonical digest")
	}
	if _, err := hex.DecodeString(segment.SHA256[len("sha256:"):]); err != nil {
		return refusal(RefusalDigestMismatch, "a segment digest must be lowercase hexadecimal")
	}
	if segment.Bytes <= 0 {
		return refusal(RefusalUnadmittedPath, "a segment must declare a positive byte length")
	}
	if segment.Schema.Empty() {
		return refusal(RefusalSchemaMismatch, "a segment must declare its column schema")
	}
	return nil
}

// MaterializeRequest is one bounded segment materialization through the child.
type MaterializeRequest struct {
	RequestID string
	// AdmittedDirectories are the directories the child may read and write for
	// this materialization. The output must be inside one of them.
	AdmittedDirectories []string
	OutputPath          string
	Columns             []QueryColumn
	Rows                [][]QueryValue
	Bounds              QueryBounds
	// MaximumBytes is this package's own cap on the materialized segment. It is
	// never raised by the request.
	MaximumBytes int64
}

// WriteSegment is the declared warehouse segment writer entry point: it
// constructs one supervisor from the validated configuration and materializes
// exactly one Parquet segment through it. A caller that holds a supervisor
// configuration rather than a constructed instance uses this seam;
// MaterializeParquetSegment is the underlying materialization for a retained
// supervisor.
func WriteSegment(ctx context.Context, config ChildConfig, request MaterializeRequest) (SegmentBytes, error) {
	var segment SegmentBytes
	child, err := NewQueryChild(config)
	if err != nil {
		return segment, err
	}
	return MaterializeParquetSegment(ctx, child, request)
}

// MaterializeParquetSegment writes one immutable Parquet segment through the
// restricted child and returns the verified local bytes. The declared bounds and
// the output path are admitted before any child process exists, and the bytes
// the child left behind are re-measured here: a digest, length, or document that
// disagrees with the child's own report is refused and removed rather than
// published.
func MaterializeParquetSegment(ctx context.Context, child *QueryChild, request MaterializeRequest) (SegmentBytes, error) {
	var segment SegmentBytes
	if child == nil {
		return segment, refusal(RefusalProtocol, "segment materialization requires a query child supervisor")
	}
	if request.MaximumBytes <= 0 {
		return segment, refusal(RefusalBoundExceeded, "segment materialization requires a positive byte cap")
	}
	schema, err := NewSegmentSchema(request.Columns)
	if err != nil {
		return segment, err
	}
	if request.OutputPath == "" || !filepath.IsAbs(request.OutputPath) || filepath.Clean(request.OutputPath) != request.OutputPath {
		return segment, refusal(RefusalUnadmittedPath, "the segment output must name one absolute, cleaned local path")
	}
	if !withinAdmittedRoots(request.OutputPath, request.AdmittedDirectories) {
		return segment, refusal(RefusalUnadmittedPath, "the segment output is outside the admitted directories")
	}
	if err := inspectAdmittedOutput(request.OutputPath); err != nil {
		return segment, err
	}
	response, err := child.Execute(ctx, QueryRequest{
		RequestID:           request.RequestID,
		AdmittedDirectories: request.AdmittedDirectories,
		Bounds:              request.Bounds,
		Encode: &QueryEncode{
			Columns: schema.Columns(),
			Rows:    request.Rows,
			Output:  QueryOutput{Path: request.OutputPath},
		},
	})
	if err != nil {
		return segment, err
	}
	if response.Output == nil || response.Output.Path != request.OutputPath {
		return segment, refusal(RefusalOutputFailed, "the child did not report the admitted segment output")
	}
	segment, err = verifyMaterializedSegment(request.OutputPath, response.Output.SHA256, response.Output.Bytes, request.MaximumBytes, schema)
	if err != nil {
		_ = os.Remove(request.OutputPath)
		return SegmentBytes{}, err
	}
	return segment, nil
}

// verifyMaterializedSegment measures the file the child left behind. Nothing
// here trusts the child's own description: the digest, the length, and the
// Parquet envelope are all re-derived from the actual bytes.
func verifyMaterializedSegment(path, reportedSHA256 string, reportedBytes, maximumBytes int64, schema SegmentSchema) (SegmentBytes, error) {
	var segment SegmentBytes
	info, err := os.Lstat(path)
	if err != nil {
		return segment, refusal(RefusalOutputFailed, "the materialized segment is unreadable: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return segment, refusal(RefusalOutputFailed, "the materialized segment is not a regular file")
	}
	if info.Size() != reportedBytes {
		return segment, refusal(RefusalOutputFailed, "the materialized segment is %d bytes, not the reported %d", info.Size(), reportedBytes)
	}
	if info.Size() > maximumBytes {
		return segment, refusal(RefusalBoundExceeded, "the materialized segment exceeds the admitted byte cap")
	}
	file, err := os.Open(path)
	if err != nil {
		return segment, refusal(RefusalOutputFailed, "open the materialized segment: %v", err)
	}
	defer func() { _ = file.Close() }()
	digest := sha256.New()
	written, err := io.Copy(digest, io.LimitReader(file, maximumBytes+1))
	if err != nil {
		return segment, refusal(RefusalOutputFailed, "read the materialized segment: %v", err)
	}
	if written != info.Size() || written > maximumBytes {
		return segment, refusal(RefusalOutputFailed, "the materialized segment changed while it was verified")
	}
	observed := "sha256:" + hex.EncodeToString(digest.Sum(nil))
	if reportedSHA256 != observed {
		return segment, refusal(RefusalDigestMismatch, "the materialized segment does not match the digest the child reported")
	}
	if err := verifyParquetEnvelope(file, info.Size()); err != nil {
		return segment, err
	}
	return SegmentBytes{Path: path, SHA256: observed, Bytes: info.Size(), Schema: schema}, nil
}

// verifyParquetEnvelope refuses a document that is not a Parquet file. An
// admitted segment must open and close with the Parquet magic.
func verifyParquetEnvelope(file *os.File, size int64) error {
	if size < int64(2*len(parquetMagic)) {
		return refusal(RefusalOutputFailed, "the materialized segment is too short to be a Parquet document")
	}
	opening := make([]byte, len(parquetMagic))
	if _, err := file.ReadAt(opening, 0); err != nil {
		return refusal(RefusalOutputFailed, "read the materialized segment header: %v", err)
	}
	trailing := make([]byte, len(parquetMagic))
	if _, err := file.ReadAt(trailing, size-int64(len(parquetMagic))); err != nil {
		return refusal(RefusalOutputFailed, "read the materialized segment footer: %v", err)
	}
	if string(opening) != parquetMagic || string(trailing) != parquetMagic {
		return refusal(RefusalOutputFailed, "the materialized segment is not a Parquet document")
	}
	return nil
}

// LoadSegmentBytes reads one verified local segment into bounded memory and
// re-verifies its digest and length, so the bytes handed to the shared object
// store are the bytes that were measured.
func LoadSegmentBytes(segment SegmentBytes, maximumBytes int64) ([]byte, error) {
	if err := segment.Validate(); err != nil {
		return nil, err
	}
	if maximumBytes <= 0 || segment.Bytes > maximumBytes {
		return nil, refusal(RefusalBoundExceeded, "the segment exceeds the admitted byte cap")
	}
	info, err := os.Lstat(segment.Path)
	if err != nil {
		return nil, refusal(RefusalUnadmittedPath, "the admitted segment is unreadable: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, refusal(RefusalUnadmittedPath, "the admitted segment is not a regular file")
	}
	file, err := os.Open(segment.Path)
	if err != nil {
		return nil, refusal(RefusalUnadmittedPath, "open the admitted segment: %v", err)
	}
	defer func() { _ = file.Close() }()
	payload, err := io.ReadAll(io.LimitReader(file, maximumBytes+1))
	if err != nil {
		return nil, refusal(RefusalOutputFailed, "read the admitted segment: %v", err)
	}
	if int64(len(payload)) != segment.Bytes {
		return nil, refusal(RefusalDigestMismatch, "the admitted segment changed since it was verified")
	}
	digest := sha256.Sum256(payload)
	if observed := "sha256:" + hex.EncodeToString(digest[:]); observed != segment.SHA256 {
		return nil, refusal(RefusalDigestMismatch, "the admitted segment does not match its verified digest")
	}
	return payload, nil
}

// segmentDigestHex renders the bare hexadecimal digest the shared object store
// registers.
func segmentDigestHex(digest string) string { return digest[len("sha256:"):] }
