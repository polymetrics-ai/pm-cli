package warehouse_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/warehouse"
)

// The isolation fixture owns four observable surfaces: the admitted directory
// holding the only readable inputs, a probes directory holding the test's own
// hostile inputs (a symlink, a FIFO, a descriptor canary), an outside directory
// that no child may reach, and a read-only state directory holding the source
// manifest and effect record whose immutability under a refused or killed child
// is the R18 observation. The seal covers the admitted and state directories
// only, so the test's own hostile fixtures never weaken it.
const (
	isolationCanaryDescriptorBytes = 4097
	isolationBulkRows              = 40000
	isolationRootStatePermission   = 0o500
	isolationSourceRelation        = "orders"
	// isolationChildRefusalStatus is the exit status the restricted child uses
	// for one typed refusal document, so the supervisor's own report of an
	// observed status can be compared with the child's declared contract.
	isolationChildRefusalStatus = 1
)

// TestQueryProcessIsolation is the R18 acceptance case. It exercises the real
// restricted child binary through the parent supervisor and observes, from
// outside both processes, that prohibited requests never reach execution, that
// declared bounds beyond the child capability are refused, that a killed child
// leaves the source manifest and effect record byte-identical, and that the
// child inherits neither credentials nor descriptors.
func TestQueryProcessIsolation(t *testing.T) {
	fixture := newQueryIsolationFixture(t)

	t.Run("admitted_request_returns_exact_typed_values", func(t *testing.T) {
		child := fixture.newChild(t, fixture.defaultLimits())

		request := fixture.sourceRequest()
		response, err := child.Execute(t.Context(), request)
		if err != nil {
			t.Fatalf("execute admitted request: %v", err)
		}
		if response.Truncated {
			t.Fatalf("admitted request reported a truncated result for %d complete rows", len(fixture.rows))
		}
		if !reflect.DeepEqual(response.Columns, fixture.columns) {
			t.Fatalf("projected columns differ:\n got %s\nwant %s", describeColumns(response.Columns), describeColumns(fixture.columns))
		}
		// The request declares ascending order by id, so the exact rows must
		// arrive in that declared order.
		expected := fixture.rowsAscendingByID()
		if !reflect.DeepEqual(response.Rows, expected) {
			t.Fatalf("exact typed rows differ:\n got %s\nwant %s", describeRows(response.Rows), describeRows(expected))
		}
		fixture.observeChildReport(t, response)

		// A typed predicate, order and limit travel as data. The same query
		// with a two-row bound must truncate truthfully rather than silently.
		bounded := fixture.sourceRequest()
		bounded.Select.Predicates = []warehouse.QueryPredicate{{Column: "id", Operator: warehouse.QueryOperatorGreaterEqual, Value: warehouse.Int64QueryValue(0)}}
		bounded.Select.Order = []warehouse.QueryOrder{{Column: "id", Descending: true}}
		bounded.Bounds.Rows = 2
		truncated, err := child.Execute(t.Context(), bounded)
		if err != nil {
			t.Fatalf("execute row-bounded request: %v", err)
		}
		if !truncated.Truncated {
			t.Fatal("row bound that cuts the result did not report truncation")
		}
		if len(truncated.Rows) != 2 {
			t.Fatalf("row bound returned %d rows, want 2", len(truncated.Rows))
		}
		// Descending by id over an exact int64 predicate: the two largest
		// matching identifiers, in that order.
		largest, middle := fixture.rows[3], fixture.rows[0]
		if !reflect.DeepEqual(truncated.Rows[0], largest) || !reflect.DeepEqual(truncated.Rows[1], middle) {
			t.Fatalf("descending order over an exact predicate returned %s", describeRows(truncated.Rows))
		}

		// The protocol carries typed intent, never statement text.
		encoded, err := warehouse.EncodeQueryRequest(bounded)
		if err != nil {
			t.Fatalf("encode request: %v", err)
		}
		for _, forbidden := range []string{"SELECT", "select ", "read_parquet", "read_csv", "INSERT", "COPY "} {
			if bytes.Contains(encoded, []byte(forbidden)) {
				t.Fatalf("encoded request carried statement text %q", forbidden)
			}
		}
		decoded, err := warehouse.DecodeQueryRequest(encoded)
		if err != nil {
			t.Fatalf("decode encoded request: %v", err)
		}
		if decoded.Select == nil || decoded.Select.Limit != int64(len(fixture.rows)) || len(decoded.Relations) != 1 {
			t.Fatalf("round-tripped request lost its typed intent: %+v", decoded.Select)
		}
		if !reflect.DeepEqual(decoded.Relations[0].Columns, fixture.columns) {
			t.Fatal("round-tripped request lost the declared relation schema")
		}
		if !fixture.rows[0][10].IsNull() {
			t.Fatal("fixture no longer carries an explicit null cell")
		}
	})

	t.Run("declared_entry_point_executes_one_attempt", func(t *testing.T) {
		// The declared seam constructs the supervisor from its configuration and
		// executes one request: a caller that holds a configuration rather than a
		// constructed supervisor uses it.
		request := fixture.sourceRequest()
		request.Select.Limit = 2
		response, err := warehouse.ExecuteQueryChild(t.Context(), warehouse.ChildConfig{
			BinaryPath:    fixture.childPath,
			ScratchRoot:   fixture.scratchRoot,
			AdmittedRoots: []string{fixture.admitted, fixture.probes},
			Limits:        fixture.defaultLimits(),
		}, request)
		if err != nil {
			t.Fatalf("execute one attempt through the declared entry point: %v", err)
		}
		expected := fixture.rowsAscendingByID()[:2]
		if !reflect.DeepEqual(response.Rows, expected) {
			t.Fatalf("the declared entry point returned %s, want %s", describeRows(response.Rows), describeRows(expected))
		}
		fixture.observeChildReport(t, response)
		fixture.expectScratchEmpty(t)
	})

	t.Run("unhonourable_supervisor_document_caps_are_refused", func(t *testing.T) {
		// One number both admits and decodes: a cap above the protocol document
		// capability would admit an attempt whose typed document the codec
		// refuses, after the child had already run and materialized an immutable
		// segment at the admitted output path.
		unhonourable := map[string]func(limits *warehouse.ChildLimits){
			"response_cap": func(limits *warehouse.ChildLimits) { limits.MaxResponseBytes = warehouseMaximumResponseCap + 1 },
			"request_cap":  func(limits *warehouse.ChildLimits) { limits.MaxRequestBytes = warehouse.MaxQueryDocumentBytes + 1 },
		}
		for name, mutate := range unhonourable {
			t.Run("supervisor_refuses_"+name, func(t *testing.T) {
				limits := fixture.maximumLimits()
				mutate(&limits)
				child, err := warehouse.NewQueryChild(warehouse.ChildConfig{
					BinaryPath:    fixture.childPath,
					ScratchRoot:   fixture.scratchRoot,
					AdmittedRoots: []string{fixture.admitted, fixture.probes},
					Limits:        limits,
				})
				if err == nil {
					t.Fatalf("the supervisor accepted a %s the protocol document codec cannot carry", name)
				}
				if refusal := requireRefusal(t, err); refusal.Code != warehouse.RefusalBoundExceeded {
					t.Fatalf("%s refusal code = %q, want %q: %s", name, refusal.Code, warehouse.RefusalBoundExceeded, refusal.Reason)
				}
				if child != nil {
					t.Fatalf("an unhonourable %s produced a usable supervisor", name)
				}
			})
		}

		// The largest honourable configuration is accepted, executes, and admits
		// the largest declared byte bound the codec can carry; one byte more is
		// refused by admission before any child exists.
		child := fixture.newChild(t, fixture.maximumLimits())
		largest := fixture.sourceRequest()
		largest.Select.Limit = 1
		largest.Bounds.Bytes = isolationByteBound
		if _, err := child.Execute(t.Context(), largest); err != nil {
			t.Fatalf("the largest honourable configuration refused an admitted request: %v", err)
		}
		oversized := fixture.sourceRequest()
		oversized.Select.Limit = 1
		oversized.Bounds.Bytes = isolationByteBound + 1
		response, err := child.Execute(t.Context(), oversized)
		if err == nil {
			t.Fatalf("the supervisor admitted a byte bound beyond its honourable response cap and returned %d rows", len(response.Rows))
		}
		refusal := requireRefusal(t, err)
		if refusal.Code != warehouse.RefusalBoundExceeded {
			t.Fatalf("byte-bound refusal code = %q, want %q: %s", refusal.Code, warehouse.RefusalBoundExceeded, refusal.Reason)
		}
		if refusal.Child != nil {
			t.Fatal("an over-cap byte bound reached a child process instead of being refused before execution")
		}
		fixture.expectScratchEmpty(t)
		fixture.expectDurableStateUnchanged(t)
	})

	t.Run("prohibited_requests_are_refused_before_execution", func(t *testing.T) {
		child := fixture.newChild(t, fixture.defaultLimits())

		// A document carrying statement text, a file function, an extension or
		// a remote URL has no place in the protocol and is refused by both the
		// parent codec and the child itself.
		prohibited := []struct {
			name     string
			document string
		}{
			{"raw_sql_member", `{"protocol":"pm.query/1","request_id":"r1","sql":"SELECT * FROM read_csv('/etc/hosts')"}`},
			{"statement_text_member", `{"protocol":"pm.query/1","request_id":"r1","statement":"SELECT 1"}`},
			{"file_function_member", `{"protocol":"pm.query/1","request_id":"r1","function":"read_csv","path":"/etc/hosts"}`},
			{"extension_member", `{"protocol":"pm.query/1","request_id":"r1","extension":"httpfs"}`},
			{"install_member", `{"protocol":"pm.query/1","request_id":"r1","install":"httpfs"}`},
			{"network_member", `{"protocol":"pm.query/1","request_id":"r1","url":"http://169.254.169.254/latest/meta-data"}`},
			{"duplicate_member", `{"protocol":"pm.query/1","request_id":"r1","request_id":"r2"}`},
			{"trailing_document", `{"protocol":"pm.query/1","request_id":"r1"} {"protocol":"pm.query/1"}`},
			{"absent_required_member", `{"protocol":"pm.query/1"}`},
			{"wrong_protocol", `{"protocol":"pm.query/2","request_id":"r1"}`},
			{"wrong_member_type", `{"protocol":"pm.query/1","request_id":"r1","bounds":{"relations":true}}`},
		}
		for _, testCase := range prohibited {
			t.Run("codec_refuses_"+testCase.name, func(t *testing.T) {
				if _, err := warehouse.DecodeQueryRequest([]byte(testCase.document)); err == nil {
					t.Fatalf("parent codec accepted a prohibited request document: %s", testCase.document)
				}
			})
			t.Run("child_refuses_"+testCase.name, func(t *testing.T) {
				run := fixture.runChildDocument(t, testCase.document, fixture.defaultLimits())
				if run.exitCode == 0 {
					t.Fatalf("child exited zero for a prohibited request; stdout=%q stderr=%q", run.stdout, run.stderr)
				}
				refusal := run.refusal(t)
				if refusal.Code != warehouse.RefusalProtocol {
					t.Fatalf("child refusal code = %q, want %q: %s", refusal.Code, warehouse.RefusalProtocol, refusal.Reason)
				}
			})
		}

		t.Run("codec_refuses_oversized_document", func(t *testing.T) {
			oversized := fmt.Sprintf(`{"protocol":"pm.query/1","request_id":"r1","padding":"%s"}`,
				strings.Repeat("p", warehouse.MaxQueryDocumentBytes))
			if _, err := warehouse.DecodeQueryRequest([]byte(oversized)); err == nil {
				t.Fatal("parent codec accepted a document above its declared byte bound")
			}
		})

		// Cross-scope paths, unregistered relations, unknown identifiers and
		// function-shaped projections are refused by the supervisor's own
		// admission checks before a child process exists.
		supervisorRefusals := []struct {
			name   string
			mutate func(t *testing.T, request *warehouse.QueryRequest)
			code   warehouse.RefusalCode
		}{
			{"cross_scope_path", func(_ *testing.T, request *warehouse.QueryRequest) {
				request.Relations[0].Path = fixture.outsidePath()
			}, warehouse.RefusalUnadmittedPath},
			{"remote_url_path", func(_ *testing.T, request *warehouse.QueryRequest) {
				request.Relations[0].Path = "http://169.254.169.254/source.parquet"
			}, warehouse.RefusalUnadmittedPath},
			{"symlink_path", func(t *testing.T, request *warehouse.QueryRequest) {
				link := filepath.Join(fixture.probes, "linked.parquet")
				if err := os.Symlink(fixture.sourcePath(), link); err != nil {
					t.Fatalf("create the symlink input: %v", err)
				}
				request.Relations[0].Path = link
				request.AdmittedDirectories = []string{fixture.admitted, fixture.probes}
			}, warehouse.RefusalUnadmittedPath},
			{"directory_path", func(_ *testing.T, request *warehouse.QueryRequest) {
				request.Relations[0].Path = fixture.outside
			}, warehouse.RefusalUnadmittedPath},
			{"absent_path", func(_ *testing.T, request *warehouse.QueryRequest) {
				request.Relations[0].Path = filepath.Join(fixture.probes, "absent.parquet")
				request.AdmittedDirectories = []string{fixture.admitted, fixture.probes}
			}, warehouse.RefusalUnadmittedPath},
			{"unknown_relation", func(_ *testing.T, request *warehouse.QueryRequest) {
				request.Select.From = "missing"
			}, warehouse.RefusalUnknownRelation},
			{"unknown_column", func(_ *testing.T, request *warehouse.QueryRequest) {
				request.Select.Project = []warehouse.QueryProjection{{Column: "no_such_column"}}
			}, warehouse.RefusalUnknownColumn},
			{"function_shaped_column", func(_ *testing.T, request *warehouse.QueryRequest) {
				request.Select.Project = []warehouse.QueryProjection{{Column: "read_csv('/etc/hosts')"}}
			}, warehouse.RefusalUnknownColumn},
			{"function_shaped_predicate", func(_ *testing.T, request *warehouse.QueryRequest) {
				request.Select.Predicates = []warehouse.QueryPredicate{{Column: "id", Operator: warehouse.QueryOperator("read_csv")}}
			}, warehouse.RefusalProtocol},
			{"output_outside_admitted_directory", func(_ *testing.T, request *warehouse.QueryRequest) {
				request.Select.Output = &warehouse.QueryOutput{Path: filepath.Join(fixture.outside, "written.parquet")}
			}, warehouse.RefusalUnadmittedPath},
			{"unadmitted_scope_declaration", func(_ *testing.T, request *warehouse.QueryRequest) {
				request.AdmittedDirectories = []string{fixture.outside}
			}, warehouse.RefusalUnadmittedPath},
			{"unknown_operator", func(_ *testing.T, request *warehouse.QueryRequest) {
				request.Select.Predicates = []warehouse.QueryPredicate{{Column: "id", Operator: warehouse.QueryOperator("like")}}
			}, warehouse.RefusalProtocol},
		}
		for _, testCase := range supervisorRefusals {
			t.Run("supervisor_refuses_"+testCase.name, func(t *testing.T) {
				request := fixture.sourceRequest()
				request.Select.Limit = 1
				testCase.mutate(t, &request)
				response, err := child.Execute(t.Context(), request)
				if err == nil {
					t.Fatalf("supervisor accepted a prohibited request and returned %d rows", len(response.Rows))
				}
				refusal := requireRefusal(t, err)
				if refusal.Code != testCase.code {
					t.Fatalf("refusal code = %q, want %q: %s", refusal.Code, testCase.code, refusal.Reason)
				}
				if refusal.Origin != warehouse.RefusalOriginSupervisor {
					t.Fatalf("refusal origin = %q, want supervisor: this class must be refused before a child executes", refusal.Origin)
				}
				if refusal.Child != nil {
					t.Fatal("supervisor refusal reported a child that should never have started")
				}
				if refusal.ExitCode != -1 {
					t.Fatalf("a refusal taken before a child exists reported exit status %d, want -1", refusal.ExitCode)
				}
				if len(response.Rows) != 0 {
					t.Fatalf("refused request invented %d rows", len(response.Rows))
				}
				fixture.expectScratchEmpty(t)
			})
		}

		// The child independently refuses the same classes: an unadmitted path,
		// an undeclared relation, a mismatched declared schema, a digest the
		// admitted bytes do not have, a NULL in a non-nullable column, and an
		// output it may not write. These documents bypass the supervisor so the
		// child's own guards are observable.
		childRefusals := []struct {
			name   string
			mutate func(t *testing.T, request *warehouse.QueryRequest)
			code   warehouse.RefusalCode
		}{
			{"cross_scope_path", func(_ *testing.T, request *warehouse.QueryRequest) {
				request.Relations[0].Path = fixture.outsidePath()
				request.AdmittedDirectories = []string{fixture.outside}
			}, warehouse.RefusalUnadmittedPath},
			{"undeclared_relation", func(_ *testing.T, request *warehouse.QueryRequest) {
				request.Select.From = "positions"
			}, warehouse.RefusalUnknownRelation},
			{"unknown_column", func(_ *testing.T, request *warehouse.QueryRequest) {
				request.Select.Project = []warehouse.QueryProjection{{Column: "no_such_column"}}
			}, warehouse.RefusalUnknownColumn},
			{"digest_mismatch", func(_ *testing.T, request *warehouse.QueryRequest) {
				request.Relations[0].SHA256 = strings.Repeat("0", 64)
			}, warehouse.RefusalDigestMismatch},
			{"declared_kind_mismatch", func(_ *testing.T, request *warehouse.QueryRequest) {
				request.Relations[0].Columns[0].Kind = warehouse.QueryKindText
			}, warehouse.RefusalSchemaMismatch},
			{"declared_scale_mismatch", func(_ *testing.T, request *warehouse.QueryRequest) {
				request.Relations[0].Columns[1].Scale = 2
			}, warehouse.RefusalSchemaMismatch},
			{"declared_precision_mismatch", func(_ *testing.T, request *warehouse.QueryRequest) {
				request.Relations[0].Columns[1].Precision = 9
			}, warehouse.RefusalSchemaMismatch},
			{"non_nullable_null", func(_ *testing.T, request *warehouse.QueryRequest) {
				request.Relations[0].Columns[10].Nullable = false
			}, warehouse.RefusalSchemaMismatch},
			{"output_outside_admitted_directory", func(_ *testing.T, request *warehouse.QueryRequest) {
				request.Select.Output = &warehouse.QueryOutput{Path: filepath.Join(fixture.outside, "written.parquet")}
				request.AdmittedDirectories = []string{fixture.outside}
			}, warehouse.RefusalUnadmittedPath},
			{"existing_output_is_not_overwritten", func(_ *testing.T, request *warehouse.QueryRequest) {
				request.Select.Output = &warehouse.QueryOutput{Path: fixture.source}
			}, warehouse.RefusalUnadmittedPath},
		}
		for _, testCase := range childRefusals {
			t.Run("child_refuses_"+testCase.name, func(t *testing.T) {
				document := fixture.childDocument(t, func(request *warehouse.QueryRequest) {
					request.RequestID = "child-refusal"
					request.Select.Limit = 1
					testCase.mutate(t, request)
				})
				run := fixture.runChildDocument(t, string(document), fixture.defaultLimits())
				if run.exitCode == 0 {
					t.Fatalf("child accepted a prohibited request; stdout=%q", run.stdout)
				}
				refusal := run.refusal(t)
				if refusal.Code != testCase.code {
					t.Fatalf("child refusal code = %q, want %q: %s", refusal.Code, testCase.code, refusal.Reason)
				}
				if refusal.Origin != warehouse.RefusalOriginChild {
					t.Fatalf("child refusal origin = %q, want child", refusal.Origin)
				}
			})
		}

		// A FIFO is the causal form of "opened nothing": had the child opened
		// the admitted path for reading, this call would block instead of
		// returning a refusal.
		t.Run("fifo_path", func(t *testing.T) {
			fifo := filepath.Join(fixture.probes, "fifo.parquet")
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatalf("create the FIFO input for the isolation case: %v", err)
			}
			document := fixture.childDocument(t, func(request *warehouse.QueryRequest) {
				request.Relations[0].Path = fifo
				request.AdmittedDirectories = []string{fixture.admitted, fixture.probes}
			})
			started := time.Now()
			run := fixture.runChildDocument(t, string(document), fixture.defaultLimits())
			if run.exitCode == 0 {
				t.Fatal("child accepted a FIFO relation")
			}
			if elapsed := time.Since(started); elapsed > 10*time.Second {
				t.Fatalf("child blocked for %s on a FIFO, so it opened a non-regular input", elapsed)
			}
			if code := run.refusal(t).Code; code != warehouse.RefusalUnadmittedPath {
				t.Fatalf("FIFO refusal code = %q, want %q", code, warehouse.RefusalUnadmittedPath)
			}
		})

		fixture.expectScratchEmpty(t)
		fixture.expectDurableStateUnchanged(t)
	})

	t.Run("symlinked_components_inside_admitted_roots_are_refused", func(t *testing.T) {
		layout := fixture.escapingExports(t)
		child := fixture.newChild(t, fixture.defaultLimits())

		// A relation whose directory component is a pre-existing symlink inside
		// an admitted root. The bytes behind the link exist and carry the admitted
		// identity, so the traversed link is the only reason to refuse it: had the
		// engine opened it, it would have read outside the admitted roots.
		escaped := fixture.sourceRequest()
		escaped.AdmittedDirectories = []string{fixture.probes}
		escaped.Relations[0].Path = layout.relation
		escaped.Select.Limit = 1
		response, err := child.Execute(t.Context(), escaped)
		if err == nil {
			t.Fatalf("the supervisor admitted a relation that resolves outside its admitted roots and returned %d rows", len(response.Rows))
		}
		refusal := requireRefusal(t, err)
		if refusal.Code != warehouse.RefusalUnadmittedPath {
			t.Fatalf("escaped relation refusal code = %q, want %q: %s", refusal.Code, warehouse.RefusalUnadmittedPath, refusal.Reason)
		}
		if refusal.Origin != warehouse.RefusalOriginSupervisor {
			t.Fatalf("escaped relation refusal origin = %q, want the supervisor", refusal.Origin)
		}
		if refusal.Child != nil {
			t.Fatal("a relation that resolves outside the admitted roots reached a child process")
		}
		if len(response.Rows) != 0 {
			t.Fatalf("refused escaped relation returned %d rows", len(response.Rows))
		}
		fixture.expectScratchEmpty(t)

		// The child refuses the same class on its own, without the supervisor.
		document := fixture.childDocument(t, func(request *warehouse.QueryRequest) {
			request.AdmittedDirectories = []string{fixture.probes}
			request.Relations[0].Path = layout.relation
			request.Select.Limit = 1
		})
		run := fixture.runChildDocument(t, string(document), fixture.defaultLimits())
		if run.exitCode == 0 {
			t.Fatalf("the child read a relation that resolves outside its admitted directories: %s", run.stdout)
		}
		childRefusal := run.refusal(t)
		if childRefusal.Code != warehouse.RefusalUnadmittedPath || childRefusal.Origin != warehouse.RefusalOriginChild {
			t.Fatalf("child-origin escaped relation refusal = %q/%q, want %q/child: %s",
				childRefusal.Code, childRefusal.Origin, warehouse.RefusalUnadmittedPath, childRefusal.Reason)
		}

		// A declared admitted directory that is itself a symlink is refused by
		// the child's own anchor check, before any component below it is used.
		symlinked := fixture.childDocument(t, func(request *warehouse.QueryRequest) {
			request.AdmittedDirectories = []string{layout.link}
			request.Relations[0].Path = layout.relation
			request.Select.Limit = 1
		})
		run = fixture.runChildDocument(t, string(symlinked), fixture.defaultLimits())
		if run.exitCode == 0 {
			t.Fatalf("the child admitted a declared admitted directory that is a symlink: %s", run.stdout)
		}
		if code := run.refusal(t).Code; code != warehouse.RefusalUnadmittedPath {
			t.Fatalf("symlinked admitted directory refusal code = %q, want %q", code, warehouse.RefusalUnadmittedPath)
		}

		// An output path whose grandparent is a symlink: refused, and nothing is
		// written outside the admitted roots.
		written := fixture.sourceRequest()
		written.AdmittedDirectories = []string{fixture.probes}
		written.Relations[0].Path = layout.nested
		written.Select.Output = &warehouse.QueryOutput{Path: layout.output}
		written.Select.Limit = 1
		response, err = child.Execute(t.Context(), written)
		if err == nil {
			t.Fatalf("the supervisor admitted an output that resolves outside its admitted roots and returned %d rows", len(response.Rows))
		}
		refusal = requireRefusal(t, err)
		if refusal.Code != warehouse.RefusalUnadmittedPath || refusal.Origin != warehouse.RefusalOriginSupervisor || refusal.Child != nil {
			t.Fatalf("escaped output refusal = %q/%q child=%v, want %q/supervisor and no child: %s",
				refusal.Code, refusal.Origin, refusal.Child, warehouse.RefusalUnadmittedPath, refusal.Reason)
		}
		if _, statErr := os.Lstat(layout.output); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("the refused attempt left a segment outside the admitted roots at %s", layout.output)
		}
		fixture.expectScratchEmpty(t)
		fixture.expectDurableStateUnchanged(t)

		// The child refuses that output on its own and materializes nothing.
		childOutput := fixture.childDocument(t, func(request *warehouse.QueryRequest) {
			request.AdmittedDirectories = []string{fixture.probes}
			request.Relations[0].Path = layout.nested
			request.Select.Output = &warehouse.QueryOutput{Path: layout.output}
			request.Select.Limit = 1
		})
		run = fixture.runChildDocument(t, string(childOutput), fixture.defaultLimits())
		if run.exitCode == 0 {
			t.Fatalf("the child materialized a segment outside its admitted directories: %s", run.stdout)
		}
		if code := run.refusal(t).Code; code != warehouse.RefusalUnadmittedPath {
			t.Fatalf("child-origin escaped output refusal code = %q, want %q", code, warehouse.RefusalUnadmittedPath)
		}
		if _, statErr := os.Lstat(layout.output); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("the child left a segment outside its admitted directories at %s", layout.output)
		}

		// An ordinary nested directory inside the admitted root is still accepted
		// end to end, so the component walk refuses only what leaves the roots.
		nested := fixture.sourceRequest()
		nested.AdmittedDirectories = []string{fixture.probes}
		nested.Relations[0].Path = layout.nested
		response, err = child.Execute(t.Context(), nested)
		if err != nil {
			t.Fatalf("an ordinary nested directory inside the admitted root was refused: %v", err)
		}
		expected := fixture.rowsAscendingByID()
		if !reflect.DeepEqual(response.Rows, expected) {
			t.Fatalf("nested-directory relation returned %s, want %s", describeRows(response.Rows), describeRows(expected))
		}
		fixture.expectScratchEmpty(t)
		fixture.expectDurableStateUnchanged(t)
	})

	t.Run("declared_bounds_beyond_child_capability_are_refused", func(t *testing.T) {
		limits := fixture.defaultLimits()
		limits.MemoryBytes = 32 << 20
		limits.TempBytes = 16 << 20
		child := fixture.newChild(t, limits)

		for name, mutate := range map[string]func(request *warehouse.QueryRequest){
			"memory_bound": func(request *warehouse.QueryRequest) { request.Bounds.MemoryBytes = 64 << 20 },
			"temp_bound":   func(request *warehouse.QueryRequest) { request.Bounds.TempBytes = 128 << 20 },
			"row_bound":    func(request *warehouse.QueryRequest) { request.Bounds.Rows = 1 << 40 },
			"byte_bound":   func(request *warehouse.QueryRequest) { request.Bounds.Bytes = 1 << 40 },
		} {
			t.Run("supervisor_refuses_"+name, func(t *testing.T) {
				request := fixture.sourceRequest()
				request.Select.Limit = 1
				mutate(&request)
				response, err := child.Execute(t.Context(), request)
				if err == nil {
					t.Fatalf("supervisor accepted an oversized %s and returned %d rows", name, len(response.Rows))
				}
				refusal := requireRefusal(t, err)
				if refusal.Code != warehouse.RefusalBoundExceeded {
					t.Fatalf("oversized %s refusal code = %q, want %q: %s", name, refusal.Code, warehouse.RefusalBoundExceeded, refusal.Reason)
				}
				if refusal.Child != nil {
					t.Fatalf("oversized %s reached a child process instead of being refused before execution", name)
				}
				fixture.expectScratchEmpty(t)
			})
		}

		// A request the supervisor accepts still carries its declared bounds to
		// the child, which refuses a value beyond its own hard capability
		// before executing anything.
		for name, mutate := range map[string]func(request *warehouse.QueryRequest){
			"memory_bound": func(request *warehouse.QueryRequest) { request.Bounds.MemoryBytes = 1 << 40 },
			"temp_bound":   func(request *warehouse.QueryRequest) { request.Bounds.TempBytes = 1 << 40 },
			"row_bound":    func(request *warehouse.QueryRequest) { request.Bounds.Rows = 1 << 40 },
		} {
			t.Run("child_refuses_"+name, func(t *testing.T) {
				document := fixture.childDocument(t, func(request *warehouse.QueryRequest) {
					request.Select.Limit = 1
					mutate(request)
				})
				run := fixture.runChildDocument(t, string(document), fixture.maximumLimits())
				if run.exitCode == 0 {
					t.Fatalf("child accepted an oversized %s", name)
				}
				refusal := run.refusal(t)
				if refusal.Code != warehouse.RefusalBoundExceeded {
					t.Fatalf("child %s refusal code = %q, want %q", name, refusal.Code, warehouse.RefusalBoundExceeded)
				}
				if refusal.Origin != warehouse.RefusalOriginChild {
					t.Fatalf("child %s refusal origin = %q, want child", name, refusal.Origin)
				}
			})
		}

		// An absent or non-positive bound is refused by the codec before any
		// process exists.
		codecCases := map[string]string{
			"absent_bound":  `{"protocol":"pm.query/1","request_id":"r1","bounds":{"relations":"1","columns":"1","rows":"1","bytes":"1","temp_bytes":"1","memory_bytes":"1"},"relations":[],"select":{"from":"orders","project":[{"column":"id"}],"limit":"1"}}`,
			"zero_bound":    `{"protocol":"pm.query/1","request_id":"r1","bounds":{"relations":"1","columns":"1","rows":"0","bytes":"1","temp_bytes":"1","memory_bytes":"1","wall_millis":"1"},"relations":[],"select":{"from":"orders","project":[{"column":"id"}],"limit":"1"}}`,
			"numeric_bound": `{"protocol":"pm.query/1","request_id":"r1","bounds":{"relations":1,"columns":"1","rows":"1","bytes":"1","temp_bytes":"1","memory_bytes":"1","wall_millis":"1"},"relations":[],"select":{"from":"orders","project":[{"column":"id"}],"limit":"1"}}`,
		}
		for name, document := range codecCases {
			t.Run("codec_refuses_"+name, func(t *testing.T) {
				if _, err := warehouse.DecodeQueryRequest([]byte(document)); err == nil {
					t.Fatalf("parent codec accepted a request with an %s: %s", name, document)
				}
			})
		}
		fixture.expectDurableStateUnchanged(t)
	})

	t.Run("wrapped_wall_time_bounds_are_refused_before_conversion", func(t *testing.T) {
		// The wire form carries milliseconds, so a decoded value is range-checked
		// before the nanosecond multiplication wraps it into the accepted range.
		documents := map[string]string{
			"wrapped_multiple": fmt.Sprintf(boundsProbeDocument, isolationWrappedWallMillis),
			"wrapped_minimum":  fmt.Sprintf(boundsProbeDocument, "-9223372036854775808"),
			"over_capability":  fmt.Sprintf(boundsProbeDocument, "300001"),
		}
		reasons := map[string]string{}
		for name, document := range documents {
			t.Run("codec_refuses_"+name, func(t *testing.T) {
				_, err := warehouse.DecodeQueryRequest([]byte(document))
				if err == nil {
					t.Fatalf("the parent codec accepted a %s wall-time bound: %s", name, document)
				}
				reasons[name] = requireRefusal(t, err).Reason
			})
		}
		// The wrapped bound is refused as the documented capability refusal, not
		// as a wrap-induced report about a non-positive value.
		if reasons["wrapped_multiple"] == "" || reasons["wrapped_multiple"] != reasons["over_capability"] {
			t.Fatalf("a wrapped wall-time bound was refused as %q while a value above the capability was refused as %q",
				reasons["wrapped_multiple"], reasons["over_capability"])
		}
		t.Run("child_refuses_wrapped_multiple", func(t *testing.T) {
			run := fixture.runChildDocument(t, documents["wrapped_multiple"], fixture.defaultLimits())
			if run.exitCode == 0 {
				t.Fatalf("the child executed a request whose wrapped wall-time bound decoded to exactly one second: %s", run.stdout)
			}
			refusal := run.refusal(t)
			if refusal.Code != warehouse.RefusalBoundExceeded || refusal.Origin != warehouse.RefusalOriginChild {
				t.Fatalf("child refusal for a wrapped wall-time bound = %q/%q, want %q/child: %s",
					refusal.Code, refusal.Origin, warehouse.RefusalBoundExceeded, refusal.Reason)
			}
		})
	})

	t.Run("over_bound_segment_removal_failure_is_surfaced", func(t *testing.T) {
		// Removing an over-bound segment fails while the directory that holds it
		// refuses writes: the child creates the segment first, and the case closes
		// the admitted directory's write permission as soon as the segment exists,
		// while the child still holds that file open for writing and before it
		// removes it. The refusal must then report the removal failure instead of a
		// bound refusal that hides a file left at the admitted output path.
		output := filepath.Join(fixture.admitted, "over-bound.parquet")
		restore := func() {
			_ = os.Chmod(fixture.admitted, 0o700)
			_ = os.Remove(output)
		}
		t.Cleanup(restore)
		if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the over-bound output path already exists: %v", err)
		}
		request := fixture.overBoundEncodeRequest(output)
		if int64(len(request.Encode.Rows))*isolationOverBoundValueBytes <= request.Bounds.Bytes {
			t.Fatal("the over-bound fixture declares a byte bound its own payload cannot exceed")
		}
		child := fixture.newChild(t, fixture.defaultLimits())
		type outcome struct {
			response warehouse.QueryResponse
			err      error
		}
		results := make(chan outcome, 1)
		go func() {
			response, err := child.Execute(t.Context(), request)
			results <- outcome{response: response, err: err}
		}()
		awaitRegularFile(t, output, 60*time.Second)
		if err := os.Chmod(fixture.admitted, 0o500); err != nil {
			t.Fatalf("close the admitted directory's write permission: %v", err)
		}
		var result outcome
		select {
		case result = <-results:
		case <-time.After(60 * time.Second):
			t.Fatal("the supervisor did not return after the over-bound removal failed")
		}
		if result.err == nil {
			t.Fatalf("an over-bound segment whose removal failed returned %d rows instead of a refusal", len(result.response.Rows))
		}
		refusal := requireRefusal(t, result.err)
		if refusal.Code != warehouse.RefusalOutputFailed {
			t.Fatalf("refusal code for an unremovable over-bound segment = %q, want %q: %s",
				refusal.Code, warehouse.RefusalOutputFailed, refusal.Reason)
		}
		if _, err := os.Lstat(output); err != nil {
			t.Fatalf("the fixture did not leave the over-bound segment at the admitted output path: %v", err)
		}
		restore()
		fixture.expectScratchEmpty(t)
		fixture.expectDurableStateUnchanged(t)
	})

	t.Run("killed_child_preserves_source_manifest_and_effect_state", func(t *testing.T) {
		t.Run("supervisor_wall_time_kill", func(t *testing.T) {
			limits := fixture.defaultLimits()
			limits.WallTime = 5 * time.Millisecond
			child := fixture.newChild(t, limits)
			request := fixture.bulkRequest()
			request.Bounds.WallTime = 5 * time.Millisecond
			response, err := child.Execute(t.Context(), request)
			if err == nil {
				t.Fatalf("a five-millisecond wall time returned %d rows instead of killing the child", len(response.Rows))
			}
			refusal := requireRefusal(t, err)
			if refusal.Code != warehouse.RefusalChildTimeout {
				t.Fatalf("wall-time refusal code = %q, want %q: %s", refusal.Code, warehouse.RefusalChildTimeout, refusal.Reason)
			}
			if refusal.Origin != warehouse.RefusalOriginSupervisor {
				t.Fatalf("wall-time refusal origin = %q, want supervisor", refusal.Origin)
			}
			if len(response.Rows) != 0 {
				t.Fatalf("killed child reported %d invented rows", len(response.Rows))
			}
			if refusal.Child != nil && refusal.Child.PID > 0 {
				if err := syscall.Kill(refusal.Child.PID, 0); err == nil {
					t.Fatalf("killed child %d is still running", refusal.Child.PID)
				}
			}
			fixture.expectScratchEmpty(t)
			fixture.expectDurableStateUnchanged(t)
		})

		t.Run("child_declared_wall_time_refusal", func(t *testing.T) {
			child := fixture.newChild(t, fixture.defaultLimits())
			request := fixture.bulkRequest()
			request.Bounds.WallTime = 5 * time.Millisecond
			response, err := child.Execute(t.Context(), request)
			if err == nil {
				t.Fatalf("declared wall time returned %d rows instead of refusing", len(response.Rows))
			}
			refusal := requireRefusal(t, err)
			if refusal.Origin != warehouse.RefusalOriginChild {
				t.Fatalf("declared wall-time refusal origin = %q, want the child: the supervisor wall time was generous", refusal.Origin)
			}
			if refusal.Code != warehouse.RefusalBoundExceeded {
				t.Fatalf("declared wall-time refusal code = %q, want %q: %s", refusal.Code, warehouse.RefusalBoundExceeded, refusal.Reason)
			}
			if refusal.ExitCode != isolationChildRefusalStatus {
				t.Fatalf("an explicit child refusal reported exit status %d, want %d", refusal.ExitCode, isolationChildRefusalStatus)
			}
			if len(response.Rows) != 0 {
				t.Fatalf("deadline refusal reported %d invented rows", len(response.Rows))
			}
			fixture.expectScratchEmpty(t)
			fixture.expectDurableStateUnchanged(t)
		})

		t.Run("external_kill_of_live_child", func(t *testing.T) {
			diagnostics := &isolationDiagnostics{}
			child := fixture.newObservedChild(t, fixture.defaultLimits(), diagnostics)
			type outcome struct {
				response warehouse.QueryResponse
				err      error
			}
			results := make(chan outcome, 1)
			go func() {
				response, err := child.Execute(t.Context(), fixture.bulkRequest())
				results <- outcome{response: response, err: err}
			}()
			pid := diagnostics.waitForProcessID(t, 30*time.Second)
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
				t.Fatalf("kill the live child %d: %v", pid, err)
			}
			select {
			case result := <-results:
				if result.err == nil {
					t.Fatalf("an externally killed child returned %d rows", len(result.response.Rows))
				}
				refusal := requireRefusal(t, result.err)
				if refusal.Code != warehouse.RefusalChildKilled && refusal.Code != warehouse.RefusalChildExit {
					t.Fatalf("killed child refusal code = %q, want %q or %q: %s", refusal.Code, warehouse.RefusalChildKilled, warehouse.RefusalChildExit, refusal.Reason)
				}
				// A process killed by a signal has no exit status, and the refusal
				// reports the kill through its code instead of inventing one.
				if refusal.ExitCode != -1 {
					t.Fatalf("a killed child reported exit status %d, want -1", refusal.ExitCode)
				}
				if len(result.response.Rows) != 0 {
					t.Fatalf("externally killed child reported %d invented rows", len(result.response.Rows))
				}
				if refusal.Child == nil || refusal.Child.PID != pid {
					t.Fatalf("killed-child refusal did not name the killed process: %+v", refusal.Child)
				}
			case <-time.After(60 * time.Second):
				t.Fatal("supervisor did not return after its child was killed")
			}
			fixture.expectScratchEmpty(t)
			fixture.expectDurableStateUnchanged(t)
		})

		t.Run("caller_cancellation_kills_the_child", func(t *testing.T) {
			diagnostics := &isolationDiagnostics{}
			child := fixture.newObservedChild(t, fixture.defaultLimits(), diagnostics)
			ctx, cancel := context.WithCancel(t.Context())
			results := make(chan error, 1)
			go func() {
				_, err := child.Execute(ctx, fixture.bulkRequest())
				results <- err
			}()
			pid := diagnostics.waitForProcessID(t, 30*time.Second)
			cancel()
			select {
			case err := <-results:
				if err == nil {
					t.Fatal("cancelled execution returned success")
				}
				requireRefusal(t, err)
			case <-time.After(60 * time.Second):
				t.Fatal("supervisor did not return after cancellation")
			}
			if err := syscall.Kill(pid, 0); err == nil {
				t.Fatalf("cancelled child %d is still running", pid)
			}
			fixture.expectScratchEmpty(t)
			fixture.expectDurableStateUnchanged(t)
		})
	})

	t.Run("scratch_removal_failure_is_reported", func(t *testing.T) {
		// An attempt whose per-attempt scratch directory cannot be removed is
		// never reported as a clean outcome: that directory still holds the
		// child's engine spill, so the supervisor reports the removal failure.
		child := fixture.newChild(t, fixture.defaultLimits())
		type outcome struct {
			response warehouse.QueryResponse
			err      error
		}
		results := make(chan outcome, 1)
		go func() {
			response, err := child.Execute(t.Context(), fixture.sourceRequest())
			results <- outcome{response: response, err: err}
		}()
		scratch := fixture.awaitScratchEngine(t, 60*time.Second)
		displaced := fixture.displaceScratchRoot(t)
		var result outcome
		select {
		case result = <-results:
		case <-time.After(60 * time.Second):
			t.Fatal("the supervisor did not return while its scratch root was displaced")
		}
		remaining := filepath.Join(displaced, filepath.Base(scratch))
		if _, err := os.Lstat(remaining); err != nil {
			t.Fatalf("the case did not leave a per-attempt scratch directory on disk: %v", err)
		}
		if result.err == nil {
			t.Fatalf("an attempt whose scratch could not be removed returned %d rows as a clean outcome", len(result.response.Rows))
		}
		refusal := requireRefusal(t, result.err)
		if refusal.Code != warehouse.RefusalEnvironment || refusal.Origin != warehouse.RefusalOriginSupervisor {
			t.Fatalf("scratch removal refusal = %q/%q, want %q/supervisor: %s",
				refusal.Code, refusal.Origin, warehouse.RefusalEnvironment, refusal.Reason)
		}
		if len(result.response.Rows) != 0 {
			t.Fatalf("an undisposable attempt reported %d invented rows", len(result.response.Rows))
		}
	})

	t.Run("child_exit_status_is_reported", func(t *testing.T) {
		// A child stand-in that exits without a response document is the
		// supervisor's own observation: the refusal reports the status it saw, so
		// a caller can tell a startup failure from a refusal from a kill.
		for _, status := range []int{2, 7} {
			t.Run(fmt.Sprintf("exit_status_%d", status), func(t *testing.T) {
				child := fixture.newChildWithBinary(t, fixture.writeStubChild(t, status), fixture.defaultLimits())
				request := fixture.sourceRequest()
				request.Select.Limit = 1
				response, err := child.Execute(t.Context(), request)
				if err == nil {
					t.Fatalf("a child stand-in that exited %d returned %d rows", status, len(response.Rows))
				}
				refusal := requireRefusal(t, err)
				if refusal.Code != warehouse.RefusalChildExit || refusal.Origin != warehouse.RefusalOriginSupervisor {
					t.Fatalf("child stand-in refusal = %q/%q, want %q/supervisor: %s",
						refusal.Code, refusal.Origin, warehouse.RefusalChildExit, refusal.Reason)
				}
				if refusal.ExitCode != status {
					t.Fatalf("child stand-in exit status reported as %d, want %d", refusal.ExitCode, status)
				}
				if refusal.Child == nil || refusal.Child.PID <= 0 {
					t.Fatalf("child stand-in refusal did not name the process that exited: %+v", refusal.Child)
				}
				fixture.expectScratchEmpty(t)
			})
		}
	})

	t.Run("child_environment_and_descriptors_are_cleared", func(t *testing.T) {
		// Credential-shaped canaries are placed in the test process first: an
		// inheriting supervisor would surface them in the child observation.
		canaries := map[string]string{
			"PM_STACK_TEST_CONFIG":           `{"postgres":"postgres://canary@127.0.0.1/pm"}`,
			"PM_STORE_TEST_SECRETS_DIR":      "/canary/store/secrets",
			"AWS_SECRET_ACCESS_KEY":          "canary-object-store-secret",
			"AWS_ACCESS_KEY_ID":              "canary-object-store-key",
			"PGHOST":                         "canary.postgres.invalid",
			"DATABASE_URL":                   "postgres://canary@canary.postgres.invalid/pm",
			"DUCKDB_SECRET":                  "canary-duckdb-secret",
			"PM_SERVER_TOKEN":                "canary-server-token",
			"GOOGLE_APPLICATION_CREDENTIALS": "/canary/service-account.json",
		}
		for name, value := range canaries {
			t.Setenv(name, value)
		}

		// A non-exec descriptor held by this process must not reach the child
		// and must not be closed by the supervisor either.
		canaryPath := filepath.Join(fixture.probes, "descriptor-canary.bin")
		canaryBytes := bytes.Repeat([]byte{0x5a}, isolationCanaryDescriptorBytes)
		if err := os.WriteFile(canaryPath, canaryBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		canaryDescriptor, err := syscall.Open(canaryPath, syscall.O_RDONLY, 0)
		if err != nil {
			t.Fatalf("open a descriptor without close-on-exec: %v", err)
		}
		defer func() { _ = syscall.Close(canaryDescriptor) }()

		child := fixture.newChild(t, fixture.defaultLimits())
		request := fixture.sourceRequest()
		request.Select.Limit = 1
		response, err := child.Execute(t.Context(), request)
		if err != nil {
			t.Fatalf("execute request while observing the child: %v", err)
		}
		if len(response.Rows) != 1 {
			t.Fatalf("observed run returned %d rows, want 1", len(response.Rows))
		}
		report := response.Child
		if report == nil {
			t.Fatal("successful response carried no child observation")
		}
		if report.PID <= 0 {
			t.Fatalf("child observation reported pid %d", report.PID)
		}
		for name := range canaries {
			if containsString(report.Environment, name) {
				t.Fatalf("child inherited credential-bearing variable %q", name)
			}
		}
		for _, observed := range report.Environment {
			switch {
			case strings.Contains(observed, "CANARY"), strings.Contains(observed, "SECRET"),
				strings.Contains(observed, "TOKEN"), strings.Contains(observed, "CREDENTIAL"),
				strings.HasPrefix(observed, "PM_STACK"), strings.HasPrefix(observed, "PM_STORE"),
				strings.HasPrefix(observed, "AWS_"), strings.HasPrefix(observed, "PG"),
				strings.HasPrefix(observed, "DATABASE"), strings.HasPrefix(observed, "DUCKDB"):
				t.Fatalf("child environment exposes credential-bearing variable %q", observed)
			}
		}
		for _, required := range []string{"HOME", "PATH", "TMPDIR"} {
			if !containsString(report.Environment, required) {
				t.Fatalf("child environment is missing %q: %v", required, report.Environment)
			}
		}
		if len(report.UnclosedDescriptors) != 0 {
			t.Fatalf("child could not close declared inherited descriptors: %v", report.UnclosedDescriptors)
		}
		if !containsInt(report.InheritedDescriptors, canaryDescriptor) {
			t.Fatalf("the supervisor did not declare the inherited descriptor %d: %v", canaryDescriptor, report.InheritedDescriptors)
		}
		if !containsInt(report.ClosedDescriptors, canaryDescriptor) {
			t.Fatalf("the child did not close the inherited descriptor %d: %v", canaryDescriptor, report.ClosedDescriptors)
		}
		for _, descriptor := range report.RemainingDescriptors {
			if containsInt(report.InheritedDescriptors, descriptor.Descriptor) {
				t.Fatalf("child still holds inherited descriptor %d (%s)", descriptor.Descriptor, descriptor.Kind)
			}
		}
		if report.TemporaryDirectory == "" {
			t.Fatal("child did not report its temporary directory")
		}
		if !withinRoot(fixture.scratchRoot, report.TemporaryDirectory) {
			t.Fatalf("child temporary directory %q is outside the owner-only scratch root %q", report.TemporaryDirectory, fixture.scratchRoot)
		}
		if err := syscall.Fstat(canaryDescriptor, &syscall.Stat_t{}); err != nil {
			t.Fatalf("supervisor closed the caller's own descriptor %d: %v", canaryDescriptor, err)
		}
		t.Run("undeclared_child_refuses_to_run", func(t *testing.T) {
			// Without the supervisor's descriptor declaration the child cannot
			// know which inherited descriptors to close, so it must refuse to
			// run rather than execute with a possibly credential-bearing
			// descriptor open.
			temporary := t.TempDir()
			command := exec.Command(fixture.childPath)
			command.Env = []string{
				"HOME=" + temporary,
				"TMPDIR=" + temporary,
				"PATH=/usr/bin:/bin",
			}
			command.Stdin = strings.NewReader("{}")
			var stdout, stderr bytes.Buffer
			command.Stdout = &stdout
			command.Stderr = &stderr
			if err := command.Run(); err == nil {
				t.Fatal("the child ran without the inherited descriptor declaration")
			}
			if stdout.Len() != 0 {
				t.Fatalf("the child answered without the descriptor declaration: %q", stdout.String())
			}
		})
		fixture.expectScratchEmpty(t)
		if _, err := child.Execute(t.Context(), request); err != nil {
			t.Fatalf("second execute on the same supervisor: %v", err)
		}
	})

	fixture.expectScratchEmpty(t)
	fixture.expectDurableStateUnchanged(t)
}

