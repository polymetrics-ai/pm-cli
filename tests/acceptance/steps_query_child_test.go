package acceptance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cucumber/godog"
	"github.com/duckdb/duckdb-go/v2"
)

// queryChildBuild holds the real pm-query binary, built once for the suite.
var queryChildBuild struct {
	once sync.Once
	path string
	err  error
}

// queryChildBinary builds ./cmd/pm-query with CGO_ENABLED=1 and the go tool on
// PATH. The binary goes to the ignored .cache directory of the checkout, so a
// new run replaces it. CI sets TMPDIR to a persistent cache, so a new temporary
// directory for each run would stay there.
func queryChildBinary() (string, error) {
	queryChildBuild.once.Do(func() {
		root, err := filepath.Abs(filepath.Join("..", ".."))
		if err != nil {
			queryChildBuild.err = fmt.Errorf("resolve repository root: %w", err)
			return
		}
		goTool, err := exec.LookPath("go")
		if err != nil {
			queryChildBuild.err = fmt.Errorf("find the go tool on PATH: %w", err)
			return
		}
		dir := filepath.Join(root, ".cache", "acceptance")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			queryChildBuild.err = fmt.Errorf("create the binary directory: %w", err)
			return
		}
		binary := filepath.Join(dir, "pm-query")
		cmd := exec.Command(goTool, "build", "-o", binary, "./cmd/pm-query")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			queryChildBuild.err = fmt.Errorf("build pm-query: %v\n%s", err, out)
			return
		}
		queryChildBuild.path = binary
	})
	return queryChildBuild.path, queryChildBuild.err
}

// queryColumn is one column in the pm.query/1 wire form.
type queryColumn struct {
	Name      string  `json:"name"`
	Kind      string  `json:"kind"`
	Precision *string `json:"precision,omitempty"`
	Scale     *string `json:"scale,omitempty"`
	Nullable  *bool   `json:"nullable,omitempty"`
}

// kindText gives the kind, with the precision and scale of a decimal.
func (column queryColumn) kindText() string {
	if column.Kind == "decimal" && column.Precision != nil && column.Scale != nil {
		return "decimal(" + *column.Precision + "," + *column.Scale + ")"
	}
	return column.Kind
}

type queryBounds struct {
	Relations   string `json:"relations"`
	Columns     string `json:"columns"`
	Rows        string `json:"rows"`
	Bytes       string `json:"bytes"`
	TempBytes   string `json:"temp_bytes"`
	MemoryBytes string `json:"memory_bytes"`
	WallMillis  string `json:"wall_millis"`
}

type queryRelation struct {
	Name    string        `json:"name"`
	Path    string        `json:"path"`
	SHA256  string        `json:"sha256"`
	Bytes   string        `json:"bytes"`
	Columns []queryColumn `json:"columns"`
}

type queryProjection struct {
	Column string  `json:"column"`
	Alias  *string `json:"alias,omitempty"`
}

type queryOutput struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256,omitempty"`
	Bytes  string `json:"bytes,omitempty"`
}

type querySelect struct {
	From       string            `json:"from"`
	Project    []queryProjection `json:"project"`
	Predicates []json.RawMessage `json:"predicates"`
	Order      []json.RawMessage `json:"order"`
	Limit      string            `json:"limit"`
	Output     *queryOutput      `json:"output,omitempty"`
}

type queryRequest struct {
	Protocol            string          `json:"protocol"`
	RequestID           string          `json:"request_id"`
	AdmittedDirectories []string        `json:"admitted_directories"`
	Bounds              queryBounds     `json:"bounds"`
	Relations           []queryRelation `json:"relations"`
	Select              querySelect     `json:"select"`
}

type queryRefusal struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// queryValue is one typed cell in the wire form. Integral payloads travel as
// decimal strings.
type queryValue struct {
	Kind     string          `json:"kind"`
	Value    json.RawMessage `json:"value"`
	Unscaled string          `json:"unscaled"`
	Scale    string          `json:"scale"`
	Micros   string          `json:"micros"`
}

// text gives one comparable form of a returned cell.
func (value queryValue) text() string {
	switch value.Kind {
	case "null":
		return "null"
	case "decimal":
		return "decimal(" + value.Unscaled + "," + value.Scale + ")"
	case "timestamp", "timestamptz":
		return value.Kind + "(" + value.Micros + ")"
	case "text", "int64":
		var payload string
		if err := json.Unmarshal(value.Value, &payload); err == nil {
			if value.Kind == "text" {
				return "text(" + strconv.Quote(payload) + ")"
			}
			return "int64(" + payload + ")"
		}
	}
	return value.Kind + "(" + string(value.Value) + ")"
}

