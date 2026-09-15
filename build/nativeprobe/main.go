package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	duckdbbindings "github.com/duckdb/duckdb-go-bindings"
	_ "github.com/duckdb/duckdb-go/v2"
)

const (
	nativeProbeSchema           = "pm.native-probe/v1"
	expectedDuckDBVersion       = "v1.5.5"
	probeTimeout                = 30 * time.Second
	driverModulePath            = "github.com/duckdb/duckdb-go/v2"
	driverModuleVersion         = "v2.10505.0"
	driverModuleSum             = "h1:SWwvLn2Qx/RQSnQNupwgIF8VbnJ5A6OQU9lYb/mDETI="
	bindingsModulePath          = "github.com/duckdb/duckdb-go-bindings"
	bindingsModuleVersion       = "v0.10505.0"
	bindingsModuleSum           = "h1:/0pPsTLrcCsTGxT0VrHgJWnOcPe1tQL1vrki1v3jbAI="
	positiveExactValue    int64 = 9007199254740993
	negativeExactValue    int64 = -9007199254740993
)

type moduleIdentity struct {
	Path            string `json:"path"`
	Version         string `json:"version"`
	BuildInfoSum    string `json:"build_info_sum,omitempty"`
	LockedSourceSum string `json:"locked_source_sum"`
	SumEvidence     string `json:"sum_evidence"`
}

type nativeProbeResult struct {
	SchemaVersion       string         `json:"schema_version"`
	Status              string         `json:"status"`
	Platform            string         `json:"platform"`
	PlatformSupport     string         `json:"platform_support"`
	DuckDBVersionSQL    string         `json:"duckdb_version_sql"`
	DuckDBVersionCAPI   string         `json:"duckdb_version_c_api"`
	DriverModule        moduleIdentity `json:"driver_module"`
	BindingsModule      moduleIdentity `json:"bindings_module"`
	NativeLibraryModule moduleIdentity `json:"native_library_module"`
	ExactValues         []string       `json:"exact_values"`
	ParquetRows         int            `json:"parquet_rows"`
}

type expectedModuleIdentity struct {
	path    string
	version string
	sum     string
}

var nativeModules = map[string]expectedModuleIdentity{
	"darwin-amd64": {
		path:    "github.com/duckdb/duckdb-go-bindings/lib/darwin-amd64",
		version: "v0.10505.0",
		sum:     "h1:FrMqquFBQlMsi34h2KZgCku54rqA8xEbXZ0NLVDKwYs=",
	},
	"darwin-arm64": {
		path:    "github.com/duckdb/duckdb-go-bindings/lib/darwin-arm64",
		version: "v0.10505.0",
		sum:     "h1:lbRbpQwT1MmUhh/VTwukV9K8bxKByV3UghAP3MvsbBo=",
	},
	"linux-amd64": {
		path:    "github.com/duckdb/duckdb-go-bindings/lib/linux-amd64",
		version: "v0.10505.0",
		sum:     "h1:nrsaVYj3XYCRbS2FpdOMD/KHE7egRMr+/NR1IHmjT84=",
	},
	"linux-arm64": {
		path:    "github.com/duckdb/duckdb-go-bindings/lib/linux-arm64",
		version: "v0.10505.0",
		sum:     "h1:qM6oGDgwXBILJGbTY4fCy6QOczLpucUA6yn6g3ORjh4=",
	},
}

func main() {
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "nativeprobe takes no arguments; it executes only the fixed in-memory and owned-temporary-file qualification")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	if err := runNativeProbe(ctx, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "nativeprobe: %v\n", err)
		os.Exit(1)
	}
}

func runNativeProbe(ctx context.Context, output io.Writer) error {
	result, err := probeDuckDB(ctx)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(result); err != nil {
		return fmt.Errorf("write probe result: %w", err)
	}
	return nil
}