// queryIsolationFixture owns the sealed observation surface of the acceptance
// case.
type queryIsolationFixture struct {
	root         string
	admitted     string
	probes       string
	outside      string
	state        string
	scratchRoot  string
	childPath    string
	source       string
	sourceDigest string
	sourceBytes  int64
	bulk         string
	bulkDigest   string
	bulkBytes    int64
	columns      []warehouse.QueryColumn
	rows         [][]warehouse.QueryValue
	manifestPath string
	effectsPath  string
	sealed       isolationSeal
}

type isolationSeal struct {
	manifest  string
	effects   string
	inventory string
}

func newQueryIsolationFixture(t *testing.T) *queryIsolationFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &queryIsolationFixture{root: root}
	for name, target := range map[string]*string{
		"admitted": &fixture.admitted,
		"probes":   &fixture.probes,
		"outside":  &fixture.outside,
		"state":    &fixture.state,
		"scratch":  &fixture.scratchRoot,
	} {
		directory := filepath.Join(root, name)
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatalf("create fixture directory %s: %v", name, err)
		}
		*target = directory
	}
	fixture.childPath = buildRestrictedQueryChild(t)
	fixture.columns, fixture.rows = isolationSourceRows(t)
	fixture.source = filepath.Join(fixture.admitted, "source.parquet")
	fixture.bulk = filepath.Join(fixture.admitted, "bulk.parquet")

	// The admitted Parquet inputs are produced by the restricted child itself:
	// the encode operation writes an immutable segment and the query operation
	// reads exactly those bytes back. No binary fixture is committed and no
	// other writer touches the admitted directory.
	child := fixture.newChild(t, fixture.maximumLimits())
	encode := warehouse.QueryRequest{
		RequestID:           "fixture-source",
		AdmittedDirectories: []string{fixture.admitted},
		Bounds: warehouse.QueryBounds{
			Relations:   2,
			Columns:     32,
			Rows:        int64(len(fixture.rows)),
			Bytes:       isolationByteBound,
			TempBytes:   64 << 20,
			MemoryBytes: 128 << 20,
			WallTime:    time.Minute,
		},
		Encode: &warehouse.QueryEncode{
			Columns: fixture.columnCopy(),
			Rows:    fixture.rows,
			Output:  warehouse.QueryOutput{Path: fixture.source},
		},
	}
	response, err := child.Execute(t.Context(), encode)
	if err != nil {
		t.Fatalf("materialize the admitted source segment: %v", err)
	}
	if response.Output == nil || response.Output.Path != fixture.source {
		t.Fatalf("source segment response did not describe the admitted output: %+v", response.Output)
	}
	fixture.sourceDigest, fixture.sourceBytes = digestFile(t, fixture.source)
	if response.Output.SHA256 != fixture.sourceDigest || response.Output.Bytes != fixture.sourceBytes {
		t.Fatalf("source segment descriptor disagrees with the admitted bytes: %+v", response.Output)
	}

	bulkRows := make([][]warehouse.QueryValue, isolationBulkRows)
	for index := range bulkRows {
		bulkRows[index] = []warehouse.QueryValue{warehouse.Int64QueryValue(int64(index))}
	}
	bulkEncode := warehouse.QueryRequest{
		RequestID:           "fixture-bulk",
		AdmittedDirectories: []string{fixture.admitted},
		Bounds: warehouse.QueryBounds{
			Relations:   2,
			Columns:     32,
			Rows:        isolationBulkRows,
			Bytes:       isolationByteBound,
			TempBytes:   64 << 20,
			MemoryBytes: 128 << 20,
			WallTime:    2 * time.Minute,
		},
		Encode: &warehouse.QueryEncode{
			Columns: []warehouse.QueryColumn{{Name: "position", Kind: warehouse.QueryKindInt64, Nullable: false}},
			Rows:    bulkRows,
			Output:  warehouse.QueryOutput{Path: fixture.bulk},
		},
	}
	if _, err := child.Execute(t.Context(), bulkEncode); err != nil {
		t.Fatalf("materialize the bulk admitted segment: %v", err)
	}
	fixture.bulkDigest, fixture.bulkBytes = digestFile(t, fixture.bulk)
	if fixture.bulkBytes == 0 {
		t.Fatal("bulk segment is empty")
	}

	fixture.sealDurableState(t)
	fixture.sealed = fixture.captureSeal(t)
	fixture.expectScratchEmpty(t)
	return fixture
}