type queryResponse struct {
	Protocol  string          `json:"protocol"`
	RequestID string          `json:"request_id"`
	Columns   *[]queryColumn  `json:"columns"`
	Rows      *[][]queryValue `json:"rows"`
	Truncated *bool           `json:"truncated"`
	Output    *queryOutput    `json:"output"`
	Refusal   *queryRefusal   `json:"refusal"`
	Child     json.RawMessage `json:"child"`
}

// queryChildRun records one real pm-query process.
type queryChildRun struct {
	exitCode  int
	stdout    string
	stderr    string
	response  *queryResponse
	decodeErr error
}

func (run *queryChildRun) describe() string {
	return fmt.Sprintf("pm-query exit code %d\nstdout: %s\nstderr: %s", run.exitCode, run.stdout, run.stderr)
}

// succeeded returns the response of a run that returned columns and rows.
func (run *queryChildRun) succeeded() (*queryResponse, error) {
	switch {
	case run.exitCode != 0:
		return nil, fmt.Errorf("pm-query did not succeed\n%s", run.describe())
	case run.decodeErr != nil:
		return nil, fmt.Errorf("decode the pm-query response: %v\n%s", run.decodeErr, run.describe())
	case run.response.Refusal != nil:
		return nil, fmt.Errorf("pm-query refused the request\n%s", run.describe())
	case run.response.Columns == nil || run.response.Rows == nil:
		return nil, fmt.Errorf("the pm-query response has no columns or no rows\n%s", run.describe())
	}
	return run.response, nil
}

// footerColumn is one leaf column of a Parquet footer, with its kind in the
// pm-query vocabulary.
type footerColumn struct {
	name string
	kind string
}

// queryChildScenario holds the state of one scenario: its directories, the
// observer's DuckDB, the relations that the observer wrote, and the last
// pm-query run.
type queryChildScenario struct {
	root      string
	data      string
	scratch   string
	observer  *sql.DB
	relations map[string]queryRelation
	from      string
	project   []queryProjection
	output    string
	last      *queryChildRun
}

func registerQueryChildSteps(sc *godog.ScenarioContext) {
	scenario := &queryChildScenario{}
	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		*scenario = queryChildScenario{relations: map[string]queryRelation{}}
		return ctx, nil
	})
	// godog reports any error from this hook as a hook failure, so the hook
	// returns only its own error and never the step error.
	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		return ctx, scenario.close()
	})
	sc.Step(`^the observer writes a Parquet relation "([^"]*)" with these rows:$`, scenario.writeRelation)
	sc.Step(`^pm-query selects "([^"]*)" from "([^"]*)" into a Parquet output$`, scenario.selectIntoParquet)
	sc.Step(`^pm-query succeeds and returns the columns "([^"]*)"$`, scenario.returnsColumns)
	sc.Step(`^the observer reads the footer columns "([^"]*)" in that order$`, scenario.footerColumns)
	sc.Step(`^the footer column types equal the returned column types$`, scenario.footerTypesEqualReturned)
	sc.Step(`^the observer reads the same rows from the file as pm-query returns, with "([^"]*)" null for "([^"]*)"$`, scenario.sameRows)
	sc.Step(`^pm-query refuses the request$`, scenario.refuses)
	sc.Step(`^the output file does not exist$`, scenario.outputAbsent)
}

// prepare creates the scenario directories and opens the observer's in-memory
// DuckDB. The child refuses an admitted directory or a TMPDIR that is a
// symbolic link, so every path is resolved first.
func (s *queryChildScenario) prepare() error {
	if s.root != "" {
		return nil
	}
	root, err := os.MkdirTemp("", "pm-acceptance-query-")
	if err != nil {
		return fmt.Errorf("create the scenario directory: %w", err)
	}
	s.root = root
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve the scenario directory: %w", err)
	}
	s.data = filepath.Join(resolved, "data")
	s.scratch = filepath.Join(resolved, "scratch")
	for _, dir := range []string{s.data, s.scratch} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	observer, err := sql.Open("duckdb", "")
	if err != nil {
		return fmt.Errorf("open the observer DuckDB: %w", err)
	}
	// One connection keeps the session time zone for every statement.
	observer.SetMaxOpenConns(1)
	s.observer = observer
	if _, err := observer.Exec("SET TimeZone = 'UTC'"); err != nil {
		return fmt.Errorf("set the observer time zone: %w", err)
	}
	return nil
}

