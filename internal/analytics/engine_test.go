package analytics

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSelectParquetSchemaAndValues(t *testing.T) {
	for _, mode := range []string{"plain", "aliased", "mixed_reordered", "swapped_names", "empty"} {
		t.Run(mode, func(t *testing.T) {
			request, observer := parquetSelectionFixture(t)
			if mode == "mixed_reordered" {
				project := request.Select.Project
				project[0], project[2] = project[2], project[0]
			}
			for index := range request.Select.Project {
				if mode == "aliased" || (mode == "mixed_reordered" && index%2 == 0) {
					request.Select.Project[index].Alias = "out_" + request.Select.Project[index].Column.Name
				}
			}
			if mode == "swapped_names" {
				request.Select.Project[0].Alias = "title"
				request.Select.Project[1].Alias = "call_id"
			}
			if mode == "empty" {
				request.Select.Predicates = []Predicate{{Column: "call_id", Operator: OperatorEqual, Value: Int64Value(1)}}
			}
			output := request.Select.Output
			request.Select.Output = nil
			inline, err := RunAnalyticRequest(t.Context(), request)
			if err != nil {
				t.Fatalf("select inline rows: %v", err)
			}
			request.Select.Output = output
			written, err := RunAnalyticRequest(t.Context(), request)
			if err != nil {
				t.Fatalf("write selection: %v", err)
			}
			if written.Output == nil || !reflect.DeepEqual(written.Columns, inline.Columns) {
				t.Fatalf("written schema differs from inline schema: %+v", written)
			}
			rows, err := observer.Query("SELECT * FROM read_parquet(?) LIMIT 0", output.Path)
			if err != nil {
				t.Fatalf("inspect output independently: %v", err)
			}
			types, err := rows.ColumnTypes()
			closeErr := rows.Close()
			if err != nil || closeErr != nil {
				t.Fatalf("inspect output types: %v, close: %v", err, closeErr)
			}
			if len(types) != len(request.Select.Project) {
				t.Fatalf("output has %d columns, want %d", len(types), len(request.Select.Project))
			}
			wantTypes := map[string]string{
				"call_id": "BIGINT", "title": "VARCHAR", "amount": "DECIMAL(18,2)",
				"started_at": "TIMESTAMPTZ", "local_at": "TIMESTAMP", "day": "DATE",
				"payload": "BLOB", "identifier": "UUID", "document": "JSON", "enabled": "BOOLEAN", "small": "BIGINT",
			}
			for index, projection := range request.Select.Project {
				if types[index].Name() != projection.OutputName() || types[index].DatabaseTypeName() != wantTypes[projection.Column.Name] {
					t.Errorf("column %d is %s %s, want %s %s", index, types[index].Name(), types[index].DatabaseTypeName(),
						projection.OutputName(), wantTypes[projection.Column.Name])
				}
			}
			read := request
			read.Relations = []Relation{{
				Name: "result", Path: written.Output.Path, SHA256: written.Output.SHA256,
				Bytes: written.Output.Bytes, Columns: written.Columns,
			}}
			read.Select = &Select{From: "result", Limit: 10}
			for index, column := range written.Columns {
				read.Select.Project = append(read.Select.Project, Projection{Column: column})
				if request.Select.Project[index].Column.Name == "call_id" {
					read.Select.Order = []Ordering{{Column: column.Name}}
				}
			}
			decoded, err := RunAnalyticRequest(t.Context(), read)
			if err != nil {
				t.Fatalf("read with returned schema: %v", err)
			}
			if !reflect.DeepEqual(decoded.Rows, inline.Rows) {
				t.Fatalf("file rows differ from inline rows:\n got %v\nwant %v", decoded.Rows, inline.Rows)
			}
			if mode == "empty" {
				if len(decoded.Rows) != 0 {
					t.Fatal("empty selection returned rows")
				}
				return
			}
			if len(decoded.Rows) != 2 {
				t.Fatalf("decoded %d rows, want 2", len(decoded.Rows))
			}
			for index, projection := range request.Select.Project {
				if projection.Column.Name == "call_id" {
					if id, ok := decoded.Rows[0][index].Int64(); !ok || id != -9007199254740993 {
						t.Fatalf("large identifier changed: %v", decoded.Rows[0][index])
					}
				} else if !decoded.Rows[0][index].Null() {
					t.Errorf("column %s lost its null value", projection.OutputName())
				}
			}
		})
	}
}