// isolationByteBound is the largest declared byte bound the fixture's
// supervisor admits: the supervisor reads a declared bound plus the document
// envelope, both bounded by the protocol codec's own document capability.
const isolationByteBound = warehouseMaximumResponseCap - warehouseResponseEnvelopeSlack

// isolationOverBoundRows and isolationOverBoundValueBytes size one encoded
// payload the wire form can still carry as a single request document: the
// payload is incompressible, so the materialized segment keeps the encoded size
// and the writer holds the admitted output file open while the case observes
// it.
const (
	isolationOverBoundRows       = 5
	isolationOverBoundValueBytes = 1 << 20
	isolationOverBoundByteBound  = 1 << 20
)

// isolationWrappedWallMillis wraps to exactly one second when it is multiplied
// into a duration: 2^58 + 1000 milliseconds is 2^58 * 10^6 nanoseconds plus one
// second, and 2^58 * 10^6 is a whole multiple of 2^64 nanoseconds.
const isolationWrappedWallMillis = "288230376151712744"

// boundsProbeDocument is one complete request document whose only varied member
// is the declared wall-time bound in milliseconds. It is rendered as text so a
// case can present a value the typed request cannot express; its declared
// directory is one real directory on every platform so nothing but the bound
// can refuse the document.
const boundsProbeDocument = `{"protocol":"pm.query/1","request_id":"wall-time-probe",` +
	`"admitted_directories":["/usr"],` +
	`"bounds":{"relations":"1","columns":"1","rows":"1","bytes":"1","temp_bytes":"1","memory_bytes":"1","wall_millis":"%s"},` +
	`"relations":[],"select":{"from":"orders","project":[{"column":"id"}],"limit":"1"}}`