func (s *queryChildScenario) close() error {
	var errs []error
	if s.observer != nil {
		if err := s.observer.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close the observer DuckDB: %w", err))
		}
	}
	if s.root != "" {
		if err := os.RemoveAll(s.root); err != nil {
			errs = append(errs, fmt.Errorf("remove the scenario directory: %w", err))
		}
	}
	return errors.Join(errs...)
}

var (
	queryIdentifierPattern  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	queryDecimalTypePattern = regexp.MustCompile(`^DECIMAL\(([0-9]+),([0-9]+)\)$`)
)

// declaredColumn maps one DuckDB type of the table header to the pm-query
// kind. Every column is nullable, because an empty cell is NULL.
func declaredColumn(name, duckdbType string) (queryColumn, error) {
	nullable := true
	column := queryColumn{Name: name, Nullable: &nullable}
	switch duckdbType {
	case "VARCHAR":
		column.Kind = "text"
	case "BIGINT":
		column.Kind = "int64"
	case "TIMESTAMPTZ":
		column.Kind = "timestamptz"
	default:
		match := queryDecimalTypePattern.FindStringSubmatch(duckdbType)
		if match == nil {
			return column, fmt.Errorf("column %q has the type %q, which has no pm-query kind here", name, duckdbType)
		}
		column.Kind = "decimal"
		column.Precision, column.Scale = &match[1], &match[2]
	}
	return column, nil
}