func TestSelectRefusesInvalidOutputNames(t *testing.T) {
	for _, aliases := range [][2]string{{"x", "x"}, {"x", "X"}, {"title", ""}, {"bad name", "other"}, {"x\"", "other"}} {
		t.Run(strings.Join(aliases[:], "_"), func(t *testing.T) {
			request, _ := parquetSelectionFixture(t)
			request.Select.Project = request.Select.Project[:2]
			request.Select.Project[0].Alias = aliases[0]
			request.Select.Project[1].Alias = aliases[1]
			if _, err := RunAnalyticRequest(t.Context(), request); err == nil {
				t.Fatal("invalid output names were accepted")
			}
			if _, err := os.Lstat(request.Select.Output.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused output exists or cannot be inspected: %v", err)
			}
		})
	}
}

func TestWriteSegmentRefusesSchemaMismatch(t *testing.T) {
	for _, test := range []struct {
		name      string
		statement string
		columns   []Column
	}{
		{"name", "SELECT 1::BIGINT AS actual", []Column{{Name: "expected", Kind: KindInt64}}},
		{"count", "SELECT 1::BIGINT AS a, 2::BIGINT AS b", []Column{{Name: "a", Kind: KindInt64}}},
		{"order", "SELECT 1::BIGINT AS b, 2::BIGINT AS a", []Column{{Name: "a", Kind: KindInt64}, {Name: "b", Kind: KindInt64}}},
		{"kind", "SELECT '1'::VARCHAR AS a", []Column{{Name: "a", Kind: KindInt64}}},
		{"scale", "SELECT 1.234::DECIMAL(18,3) AS a", []Column{{Name: "a", Kind: KindDecimal, Precision: 18, Scale: 2}}},
		{"precision", "SELECT 1.23::DECIMAL(12,2) AS a", []Column{{Name: "a", Kind: KindDecimal, Precision: 18, Scale: 2}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, _ := parquetSelectionFixture(t)
			engine, err := openEngine(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = engine.Close() }()
			result, err := engine.writeSegment(t.Context(), request, test.statement, nil, test.columns)
			if err == nil || FailureCode(err) != CodeOutputFailed {
				t.Fatalf("schema mismatch returned %v, want CodeOutputFailed", err)
			}
			if result.Output != nil {
				t.Fatal("schema mismatch returned an output descriptor")
			}
			if _, err := os.Lstat(request.Select.Output.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("mismatched output was not removed: %v", err)
			}
		})
	}
}

func parquetSelectionFixture(t *testing.T) (Request, *sql.DB) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", root)
	observer, err := sql.Open("duckdb", "")
	if err != nil {
		t.Fatal(err)
	}
	observer.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = observer.Close() })
	statement := `CREATE TABLE calls AS SELECT
		9007199254740993::BIGINT AS call_id, 'First'::VARCHAR AS title,
		10.50::DECIMAL(18,2) AS amount, '2026-10-01T12:00:00.123456+02:00'::TIMESTAMPTZ AS started_at,
		'2026-10-01 10:00:00.123456'::TIMESTAMP AS local_at, '2026-10-01'::DATE AS day,
		from_hex('00ff') AS payload, '123e4567-e89b-12d3-a456-426614174000'::UUID AS identifier,
		'{"n":9007199254740993}'::JSON AS document, true AS enabled, 7::TINYINT AS small
		UNION ALL SELECT -9007199254740993, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL`
	if _, err := observer.Exec(statement); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "source.parquet")
	if _, err := observer.Exec("COPY calls TO " + quoteSQLString(path) + " (FORMAT PARQUET)"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	columns := []Column{
		{Name: "call_id", Kind: KindInt64}, {Name: "title", Kind: KindText, Nullable: true},
		{Name: "amount", Kind: KindDecimal, Precision: 18, Scale: 2, Nullable: true},
		{Name: "started_at", Kind: KindTimestampTZ, Nullable: true}, {Name: "local_at", Kind: KindTimestamp, Nullable: true},
		{Name: "day", Kind: KindDate, Nullable: true}, {Name: "payload", Kind: KindBytea, Nullable: true},
		{Name: "identifier", Kind: KindUUID, Nullable: true}, {Name: "document", Kind: KindJSON, Nullable: true},
		{Name: "enabled", Kind: KindBoolean, Nullable: true}, {Name: "small", Kind: KindInt64, Nullable: true},
	}
	request := Request{
		AdmittedDirectories: []string{root},
		Bounds:              BoundAnalytics{Relations: 1, Columns: 16, Rows: 10, Bytes: 1 << 20, TempBytes: 64 << 20, MemoryBytes: 128 << 20, WallTime: time.Minute},
		Relations:           []Relation{{Name: "calls", Path: path, SHA256: "sha256:" + hex.EncodeToString(digest[:]), Bytes: int64(len(content)), Columns: columns}},
		Select:              &Select{From: "calls", Order: []Ordering{{Column: "call_id"}}, Limit: 10, Output: &Output{Path: filepath.Join(root, "result.parquet")}},
	}
	for _, column := range columns {
		request.Select.Project = append(request.Select.Project, Projection{Column: Column{Name: column.Name}})
	}
	return request, observer
}