func (fixture *queryIsolationFixture) overBoundEncodeRequest(output string) warehouse.QueryRequest {
	// The payload is drawn from a fixed generator rather than a repeating
	// pattern: a compressible payload would materialize a segment that fits the
	// declared bound and never exercise the over-bound removal.
	random := rand.New(rand.NewPCG(1, 2))
	rows := make([][]warehouse.QueryValue, isolationOverBoundRows)
	for index := range rows {
		payload := make([]byte, isolationOverBoundValueBytes)
		for position := range payload {
			payload[position] = byte(random.Uint32())
		}
		rows[index] = []warehouse.QueryValue{warehouse.BytesQueryValue(payload)}
	}
	return warehouse.QueryRequest{
		RequestID:           "isolation-over-bound",
		AdmittedDirectories: []string{fixture.admitted},
		Bounds: warehouse.QueryBounds{
			Relations:   1,
			Columns:     4,
			Rows:        isolationOverBoundRows,
			Bytes:       isolationOverBoundByteBound,
			TempBytes:   64 << 20,
			MemoryBytes: 128 << 20,
			WallTime:    time.Minute,
		},
		Encode: &warehouse.QueryEncode{
			Columns: []warehouse.QueryColumn{{Name: "payload", Kind: warehouse.QueryKindBytea, Nullable: false}},
			Rows:    rows,
			Output:  warehouse.QueryOutput{Path: output},
		},
	}
}