// writeRelation writes the table as a Parquet file in the admitted directory.
// Each header cell is name:TYPE, and an empty cell is NULL.
func (s *queryChildScenario) writeRelation(name string, table *godog.Table) error {
	if err := s.prepare(); err != nil {
		return err
	}
	if !queryIdentifierPattern.MatchString(name) {
		return fmt.Errorf("relation name %q is not a plain identifier", name)
	}
	if len(table.Rows) < 2 {
		return errors.New("the table needs a header row and at least one row")
	}
	var columns []queryColumn
	var definitions, casts []string
	for _, cell := range table.Rows[0].Cells {
		columnName, duckdbType, found := strings.Cut(cell.Value, ":")
		if !found || !queryIdentifierPattern.MatchString(columnName) {
			return fmt.Errorf("header cell %q must be name:TYPE", cell.Value)
		}
		column, err := declaredColumn(columnName, duckdbType)
		if err != nil {
			return err
		}
		columns = append(columns, column)
		definitions = append(definitions, `"`+columnName+`" `+duckdbType)
		casts = append(casts, "CAST(? AS "+duckdbType+")")
	}
	if _, err := s.observer.Exec(`CREATE TABLE "` + name + `" (` + strings.Join(definitions, ", ") + ")"); err != nil {
		return fmt.Errorf("create the observer table: %w", err)
	}
	for _, row := range table.Rows[1:] {
		if len(row.Cells) != len(columns) {
			return fmt.Errorf("a row has %d cells for %d columns", len(row.Cells), len(columns))
		}
		arguments := make([]any, 0, len(columns))
		for _, cell := range row.Cells {
			if cell.Value == "" {
				arguments = append(arguments, nil)
				continue
			}
			arguments = append(arguments, cell.Value)
		}
		if _, err := s.observer.Exec(`INSERT INTO "`+name+`" VALUES (`+strings.Join(casts, ", ")+")", arguments...); err != nil {
			return fmt.Errorf("insert an observer row: %w", err)
		}
	}
	path := filepath.Join(s.data, name+".parquet")
	if _, err := s.observer.Exec(`COPY "` + name + `" TO ` + quoteQueryLiteral(path) + " (FORMAT PARQUET)"); err != nil {
		return fmt.Errorf("write the relation file: %w", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read the relation file: %w", err)
	}
	digest := sha256.Sum256(content)
	s.relations[name] = queryRelation{
		Name:    name,
		Path:    path,
		SHA256:  "sha256:" + hex.EncodeToString(digest[:]),
		Bytes:   strconv.Itoa(len(content)),
		Columns: columns,
	}
	return nil
}

func quoteQueryLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// parseProjection reads "column" or "column AS alias" items, separated by
// commas.
func parseProjection(text string) ([]queryProjection, error) {
	var project []queryProjection
	for _, item := range strings.Split(text, ",") {
		fields := strings.Fields(item)
		switch {
		case len(fields) == 1:
			project = append(project, queryProjection{Column: fields[0]})
		case len(fields) == 3 && strings.EqualFold(fields[1], "AS"):
			alias := fields[2]
			project = append(project, queryProjection{Column: fields[0], Alias: &alias})
		default:
			return nil, fmt.Errorf("projection item %q must be a column or column AS alias", strings.TrimSpace(item))
		}
	}
	return project, nil
}

// splitNames reads a comma-separated list of names.
func splitNames(text string) []string {
	var names []string
	for _, name := range strings.Split(text, ",") {
		names = append(names, strings.TrimSpace(name))
	}
	return names
}

func (s *queryChildScenario) selectIntoParquet(projection, from string) error {
	if err := s.prepare(); err != nil {
		return err
	}
	project, err := parseProjection(projection)
	if err != nil {
		return err
	}
	s.from, s.project = from, project
	s.output = filepath.Join(s.data, "result.parquet")
	run, err := s.runQuery("query-child-output", s.output)
	if err != nil {
		return err
	}
	s.last = run
	return nil
}

// runQuery sends one pm.query/1 selection to a new pm-query process. The
// environment, the descriptor declaration and the process group are the same
// as those that the supervisor gives the child. An empty output path selects
// rows without a Parquet output.
func (s *queryChildScenario) runQuery(requestID, output string) (*queryChildRun, error) {
	binary, err := queryChildBinary()
	if err != nil {
		return nil, err
	}
	relation, written := s.relations[s.from]
	if !written {
		return nil, fmt.Errorf("the observer wrote no relation %q", s.from)
	}
	request := queryRequest{
		Protocol:            "pm.query/1",
		RequestID:           requestID,
		AdmittedDirectories: []string{s.data},
		Bounds: queryBounds{
			Relations:   "1",
			Columns:     "16",
			Rows:        "1000",
			Bytes:       "16777216",
			TempBytes:   "67108864",
			MemoryBytes: "268435456",
			WallMillis:  "60000",
		},
		Relations: []queryRelation{relation},
		Select: querySelect{
			From:       s.from,
			Project:    s.project,
			Predicates: []json.RawMessage{},
			Order:      []json.RawMessage{},
			Limit:      "1000",
		},
	}
	if output != "" {
		request.Select.Output = &queryOutput{Path: output}
	}
	document, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode the pm-query request: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary)
	cmd.Env = []string{
		"HOME=" + s.scratch,
		"TMPDIR=" + s.scratch,
		"PATH=/usr/bin:/bin",
		"LC_ALL=C",
		"LANG=C",
		"TZ=UTC",
		"PM_QUERY_CLOSE_DESCRIPTORS=" + inheritableDescriptors(),
	}
	cmd.Stdin = bytes.NewReader(document)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	run := &queryChildRun{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		run.exitCode = exitErr.ExitCode()
	default:
		return nil, fmt.Errorf("start pm-query: %w", err)
	}
	if ctx.Err() != nil {
		return nil, errors.New("pm-query did not finish within 2m")
	}
	var response queryResponse
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &response); err != nil {
		run.decodeErr = err
	} else {
		run.response = &response
	}
	return run, nil
}

// inheritableDescriptors lists the descriptors above the standard three that
// this process holds without close-on-exec. The supervisor declares the same
// set, and the child refuses to start without the declaration.
func inheritableDescriptors() string {
	var declared []string
	for descriptor := 3; descriptor < 256; descriptor++ {
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(descriptor), uintptr(syscall.F_GETFD), 0)
		if errno != 0 || flags&uintptr(syscall.FD_CLOEXEC) != 0 {
			continue
		}
		declared = append(declared, strconv.Itoa(descriptor))
	}
	return strings.Join(declared, ",")
}

func (s *queryChildScenario) lastRun() (*queryChildRun, error) {
	if s.last == nil {
		return nil, errors.New("pm-query did not run")
	}
	return s.last, nil
}

func (s *queryChildScenario) returnsColumns(list string) error {
	run, err := s.lastRun()
	if err != nil {
		return err
	}
	response, err := run.succeeded()
	if err != nil {
		return err
	}
	var names []string
	for _, column := range *response.Columns {
		names = append(names, column.Name)
	}
	if want := splitNames(list); !slices.Equal(names, want) {
		return fmt.Errorf("pm-query returned the columns %q, want %q", names, want)
	}
	if response.Output == nil || response.Output.Path != s.output {
		return fmt.Errorf("pm-query did not describe the output %s\n%s", s.output, run.describe())
	}
	return nil
}