func probeDuckDB(ctx context.Context) (nativeProbeResult, error) {
	var result nativeProbeResult
	if ctx == nil {
		return result, errors.New("native probe requires a non-nil context")
	}
	platform := runtime.GOOS + "-" + runtime.GOARCH
	nativeExpected, supported := nativeModules[platform]
	if !supported {
		return result, fmt.Errorf("unsupported native probe platform %s; supported source targets are %s and all remain unexecuted until separately qualified", platform, strings.Join(sortedNativePlatforms(), ", "))
	}

	driverExpected := expectedModuleIdentity{path: driverModulePath, version: driverModuleVersion, sum: driverModuleSum}
	driver, err := linkedModuleIdentity(driverModulePath)
	if err != nil {
		return result, err
	}
	if err := verifyModuleIdentity(driver, driverExpected); err != nil {
		return result, err
	}
	driver = reportModuleIdentity(driver, driverExpected)
	bindingsExpected := expectedModuleIdentity{path: bindingsModulePath, version: bindingsModuleVersion, sum: bindingsModuleSum}
	bindings, err := linkedModuleIdentity(bindingsModulePath)
	if err != nil {
		return result, err
	}
	if err := verifyModuleIdentity(bindings, bindingsExpected); err != nil {
		return result, err
	}
	bindings = reportModuleIdentity(bindings, bindingsExpected)
	nativeLibrary, err := linkedModuleIdentity(nativeExpected.path)
	if err != nil {
		return result, err
	}
	if err := verifyModuleIdentity(nativeLibrary, nativeExpected); err != nil {
		return result, err
	}
	nativeLibrary = reportModuleIdentity(nativeLibrary, nativeExpected)

	cAPIVersion := duckdbbindings.LibraryVersion()
	if err := verifyDuckDBVersion("C API", cAPIVersion); err != nil {
		return result, err
	}
	database, err := sql.Open("duckdb", "?threads=1&memory_limit=256MB&autoinstall_known_extensions=false&autoload_known_extensions=false")
	if err != nil {
		return result, fmt.Errorf("open in-memory DuckDB: %w", err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	if err := database.PingContext(ctx); err != nil {
		return result, fmt.Errorf("ping in-memory DuckDB: %w", err)
	}
	if _, err := database.ExecContext(ctx, "LOAD parquet"); err != nil {
		return result, fmt.Errorf("load bundled Parquet extension with installation disabled: %w", err)
	}

	var sqlVersion string
	var exactValue int64
	var marker string
	if err := database.QueryRowContext(ctx, "SELECT version(), CAST(9007199254740993 AS BIGINT), CAST('pm-native-probe' AS VARCHAR)").Scan(&sqlVersion, &exactValue, &marker); err != nil {
		return result, fmt.Errorf("execute fixed in-memory query: %w", err)
	}
	if err := verifyDuckDBVersion("SQL", sqlVersion); err != nil {
		return result, err
	}
	if sqlVersion != cAPIVersion {
		return result, fmt.Errorf("DuckDB version readbacks disagree: SQL=%q C_API=%q", sqlVersion, cAPIVersion)
	}
	if exactValue != positiveExactValue || marker != "pm-native-probe" {
		return result, fmt.Errorf("fixed in-memory exact-value readback mismatch: integer=%d marker=%q", exactValue, marker)
	}

	temporaryDirectory, err := os.MkdirTemp("", "pm-nativeprobe-")
	if err != nil {
		return result, fmt.Errorf("create owned temporary directory: %w", err)
	}
	defer os.RemoveAll(temporaryDirectory)
	info, err := os.Stat(temporaryDirectory)
	if err != nil {
		return result, fmt.Errorf("stat owned temporary directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return result, fmt.Errorf("temporary directory is not owner-private: mode=%#o", info.Mode().Perm())
	}
	parquetPath := temporaryDirectory + string(os.PathSeparator) + "roundtrip.parquet"
	if err := writeFixedParquet(ctx, database, parquetPath); err != nil {
		return result, err
	}
	values, labels, err := readFixedParquet(ctx, database, parquetPath)
	if err != nil {
		return result, err
	}
	if len(values) != 2 || values[0] != negativeExactValue || values[1] != positiveExactValue || labels[0] != "negative" || labels[1] != "positive" {
		return result, fmt.Errorf("Parquet exact-value readback mismatch: values=%v labels=%v", values, labels)
	}

	exactValues := make([]string, len(values))
	for index, value := range values {
		exactValues[index] = strconv.FormatInt(value, 10)
	}
	result = nativeProbeResult{
		SchemaVersion:       nativeProbeSchema,
		Status:              "pass",
		Platform:            platform,
		PlatformSupport:     platformSupportLabel(platform),
		DuckDBVersionSQL:    sqlVersion,
		DuckDBVersionCAPI:   cAPIVersion,
		DriverModule:        driver,
		BindingsModule:      bindings,
		NativeLibraryModule: nativeLibrary,
		ExactValues:         exactValues,
		ParquetRows:         len(values),
	}
	return result, nil
}

func writeFixedParquet(ctx context.Context, database *sql.DB, parquetPath string) error {
	statements := []struct {
		query string
		args  []any
	}{
		{query: "CREATE TABLE pm_native_probe (exact_id BIGINT NOT NULL, label VARCHAR NOT NULL)"},
		{query: "INSERT INTO pm_native_probe VALUES (?, ?), (?, ?)", args: []any{negativeExactValue, "negative", positiveExactValue, "positive"}},
		{query: "COPY pm_native_probe TO '" + quoteDuckDBString(parquetPath) + "' (FORMAT PARQUET)"},
	}
	for _, statement := range statements {
		if _, err := database.ExecContext(ctx, statement.query, statement.args...); err != nil {
			return fmt.Errorf("write fixed Parquet round trip: %w", err)
		}
	}
	info, err := os.Lstat(parquetPath)
	if err != nil {
		return fmt.Errorf("stat written Parquet file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("DuckDB did not create a nonempty regular Parquet file")
	}
	return nil
}

func readFixedParquet(ctx context.Context, database *sql.DB, parquetPath string) ([]int64, []string, error) {
	query := "SELECT exact_id, label FROM read_parquet('" + quoteDuckDBString(parquetPath) + "') ORDER BY exact_id"
	rows, err := database.QueryContext(ctx, query)
	if err != nil {
		return nil, nil, fmt.Errorf("open fixed Parquet round trip: %w", err)
	}
	defer rows.Close()
	var values []int64
	var labels []string
	for rows.Next() {
		var value int64
		var label string
		if err := rows.Scan(&value, &label); err != nil {
			return nil, nil, fmt.Errorf("scan fixed Parquet round trip: %w", err)
		}
		values = append(values, value)
		labels = append(labels, label)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read fixed Parquet round trip: %w", err)
	}
	return values, labels, nil
}

func quoteDuckDBString(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}

func linkedModuleIdentity(path string) (moduleIdentity, error) {
	buildInfo, ok := debug.ReadBuildInfo()
	if !ok {
		return moduleIdentity{}, errors.New("Go build information is unavailable; native source identity cannot be verified")
	}
	for _, dependency := range buildInfo.Deps {
		if dependency.Path != path {
			continue
		}
		if dependency.Replace != nil {
			return moduleIdentity{}, fmt.Errorf("linked module %s uses replacement %s; native qualification requires the locked source", path, dependency.Replace.Path)
		}
		return moduleIdentity{Path: dependency.Path, Version: dependency.Version, BuildInfoSum: dependency.Sum}, nil
	}
	return moduleIdentity{}, fmt.Errorf("linked module %s is missing from Go build information", path)
}

func verifyModuleIdentity(actual moduleIdentity, expected expectedModuleIdentity) error {
	if actual.Path != expected.path || actual.Version != expected.version {
		return fmt.Errorf("native module identity mismatch: got %s@%s, want %s@%s", actual.Path, actual.Version, expected.path, expected.version)
	}
	if actual.BuildInfoSum != "" && actual.BuildInfoSum != expected.sum {
		return fmt.Errorf("native module source sum mismatch for %s@%s: got %s, want %s", actual.Path, actual.Version, actual.BuildInfoSum, expected.sum)
	}
	return nil
}

func reportModuleIdentity(actual moduleIdentity, expected expectedModuleIdentity) moduleIdentity {
	actual.LockedSourceSum = expected.sum
	if actual.BuildInfoSum == "" {
		actual.SumEvidence = "not-embedded-by-build-system;source-identity-depends-on-the-verified-module-lock"
	} else {
		actual.SumEvidence = "embedded-build-info-sum-matched-the-verified-module-lock"
	}
	return actual
}

func verifyDuckDBVersion(source, actual string) error {
	if actual != expectedDuckDBVersion {
		return fmt.Errorf("DuckDB %s version mismatch: got %q, want %q", source, actual, expectedDuckDBVersion)
	}
	return nil
}

func platformSupportLabel(platform string) string {
	switch platform {
	case "linux-amd64", "linux-arm64":
		return "backend-native-target-unexecuted-until-this-probe-passes-on-the-exact-target"
	case "darwin-amd64", "darwin-arm64":
		return "development-native-target-unexecuted-until-this-probe-passes;not-an-advertised-backend-release-target"
	default:
		return "unsupported"
	}
}

func sortedNativePlatforms() []string {
	platforms := make([]string, 0, len(nativeModules))
	for platform := range nativeModules {
		platforms = append(platforms, platform)
	}
	sort.Strings(platforms)
	return platforms
}