// awaitRegularFile waits until one regular file exists at path, so a case can
// act while the child still holds that file open.
func awaitRegularFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
			return
		}
		time.Sleep(50 * time.Microsecond)
	}
	t.Fatalf("no writer presented a regular file at %s within %s", path, timeout)
}

func (fixture *queryIsolationFixture) defaultLimits() warehouse.ChildLimits {
	return warehouse.ChildLimits{
		MaxRequestBytes:     warehouse.MaxQueryDocumentBytes,
		MaxResponseBytes:    warehouseMaximumResponseCap,
		MaxDiagnosticsBytes: 64 << 10,
		MemoryBytes:         128 << 20,
		TempBytes:           64 << 20,
		WallTime:            time.Minute,
		StartupTimeout:      30 * time.Second,
	}
}

func (fixture *queryIsolationFixture) maximumLimits() warehouse.ChildLimits {
	limits := fixture.defaultLimits()
	limits.WallTime = 2 * time.Minute
	limits.MemoryBytes = 256 << 20
	return limits
}

func (fixture *queryIsolationFixture) newChild(t *testing.T, limits warehouse.ChildLimits) *warehouse.QueryChild {
	t.Helper()
	child, err := warehouse.NewQueryChild(warehouse.ChildConfig{
		BinaryPath:    fixture.childPath,
		ScratchRoot:   fixture.scratchRoot,
		AdmittedRoots: []string{fixture.admitted, fixture.probes},
		Limits:        limits,
	})
	if err != nil {
		t.Fatalf("construct the query child supervisor: %v", err)
	}
	return child
}