// readFooter reads the leaf columns of the Parquet footer in file order.
func (s *queryChildScenario) readFooter(path string) ([]footerColumn, error) {
	if _, err := os.Lstat(path); err != nil {
		return nil, fmt.Errorf("the output file is not readable: %w", err)
	}
	rows, err := s.observer.Query(
		"SELECT name, type, converted_type, scale, precision, logical_type, num_children FROM parquet_schema(?)", path)
	if err != nil {
		return nil, fmt.Errorf("read the Parquet footer: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var footer []footerColumn
	for rows.Next() {
		var name, physical, converted, logical sql.NullString
		var scale, precision, children sql.NullInt64
		if err := rows.Scan(&name, &physical, &converted, &scale, &precision, &logical, &children); err != nil {
			return nil, fmt.Errorf("read the Parquet footer: %w", err)
		}
		// The root element of the schema has children. A flat file has no other
		// group element.
		if children.Valid && children.Int64 > 0 {
			continue
		}
		footer = append(footer, footerColumn{
			name: name.String,
			kind: footerKind(physical.String, converted.String, logical.String, scale, precision),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the Parquet footer: %w", err)
	}
	return footer, nil
}

// footerKind maps the Parquet physical, converted and logical types of one
// footer column to the pm-query kind vocabulary.
func footerKind(physical, converted, logical string, scale, precision sql.NullInt64) string {
	switch {
	case converted == "UTF8":
		return "text"
	case converted == "DECIMAL" && precision.Valid && scale.Valid:
		return fmt.Sprintf("decimal(%d,%d)", precision.Int64, scale.Int64)
	case converted == "TIMESTAMP_MICROS" && strings.Contains(logical, "isAdjustedToUTC=1"):
		return "timestamptz"
	case converted == "TIMESTAMP_MICROS":
		return "timestamp"
	case physical == "INT64" && (converted == "INT_64" || converted == ""):
		return "int64"
	}
	return fmt.Sprintf("unmapped(type=%s converted_type=%s logical_type=%s)", physical, converted, logical)
}

func footerNames(footer []footerColumn) []string {
	names := make([]string, 0, len(footer))
	for _, column := range footer {
		names = append(names, column.name)
	}
	return names
}

func (s *queryChildScenario) footerColumns(list string) error {
	run, err := s.lastRun()
	if err != nil {
		return err
	}
	footer, err := s.readFooter(s.output)
	if err != nil {
		return fmt.Errorf("%w\n%s", err, run.describe())
	}
	if names, want := footerNames(footer), splitNames(list); !slices.Equal(names, want) {
		return fmt.Errorf("the file footer has the columns %q, want %q in that order", names, want)
	}
	return nil
}

func (s *queryChildScenario) footerTypesEqualReturned() error {
	run, err := s.lastRun()
	if err != nil {
		return err
	}
	response, err := run.succeeded()
	if err != nil {
		return err
	}
	footer, err := s.readFooter(s.output)
	if err != nil {
		return err
	}
	returned := *response.Columns
	if len(footer) != len(returned) {
		return fmt.Errorf("the file footer has %d columns, pm-query returned %d", len(footer), len(returned))
	}
	var mismatches []string
	for index, column := range returned {
		if footer[index].kind != column.kindText() {
			mismatches = append(mismatches, fmt.Sprintf("column %d %q: footer %s, returned %s",
				index+1, column.Name, footer[index].kind, column.kindText()))
		}
	}
	if len(mismatches) > 0 {
		return fmt.Errorf("the footer types differ from the returned types:\n%s", strings.Join(mismatches, "\n"))
	}
	return nil
}

// sameRows compares the rows in the file with the rows that pm-query returns.
// pm-query returns no rows for a selection with an output, so the observer
// sends the same selection without an output and compares with its rows.
func (s *queryChildScenario) sameRows(nullColumn, key string) error {
	run, err := s.lastRun()
	if err != nil {
		return err
	}
	written, err := run.succeeded()
	if err != nil {
		return err
	}
	read, err := s.runQuery("query-child-rows", "")
	if err != nil {
		return err
	}
	response, err := read.succeeded()
	if err != nil {
		return err
	}
	if !slices.Equal(columnKinds(*response.Columns), columnKinds(*written.Columns)) {
		return fmt.Errorf("pm-query returned the columns %q without an output and %q with an output",
			columnKinds(*response.Columns), columnKinds(*written.Columns))
	}
	var returned [][]string
	for _, row := range *response.Rows {
		cells := make([]string, 0, len(row))
		for _, value := range row {
			cells = append(cells, value.text())
		}
		returned = append(returned, cells)
	}
	footer, err := s.readFooter(s.output)
	if err != nil {
		return err
	}
	file, err := s.readFileRows(s.output, footer)
	if err != nil {
		return err
	}
	if !slices.EqualFunc(file, returned, slices.Equal[[]string]) {
		return fmt.Errorf("the file has the rows %q, pm-query returns %q", file, returned)
	}
	position := slices.IndexFunc(*response.Columns, func(column queryColumn) bool { return column.Name == nullColumn })
	if position < 0 {
		return fmt.Errorf("pm-query returned no column %q", nullColumn)
	}
	keyText := "text(" + strconv.Quote(key) + ")"
	var matches [][]string
	for _, row := range file {
		if slices.Contains(row, keyText) {
			matches = append(matches, row)
		}
	}
	if len(matches) != 1 {
		return fmt.Errorf("the file has %d rows with %q, want 1: %q", len(matches), key, file)
	}
	if matches[0][position] != "null" {
		return fmt.Errorf("the file row for %q has %q = %s, want null", key, nullColumn, matches[0][position])
	}
	return nil
}

func columnKinds(columns []queryColumn) []string {
	kinds := make([]string, 0, len(columns))
	for _, column := range columns {
		kinds = append(kinds, column.Name+" "+column.kindText())
	}
	return kinds
}

// readFileRows reads every row of the file with read_parquet. Each cell gets
// the same comparable form as a returned cell. A timestamp cell takes its kind
// from the footer.
func (s *queryChildScenario) readFileRows(path string, footer []footerColumn) ([][]string, error) {
	rows, err := s.observer.Query("SELECT * FROM read_parquet(?)", path)
	if err != nil {
		return nil, fmt.Errorf("read the output file: %w", err)
	}
	defer func() { _ = rows.Close() }()
	names, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("read the output file columns: %w", err)
	}
	if len(names) != len(footer) {
		return nil, fmt.Errorf("read_parquet gives %d columns, the footer has %d", len(names), len(footer))
	}
	var file [][]string
	for rows.Next() {
		cells := make([]any, len(names))
		targets := make([]any, len(names))
		for index := range cells {
			targets[index] = &cells[index]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("read an output row: %w", err)
		}
		row := make([]string, 0, len(cells))
		for index, cell := range cells {
			row = append(row, fileCellText(footer[index].kind, cell))
		}
		file = append(file, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the output rows: %w", err)
	}
	return file, nil
}

func fileCellText(kind string, cell any) string {
	switch value := cell.(type) {
	case nil:
		return "null"
	case string:
		return "text(" + strconv.Quote(value) + ")"
	case int64:
		return "int64(" + strconv.FormatInt(value, 10) + ")"
	case duckdb.Decimal:
		return "decimal(" + value.Value.String() + "," + strconv.Itoa(int(value.Scale)) + ")"
	case time.Time:
		return kind + "(" + strconv.FormatInt(value.UnixMicro(), 10) + ")"
	default:
		return fmt.Sprintf("%T(%v)", cell, cell)
	}
}

func (s *queryChildScenario) refuses() error {
	run, err := s.lastRun()
	if err != nil {
		return err
	}
	if run.exitCode != 1 {
		return fmt.Errorf("pm-query exit code %d, want 1 (a refusal)\n%s", run.exitCode, run.describe())
	}
	if run.decodeErr != nil || run.response.Refusal == nil || run.response.Refusal.Code == "" {
		return fmt.Errorf("pm-query wrote no refusal document\n%s", run.describe())
	}
	if run.response.Columns != nil || run.response.Rows != nil || run.response.Output != nil {
		return fmt.Errorf("the refusal carries columns, rows or an output\n%s", run.describe())
	}
	return nil
}

func (s *queryChildScenario) outputAbsent() error {
	if s.output == "" {
		return errors.New("no output path was requested")
	}
	_, err := os.Lstat(s.output)
	switch {
	case err == nil:
		footer, footerErr := s.readFooter(s.output)
		if footerErr != nil {
			return fmt.Errorf("the output file %s exists: %w", s.output, footerErr)
		}
		return fmt.Errorf("the output file %s exists with the columns %q", s.output, footerNames(footer))
	case errors.Is(err, os.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("inspect the output file: %w", err)
	}
}