func (fixture *queryIsolationFixture) newObservedChild(t *testing.T, limits warehouse.ChildLimits, diagnostics *isolationDiagnostics) *warehouse.QueryChild {
	t.Helper()
	child, err := warehouse.NewQueryChild(warehouse.ChildConfig{
		BinaryPath:    fixture.childPath,
		ScratchRoot:   fixture.scratchRoot,
		AdmittedRoots: []string{fixture.admitted, fixture.probes},
		Limits:        limits,
		Diagnostics:   diagnostics,
	})
	if err != nil {
		t.Fatalf("construct the observed query child supervisor: %v", err)
	}
	return child
}

func (fixture *queryIsolationFixture) sourcePath() string { return fixture.source }

func (fixture *queryIsolationFixture) outsidePath() string {
	return filepath.Join(fixture.outside, "foreign.parquet")
}

// isolationExports is one pre-existing symbolic link inside an admitted root
// with real directories outside every admitted root behind it, plus an
// ordinary nested directory beside it. A declared path that is lexically inside
// the admitted root therefore resolves outside it, while the same component
// walk must still accept the ordinary nested directory.
type isolationExports struct {
	link     string
	nested   string
	target   string
	relation string
	output   string
}

func (fixture *queryIsolationFixture) escapingExports(t *testing.T) isolationExports {
	t.Helper()
	exports := filepath.Join(fixture.probes, "exports")
	for _, directory := range []string{exports, filepath.Join(exports, "nested"), filepath.Join(fixture.outside, "sub")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create the fixture directory %s: %v", directory, err)
		}
	}
	link := filepath.Join(exports, "link")
	switch info, err := os.Lstat(link); {
	case errors.Is(err, os.ErrNotExist):
		if err := os.Symlink(fixture.outside, link); err != nil {
			t.Fatalf("create the escaping symlink inside the admitted root: %v", err)
		}
	case err != nil:
		t.Fatalf("inspect the escaping symlink: %v", err)
	case info.Mode()&os.ModeSymlink == 0:
		t.Fatalf("the escaping fixture path %s is not a symlink", link)
	}
	layout := isolationExports{
		link:     link,
		nested:   filepath.Join(exports, "nested", "source.parquet"),
		target:   filepath.Join(fixture.outside, "sub", "source.parquet"),
		relation: filepath.Join(link, "sub", "source.parquet"),
		output:   filepath.Join(link, "sub", "written.parquet"),
	}
	source, err := os.ReadFile(fixture.source)
	if err != nil {
		t.Fatalf("read the admitted source segment: %v", err)
	}
	for _, destination := range []string{layout.nested, layout.target} {
		if err := os.WriteFile(destination, source, 0o600); err != nil {
			t.Fatalf("place the admitted bytes at %s: %v", destination, err)
		}
	}
	// The escaping relation names bytes that really exist behind the link and
	// carry the admitted identity, so the only reason to refuse it is the
	// traversed link itself.
	for _, path := range []string{layout.relation, layout.nested} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("the escaping fixture is not a readable regular file at %s: %v", path, err)
		}
		if digest := digestFileString(t, path); digest != fixture.sourceDigest {
			t.Fatalf("the escaping fixture at %s does not carry the admitted digest", path)
		}
	}
	if _, err := os.Lstat(layout.output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the escaping output fixture already exists at %s: %v", layout.output, err)
	}
	return layout
}

// awaitScratchEngine waits until the supervisor's per-attempt scratch directory
// exists and the child's private engine directories are inside it, so a case can
// change the scratch root while a live attempt still holds it and no later
// child step still has to create anything below it.
func (fixture *queryIsolationFixture) awaitScratchEngine(t *testing.T, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(fixture.scratchRoot)
		if err != nil {
			t.Fatalf("read the supervisor scratch root: %v", err)
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), "pm-query-") {
				continue
			}
			scratch := filepath.Join(fixture.scratchRoot, entry.Name())
			present := true
			for _, name := range []string{"duckdb-temp", "duckdb-secrets"} {
				if info, err := os.Lstat(filepath.Join(scratch, name)); err != nil || !info.IsDir() {
					present = false
				}
			}
			if present {
				return scratch
			}
		}
		time.Sleep(200 * time.Microsecond)
	}
	t.Fatalf("the supervisor never presented a live per-attempt scratch directory under %s", fixture.scratchRoot)
	return ""
}

// displaceScratchRoot replaces the supervisor's scratch root with one regular
// file, so removing the per-attempt scratch directory below it fails with a
// real filesystem error instead of succeeding.
func (fixture *queryIsolationFixture) displaceScratchRoot(t *testing.T) string {
	t.Helper()
	displaced := fixture.scratchRoot + ".displaced"
	if err := os.Rename(fixture.scratchRoot, displaced); err != nil {
		t.Fatalf("displace the supervisor scratch root: %v", err)
	}
	if err := os.WriteFile(fixture.scratchRoot, []byte("displaced\n"), 0o600); err != nil {
		t.Fatalf("place a regular file at the scratch root: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Remove(fixture.scratchRoot)
		_ = os.RemoveAll(displaced)
		_ = os.Mkdir(fixture.scratchRoot, 0o700)
	})
	return displaced
}

// writeStubChild places one executable child stand-in that exits with the given
// status without writing a response document, so the supervisor's own reporting
// of an observed exit status is observable.
func (fixture *queryIsolationFixture) writeStubChild(t *testing.T, status int) string {
	t.Helper()
	path := filepath.Join(fixture.root, fmt.Sprintf("pm-query-stub-%d", status))
	script := fmt.Sprintf("#!/bin/sh\nexit %d\n", status)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write the child stand-in: %v", err)
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("the child stand-in is not an executable regular file: %v", err)
	}
	return path
}

func (fixture *queryIsolationFixture) newChildWithBinary(t *testing.T, binary string, limits warehouse.ChildLimits) *warehouse.QueryChild {
	t.Helper()
	child, err := warehouse.NewQueryChild(warehouse.ChildConfig{
		BinaryPath:    binary,
		ScratchRoot:   fixture.scratchRoot,
		AdmittedRoots: []string{fixture.admitted, fixture.probes},
		Limits:        limits,
	})
	if err != nil {
		t.Fatalf("construct a supervisor for the child stand-in: %v", err)
	}
	return child
}

// columnCopy hands out the declared schema without sharing its backing array:
// a negative subtest mutates the request it sends, and that must never corrupt
// the fixture for later subtests.
func (fixture *queryIsolationFixture) columnCopy() []warehouse.QueryColumn {
	return append([]warehouse.QueryColumn(nil), fixture.columns...)
}

func (fixture *queryIsolationFixture) relation() warehouse.QueryRelation {
	return warehouse.QueryRelation{
		Name:    isolationSourceRelation,
		Path:    fixture.source,
		SHA256:  fixture.sourceDigest,
		Bytes:   fixture.sourceBytes,
		Columns: fixture.columnCopy(),
	}
}

func (fixture *queryIsolationFixture) sourceRequest() warehouse.QueryRequest {
	projections := make([]warehouse.QueryProjection, 0, len(fixture.columns))
	for _, column := range fixture.columns {
		projections = append(projections, warehouse.QueryProjection{Column: column.Name})
	}
	return warehouse.QueryRequest{
		RequestID:           "isolation-source",
		AdmittedDirectories: []string{fixture.admitted},
		Bounds: warehouse.QueryBounds{
			Relations:   2,
			Columns:     32,
			Rows:        int64(len(fixture.rows)),
			Bytes:       isolationByteBound,
			TempBytes:   32 << 20,
			MemoryBytes: 128 << 20,
			WallTime:    30 * time.Second,
		},
		Relations: []warehouse.QueryRelation{fixture.relation()},
		Select: &warehouse.QuerySelect{
			From:    isolationSourceRelation,
			Project: projections,
			Order:   []warehouse.QueryOrder{{Column: "id"}},
			Limit:   int64(len(fixture.rows)),
		},
	}
}

func (fixture *queryIsolationFixture) bulkRequest() warehouse.QueryRequest {
	return warehouse.QueryRequest{
		RequestID:           "isolation-bulk",
		AdmittedDirectories: []string{fixture.admitted},
		Bounds: warehouse.QueryBounds{
			Relations:   1,
			Columns:     4,
			Rows:        isolationBulkRows,
			Bytes:       isolationByteBound,
			TempBytes:   32 << 20,
			MemoryBytes: 128 << 20,
			WallTime:    time.Minute,
		},
		Relations: []warehouse.QueryRelation{{
			Name:    "positions",
			Path:    fixture.bulk,
			SHA256:  fixture.bulkDigest,
			Bytes:   fixture.bulkBytes,
			Columns: []warehouse.QueryColumn{{Name: "position", Kind: warehouse.QueryKindInt64, Nullable: false}},
		}},
		Select: &warehouse.QuerySelect{
			From:    "positions",
			Project: []warehouse.QueryProjection{{Column: "position"}},
			Order:   []warehouse.QueryOrder{{Column: "position", Descending: true}},
			Limit:   isolationBulkRows,
		},
	}
}

// childDocument renders one request document as the child receives it. The
// supervisor is deliberately bypassed so that the child's own admission and
// protocol guards are observable.
func (fixture *queryIsolationFixture) childDocument(t *testing.T, mutate func(request *warehouse.QueryRequest)) []byte {
	t.Helper()
	request := fixture.sourceRequest()
	if mutate != nil {
		mutate(&request)
	}
	// The document is rendered without the supervisor's own admission checks:
	// the child's independent guards must observe it. The package's exported
	// encoder validates the typed request first, and it refuses exactly the
	// classes this case presents (an undeclared relation, an unknown column, a
	// digest the admitted bytes do not have, and a bound beyond the child's own
	// capability), so the case renders the child's own wire form itself.
	document, err := isolationWireRequest(request)
	if err != nil {
		t.Fatalf("render child request document: %v", err)
	}
	return document
}

// The wire documents below are the isolation case's own rendering of the
// child's request form. The child decodes them with its own strict decoder and
// refuses any member that drifts from the form, so a mistake here fails the
// case instead of quietly presenting a different document.
type isolationColumnDocument struct {
	Name      string  `json:"name"`
	Kind      string  `json:"kind"`
	Precision *string `json:"precision,omitempty"`
	Scale     *string `json:"scale,omitempty"`
	Nullable  bool    `json:"nullable"`
}

type isolationRelationDocument struct {
	Name    string                    `json:"name"`
	Path    string                    `json:"path"`
	SHA256  string                    `json:"sha256"`
	Bytes   string                    `json:"bytes"`
	Columns []isolationColumnDocument `json:"columns"`
}

type isolationProjectionDocument struct {
	Column string `json:"column"`
	Alias  string `json:"alias,omitempty"`
}

type isolationPredicateDocument struct {
	Column   string                 `json:"column"`
	Operator string                 `json:"operator"`
	Value    *warehouse.QueryValue  `json:"value,omitempty"`
	Values   []warehouse.QueryValue `json:"values,omitempty"`
}

type isolationOrderDocument struct {
	Column    string `json:"column"`
	Direction string `json:"direction"`
}

type isolationOutputDocument struct {
	Path string `json:"path"`
}

type isolationSelectDocument struct {
	From       string                        `json:"from"`
	Project    []isolationProjectionDocument `json:"project"`
	Predicates []isolationPredicateDocument  `json:"predicates,omitempty"`
	Order      []isolationOrderDocument      `json:"order,omitempty"`
	Limit      string                        `json:"limit"`
	Output     *isolationOutputDocument      `json:"output,omitempty"`
}

type isolationBoundsDocument struct {
	Relations   string `json:"relations"`
	Columns     string `json:"columns"`
	Rows        string `json:"rows"`
	Bytes       string `json:"bytes"`
	TempBytes   string `json:"temp_bytes"`
	MemoryBytes string `json:"memory_bytes"`
	WallMillis  string `json:"wall_millis"`
}

type isolationRequestDocument struct {
	Protocol            string                      `json:"protocol"`
	RequestID           string                      `json:"request_id"`
	AdmittedDirectories []string                    `json:"admitted_directories"`
	Bounds              isolationBoundsDocument     `json:"bounds"`
	Relations           []isolationRelationDocument `json:"relations"`
	Select              *isolationSelectDocument    `json:"select,omitempty"`
}

func isolationColumnDocumentFor(column warehouse.QueryColumn) isolationColumnDocument {
	document := isolationColumnDocument{Name: column.Name, Kind: string(column.Kind), Nullable: column.Nullable}
	if column.Kind == warehouse.QueryKindDecimal {
		precision := strconv.FormatInt(int64(column.Precision), 10)
		scale := strconv.FormatInt(int64(column.Scale), 10)
		document.Precision = &precision
		document.Scale = &scale
	}
	return document
}

// isolationWireRequest renders one typed request into the exact wire form the
// child reads. Only the typed select operation is rendered: the case presents
// read documents, and an unrendered operation is refused here rather than sent
// as a different document.
func isolationWireRequest(request warehouse.QueryRequest) ([]byte, error) {
	if request.Select == nil {
		return nil, errors.New("the isolation case renders one select document; an encode document needs its own rendering here")
	}
	document := isolationRequestDocument{
		Protocol:            warehouse.QueryProtocolVersion,
		RequestID:           request.RequestID,
		AdmittedDirectories: request.AdmittedDirectories,
		Bounds: isolationBoundsDocument{
			Relations:   strconv.FormatInt(int64(request.Bounds.Relations), 10),
			Columns:     strconv.FormatInt(int64(request.Bounds.Columns), 10),
			Rows:        strconv.FormatInt(request.Bounds.Rows, 10),
			Bytes:       strconv.FormatInt(request.Bounds.Bytes, 10),
			TempBytes:   strconv.FormatInt(request.Bounds.TempBytes, 10),
			MemoryBytes: strconv.FormatInt(request.Bounds.MemoryBytes, 10),
			WallMillis:  strconv.FormatInt(request.Bounds.WallTime.Milliseconds(), 10),
		},
		Relations: make([]isolationRelationDocument, 0, len(request.Relations)),
	}
	if document.AdmittedDirectories == nil {
		document.AdmittedDirectories = []string{}
	}
	for _, relation := range request.Relations {
		entry := isolationRelationDocument{
			Name:    relation.Name,
			Path:    relation.Path,
			SHA256:  relation.SHA256,
			Bytes:   strconv.FormatInt(relation.Bytes, 10),
			Columns: make([]isolationColumnDocument, 0, len(relation.Columns)),
		}
		for _, column := range relation.Columns {
			entry.Columns = append(entry.Columns, isolationColumnDocumentFor(column))
		}
		document.Relations = append(document.Relations, entry)
	}
	selected := &isolationSelectDocument{
		From:    request.Select.From,
		Limit:   strconv.FormatInt(request.Select.Limit, 10),
		Project: make([]isolationProjectionDocument, 0, len(request.Select.Project)),
	}
	for _, projection := range request.Select.Project {
		selected.Project = append(selected.Project, isolationProjectionDocument{Column: projection.Column, Alias: projection.Alias})
	}
	for _, predicate := range request.Select.Predicates {
		entry := isolationPredicateDocument{Column: predicate.Column, Operator: string(predicate.Operator)}
		if predicate.Value.Present() {
			value := predicate.Value
			entry.Value = &value
		}
		entry.Values = append(entry.Values, predicate.Values...)
		selected.Predicates = append(selected.Predicates, entry)
	}
	for _, ordering := range request.Select.Order {
		direction := "asc"
		if ordering.Descending {
			direction = "desc"
		}
		selected.Order = append(selected.Order, isolationOrderDocument{Column: ordering.Column, Direction: direction})
	}
	if request.Select.Output != nil {
		selected.Output = &isolationOutputDocument{Path: request.Select.Output.Path}
	}
	document.Select = selected
	rendered, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode the child request document: %w", err)
	}
	if len(rendered) > warehouse.MaxQueryDocumentBytes {
		return nil, fmt.Errorf("the rendered request document exceeds the protocol byte bound of %d bytes", warehouse.MaxQueryDocumentBytes)
	}
	return rendered, nil
}

type isolationChildRun struct {
	stdout   string
	stderr   string
	exitCode int
}

// runChildDocument starts the real child binary directly, writes one bounded
// request document, and reports the raw process outcome.
func (fixture *queryIsolationFixture) runChildDocument(t *testing.T, document string, limits warehouse.ChildLimits) isolationChildRun {
	t.Helper()
	temporary := t.TempDir()
	command := exec.Command(fixture.childPath)
	command.Env = []string{
		"HOME=" + temporary,
		"PATH=/usr/bin:/bin",
		"TMPDIR=" + temporary,
		"LANG=C",
		"LC_ALL=C",
		"TZ=UTC",
		"PM_QUERY_CLOSE_DESCRIPTORS=",
	}
	command.Stdin = strings.NewReader(document)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	command.WaitDelay = 10 * time.Second
	if err := command.Start(); err != nil {
		t.Fatalf("start the restricted child directly: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			return isolationChildRun{stdout: stdout.String(), stderr: stderr.String()}
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("wait for the restricted child: %v", err)
		}
		return isolationChildRun{stdout: stdout.String(), stderr: stderr.String(), exitCode: exit.ExitCode()}
	case <-time.After(limits.WallTime + 30*time.Second):
		_ = command.Process.Kill()
		<-done
		t.Fatal("restricted child did not exit within its declared wall time")
		return isolationChildRun{}
	}
}

func (run isolationChildRun) refusal(t *testing.T) *warehouse.Refusal {
	t.Helper()
	if strings.TrimSpace(run.stdout) == "" {
		t.Fatalf("child produced no refusal document; stderr=%q", run.stderr)
	}
	_, err := warehouse.DecodeQueryResponse([]byte(run.stdout))
	if err == nil {
		t.Fatal("child refusal document decoded as a successful response")
	}
	refusal := requireRefusal(t, err)
	if refusal.Child == nil {
		t.Fatalf("child refusal %q carried no child observation; stderr=%q", refusal.Code, run.stderr)
	}
	return refusal
}

func (fixture *queryIsolationFixture) observeChildReport(t *testing.T, response warehouse.QueryResponse) {
	t.Helper()
	if response.Child == nil {
		t.Fatal("successful response carried no child observation")
	}
	if response.Child.PID <= 0 {
		t.Fatalf("child observation reported pid %d", response.Child.PID)
	}
	if len(response.Child.UnclosedDescriptors) != 0 {
		t.Fatalf("child could not close declared inherited descriptors: %v", response.Child.UnclosedDescriptors)
	}
	for _, descriptor := range response.Child.RemainingDescriptors {
		if containsInt(response.Child.InheritedDescriptors, descriptor.Descriptor) {
			t.Fatalf("child still holds inherited descriptor %d (%s)", descriptor.Descriptor, descriptor.Kind)
		}
	}
	if report := response.Child.TemporaryDirectory; !withinRoot(fixture.scratchRoot, report) {
		t.Fatalf("child temporary directory %q is outside the owner-only scratch root", report)
	}
}

// sealDurableState writes the source manifest and the effect record that a
// refused or killed child must leave untouched, and then makes their directory
// unwritable so that no accidental write can hide behind a retry.
func (fixture *queryIsolationFixture) sealDurableState(t *testing.T) {
	t.Helper()
	fixture.manifestPath = filepath.Join(fixture.state, "source-manifest.json")
	fixture.effectsPath = filepath.Join(fixture.state, "effects.json")
	manifest := map[string]any{
		"schema": "pm.warehouse.source-manifest/v1",
		"scopes": []string{"workspace-isolation"},
		"segments": []map[string]any{{
			"relation": isolationSourceRelation,
			"path":     fixture.source,
			"sha256":   fixture.sourceDigest,
			"bytes":    fixture.sourceBytes,
		}},
	}
	effects := map[string]any{
		"schema":          "pm.warehouse.effects/v1",
		"published":       []string{fixture.sourceDigest},
		"checkpoint_rows": len(fixture.rows),
	}
	writeSealedJSON(t, fixture.manifestPath, manifest)
	writeSealedJSON(t, fixture.effectsPath, effects)
	if err := os.Chmod(fixture.state, isolationRootStatePermission); err != nil {
		t.Fatalf("seal the durable state directory: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(fixture.state, 0o700) })
}

func (fixture *queryIsolationFixture) captureSeal(t *testing.T) isolationSeal {
	t.Helper()
	return isolationSeal{
		manifest:  digestFileString(t, fixture.manifestPath),
		effects:   digestFileString(t, fixture.effectsPath),
		inventory: fixture.inventory(t),
	}
}

func (fixture *queryIsolationFixture) expectDurableStateUnchanged(t *testing.T) {
	t.Helper()
	if digest := digestFileString(t, fixture.manifestPath); digest != fixture.sealed.manifest {
		t.Fatalf("source manifest changed: %s became %s", fixture.sealed.manifest, digest)
	}
	if digest := digestFileString(t, fixture.effectsPath); digest != fixture.sealed.effects {
		t.Fatalf("effect record changed: %s became %s", fixture.sealed.effects, digest)
	}
	manifest, err := os.ReadFile(fixture.manifestPath)
	if err != nil {
		t.Fatalf("read the source manifest after execution: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(manifest, &decoded); err != nil {
		t.Fatalf("source manifest is no longer a valid document: %v", err)
	}
	if decoded["schema"] != "pm.warehouse.source-manifest/v1" {
		t.Fatalf("source manifest lost its declared schema: %v", decoded["schema"])
	}
	if inventory := fixture.inventory(t); inventory != fixture.sealed.inventory {
		t.Fatalf("fixture inventory changed:\n got %s\nwant %s", inventory, fixture.sealed.inventory)
	}
}

// inventory records every path, permission, size and content digest of the
// sealed surfaces: the admitted inputs and the durable state. The probes,
// outside, and scratch directories are excluded because the test itself writes
// hostile fixtures there and the supervisor removes scratch on completion.
func (fixture *queryIsolationFixture) inventory(t *testing.T) string {
	t.Helper()
	entries := []string{}
	for _, sealed := range []string{fixture.admitted, fixture.state} {
		err := filepath.WalkDir(sealed, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(fixture.root, path)
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			digest := "-"
			if info.Mode().IsRegular() {
				digest = digestFileString(t, path)
			}
			entries = append(entries, fmt.Sprintf("%s %s %d %s", relative, info.Mode().Perm(), info.Size(), digest))
			return nil
		})
		if err != nil {
			t.Fatalf("inventory the sealed fixture surface: %v", err)
		}
	}
	sort.Strings(entries)
	return strings.Join(entries, "\n")
}

func (fixture *queryIsolationFixture) expectScratchEmpty(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(fixture.scratchRoot)
	if err != nil {
		t.Fatalf("read the supervisor scratch root: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("supervisor left disposable scratch behind: %v", names)
	}
}

// isolationDiagnostics is the concurrency-safe bounded child diagnostic sink
// used to observe a live child from the test while the supervisor is blocked.
type isolationDiagnostics struct {
	mutex  sync.Mutex
	buffer bytes.Buffer
}

func (diagnostics *isolationDiagnostics) Write(data []byte) (int, error) {
	diagnostics.mutex.Lock()
	defer diagnostics.mutex.Unlock()
	return diagnostics.buffer.Write(data)
}

func (diagnostics *isolationDiagnostics) text() string {
	diagnostics.mutex.Lock()
	defer diagnostics.mutex.Unlock()
	return diagnostics.buffer.String()
}

// waitForProcessID waits for the child's own startup announcement, which the
// child emits before it reads the request.
func (diagnostics *isolationDiagnostics) waitForProcessID(t *testing.T, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, line := range strings.Split(diagnostics.text(), "\n") {
			marker := strings.Index(line, "pm-query ready pid=")
			if marker < 0 {
				continue
			}
			fields := strings.Fields(line[marker+len("pm-query ready pid="):])
			if len(fields) == 0 {
				continue
			}
			pid, err := strconv.Atoi(fields[0])
			if err != nil || pid <= 0 {
				continue
			}
			return pid
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("child never announced its process identity; diagnostics=%q", diagnostics.text())
	return 0
}

// isolationSourceRows builds the admitted relation's declared schema and exact
// typed rows. Every value class the child must project exactly is present.
func isolationSourceRows(t *testing.T) ([]warehouse.QueryColumn, [][]warehouse.QueryValue) {
	t.Helper()
	columns := []warehouse.QueryColumn{
		{Name: "id", Kind: warehouse.QueryKindInt64, Nullable: false},
		{Name: "amount", Kind: warehouse.QueryKindDecimal, Precision: 18, Scale: 3, Nullable: false},
		{Name: "label", Kind: warehouse.QueryKindText, Nullable: false},
		{Name: "flag", Kind: warehouse.QueryKindBoolean, Nullable: false},
		{Name: "payload", Kind: warehouse.QueryKindBytea, Nullable: false},
		{Name: "day", Kind: warehouse.QueryKindDate, Nullable: false},
		{Name: "naive", Kind: warehouse.QueryKindTimestamp, Nullable: false},
		{Name: "instant", Kind: warehouse.QueryKindTimestampTZ, Nullable: false},
		{Name: "identifier", Kind: warehouse.QueryKindUUID, Nullable: false},
		{Name: "document", Kind: warehouse.QueryKindJSON, Nullable: false},
		{Name: "maybe", Kind: warehouse.QueryKindText, Nullable: true},
	}
	uuid := mustUUID(t, "6ba7b810-9dad-11d1-80b4-00c04fd430c8")
	day := mustDate(t, "2024-02-29")
	document := mustJSON(t, `{"b":1,"a":[1,2,3]}`)
	amount := mustDecimal(t, "12345678", 3)
	zeroAmount := mustDecimal(t, "0", 3)
	zeroDay := mustDate(t, "1970-01-01")
	zeroUUID := mustUUID(t, "00000000-0000-0000-0000-000000000000")
	emptyDocument := mustJSON(t, `[]`)
	unitAmount := mustDecimal(t, "1", 3)
	lastDay := mustDate(t, "9999-12-31")
	lastUUID := mustUUID(t, "ffffffff-ffff-ffff-ffff-ffffffffffff")
	scalarDocument := mustJSON(t, `"text"`)
	minAmount := mustDecimal(t, "-999999999999999", 3)
	firstDay := mustDate(t, "0001-01-01")
	lastIdentifier := mustUUID(t, "01234567-89ab-cdef-0123-456789abcdef")
	objectDocument := mustJSON(t, `{"z":null}`)
	rows := [][]warehouse.QueryValue{
		{
			warehouse.Int64QueryValue(9007199254740993),
			amount,
			warehouse.TextQueryValue("alpha"),
			warehouse.BooleanQueryValue(true),
			warehouse.BytesQueryValue([]byte{0xde, 0xad, 0xbe, 0xef}),
			day,
			warehouse.TimestampQueryValue(1709212455123456),
			warehouse.TimestampTZQueryValue(1709212455123456),
			uuid,
			document,
			warehouse.NullQueryValue(),
		},
		{
			warehouse.Int64QueryValue(-9007199254740993),
			zeroAmount,
			warehouse.TextQueryValue(""),
			warehouse.BooleanQueryValue(false),
			warehouse.BytesQueryValue([]byte{}),
			zeroDay,
			warehouse.TimestampQueryValue(0),
			warehouse.TimestampTZQueryValue(0),
			zeroUUID,
			emptyDocument,
			warehouse.TextQueryValue("present"),
		},
		{
			warehouse.Int64QueryValue(0),
			unitAmount,
			warehouse.TextQueryValue("λ-テキスト"),
			warehouse.BooleanQueryValue(true),
			warehouse.BytesQueryValue([]byte{0x00}),
			lastDay,
			warehouse.TimestampQueryValue(-1),
			warehouse.TimestampTZQueryValue(-1),
			lastUUID,
			scalarDocument,
			warehouse.NullQueryValue(),
		},
		{
			warehouse.Int64QueryValue(9223372036854775807),
			minAmount,
			warehouse.TextQueryValue("omega"),
			warehouse.BooleanQueryValue(false),
			warehouse.BytesQueryValue([]byte{0x01, 0x02, 0x03}),
			firstDay,
			warehouse.TimestampQueryValue(253402300799999999),
			warehouse.TimestampTZQueryValue(253402300799999999),
			lastIdentifier,
			objectDocument,
			warehouse.TextQueryValue(""),
		},
	}
	return columns, rows
}

// buildRestrictedQueryChild builds cmd/pm-query with the pinned toolchain. The
// child is the only process that links the native analytical engine, so the
// acceptance case must execute a real binary rather than an in-test stand-in.
func buildRestrictedQueryChild(t *testing.T) string {
	t.Helper()
	root := isolationRepositoryRoot(t)
	output := filepath.Join(t.TempDir(), "pm-query")
	environment := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "CGO_CFLAGS", "CGO_CPPFLAGS", "CGO_CXXFLAGS", "CGO_LDFLAGS":
			continue
		default:
			environment = append(environment, entry)
		}
	}
	environment = append(environment, "CGO_ENABLED=1")
	command := exec.CommandContext(t.Context(), "python3", filepath.Join(root, "tools", "bootstrap.py"),
		"exec", "go", "--", "build", "-mod=readonly", "-o", output, "./cmd/pm-query")
	command.Dir = root
	command.Env = environment
	combined, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build the restricted query child: %v\n%s", err, combined)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatalf("stat the restricted query child: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("restricted query child is not an executable regular file: %v", info.Mode())
	}
	return output
}

func isolationRepositoryRoot(t *testing.T) string {
	t.Helper()
	// The owned stack fixture exports the checkout it was started for, and the
	// thin-client qualification already resolves the repository through that
	// same explicit path. Prefer it, because a sandboxed Bazel test otherwise
	// picks its runfiles tree, which does not carry the pinned bootstrap script
	// this case builds the restricted child with.
	if root := os.Getenv("PM_STACK_ROOT"); filepath.IsAbs(root) {
		if info, err := os.Stat(filepath.Join(root, "tools", "bootstrap.py")); err == nil && info.Mode().IsRegular() {
			return root
		}
	}
	if runfiles := os.Getenv("TEST_SRCDIR"); runfiles != "" {
		workspace := os.Getenv("TEST_WORKSPACE")
		if workspace == "" {
			t.Fatal("declared Bazel workspace name is required to locate the checkout")
		}
		candidate := filepath.Join(runfiles, workspace)
		if info, err := os.Stat(filepath.Join(candidate, "tools", "bootstrap.py")); err == nil && info.Mode().IsRegular() {
			return candidate
		}
		t.Fatal("declared runfiles tree does not carry tools/bootstrap.py; run this case through the owned fixture")
	}
	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve the working directory: %v", err)
	}
	for {
		if info, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil && info.Mode().IsRegular() {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("could not locate the repository root from the test working directory")
		}
		directory = parent
	}
}

func writeSealedJSON(t *testing.T, path string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode sealed record: %v", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o400); err != nil {
		t.Fatalf("write sealed record %s: %v", filepath.Base(path), err)
	}
}

func digestFile(t *testing.T, path string) (string, int64) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", filepath.Base(path), err)
	}
	return digestFileString(t, path), info.Size()
}

func digestFileString(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), err)
	}
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func mustDecimal(t *testing.T, unscaled string, scale int) warehouse.QueryValue {
	t.Helper()
	value, err := warehouse.DecimalQueryValue(unscaled, scale)
	if err != nil {
		t.Fatalf("construct decimal value: %v", err)
	}
	return value
}

func mustDate(t *testing.T, value string) warehouse.QueryValue {
	t.Helper()
	date, err := warehouse.DateQueryValue(value)
	if err != nil {
		t.Fatalf("construct date value: %v", err)
	}
	return date
}

func mustUUID(t *testing.T, value string) warehouse.QueryValue {
	t.Helper()
	identifier, err := warehouse.UUIDQueryValue(value)
	if err != nil {
		t.Fatalf("construct UUID value: %v", err)
	}
	return identifier
}

func mustJSON(t *testing.T, value string) warehouse.QueryValue {
	t.Helper()
	document, err := warehouse.JSONQueryValue(value)
	if err != nil {
		t.Fatalf("construct JSON value: %v", err)
	}
	return document
}

func requireRefusal(t *testing.T, err error) *warehouse.Refusal {
	t.Helper()
	var refusal *warehouse.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("expected an explicit refusal, got %T: %v", err, err)
	}
	if refusal.Reason == "" {
		t.Fatal("refusal carried no reason")
	}
	return refusal
}

func withinRoot(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsInt(values []int, wanted int) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func describeColumns(columns []warehouse.QueryColumn) string {
	parts := make([]string, 0, len(columns))
	for _, column := range columns {
		parts = append(parts, fmt.Sprintf("%s:%s[%d,%d]null=%t", column.Name, column.Kind, column.Precision, column.Scale, column.Nullable))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

func describeRows(rows [][]warehouse.QueryValue) string {
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		cells := make([]string, 0, len(row))
		for _, cell := range row {
			cells = append(cells, cell.String())
		}
		parts = append(parts, "["+strings.Join(cells, ",")+"]")
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// rowsAscendingByID returns the fixture rows in the ascending identifier order
// the isolation requests declare.
func (fixture *queryIsolationFixture) rowsAscendingByID() [][]warehouse.QueryValue {
	indexed := append([][]warehouse.QueryValue(nil), fixture.rows...)
	sort.SliceStable(indexed, func(left, right int) bool {
		leftID, _ := indexed[left][0].Int64()
		rightID, _ := indexed[right][0].Int64()
		return leftID < rightID
	})
	return indexed
}
