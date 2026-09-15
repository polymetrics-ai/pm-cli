package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNativeProbe(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("locate native test executable: %v", err)
	}
	privateRoot := t.TempDir()
	temporaryDirectory := filepath.Join(privateRoot, "tmp")
	if err := os.Mkdir(temporaryDirectory, 0o700); err != nil {
		t.Fatalf("create child temporary directory: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable)
	command.Env = []string{
		"HOME=" + privateRoot,
		"LANG=C",
		"PATH=/usr/bin:/bin",
		"PM_NATIVEPROBE_CHILD=1",
		"TMPDIR=" + temporaryDirectory,
		"TZ=UTC",
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			t.Fatalf("native probe child exceeded bound: %v", ctx.Err())
		}
		t.Fatalf("native probe child failed: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	if stderr.Len() != 0 {
		t.Fatalf("native probe child wrote unexpected diagnostics: %s", strings.TrimSpace(stderr.String()))
	}
	entries, err := os.ReadDir(temporaryDirectory)
	if err != nil {
		t.Fatalf("inspect child temporary directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("native probe left owned temporary artifacts behind: %v", entries)
	}
	decoder := json.NewDecoder(&stdout)
	decoder.DisallowUnknownFields()
	var result nativeProbeResult
	if err := decoder.Decode(&result); err != nil {
		t.Fatalf("decode native probe child result: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("native probe child returned trailing output: %v", err)
	}
	if result.SchemaVersion != nativeProbeSchema || result.Status != "pass" {
		t.Fatalf("native probe result identity = %s/%s; want %s/pass", result.SchemaVersion, result.Status, nativeProbeSchema)
	}
	platform := runtime.GOOS + "-" + runtime.GOARCH
	if result.Platform != platform || result.PlatformSupport != platformSupportLabel(platform) {
		t.Fatalf("native probe platform = %s (%s); want %s (%s)", result.Platform, result.PlatformSupport, platform, platformSupportLabel(platform))
	}
	if result.DuckDBVersionSQL != expectedDuckDBVersion || result.DuckDBVersionCAPI != expectedDuckDBVersion {
		t.Fatalf("DuckDB versions = SQL %q C API %q; want %q", result.DuckDBVersionSQL, result.DuckDBVersionCAPI, expectedDuckDBVersion)
	}
	wantValues := []string{"-9007199254740993", "9007199254740993"}
	if !reflect.DeepEqual(result.ExactValues, wantValues) || result.ParquetRows != 2 {
		t.Fatalf("Parquet readback = values %v rows %d; want %v rows 2", result.ExactValues, result.ParquetRows, wantValues)
	}

	nativeExpected, ok := nativeModules[platform]
	if !ok {
		t.Fatalf("actual test platform %s was not refused before child execution", platform)
	}
	assertModuleReport(t, result.DriverModule, expectedModuleIdentity{path: driverModulePath, version: driverModuleVersion, sum: driverModuleSum})
	assertModuleReport(t, result.BindingsModule, expectedModuleIdentity{path: bindingsModulePath, version: bindingsModuleVersion, sum: bindingsModuleSum})
	assertModuleReport(t, result.NativeLibraryModule, nativeExpected)
	wrongActual := result.NativeLibraryModule
	if wrongActual.BuildInfoSum == "" {
		wrongActual.BuildInfoSum = nativeExpected.sum
	}
	nativeExpected.sum = strings.Repeat("A", len(nativeExpected.sum))
	if err := verifyModuleIdentity(wrongActual, nativeExpected); err == nil || !strings.Contains(err.Error(), "source sum mismatch") {
		t.Fatalf("wrong native asset sum error = %v; want source sum mismatch", err)
	}
	if err := verifyDuckDBVersion("negative control", "v0.0.0"); err == nil || !strings.Contains(err.Error(), "version mismatch") {
		t.Fatalf("wrong DuckDB version error = %v; want version mismatch", err)
	}
}

func assertModuleReport(t *testing.T, actual moduleIdentity, expected expectedModuleIdentity) {
	t.Helper()
	if actual.Path != expected.path || actual.Version != expected.version || actual.LockedSourceSum != expected.sum {
		t.Fatalf("native module report = %#v; want %s@%s with locked source sum %s", actual, expected.path, expected.version, expected.sum)
	}
	if actual.BuildInfoSum == "" {
		if actual.SumEvidence != "not-embedded-by-build-system;source-identity-depends-on-the-verified-module-lock" {
			t.Fatalf("missing build-info sum evidence = %q; want explicit lock dependency", actual.SumEvidence)
		}
		return
	}
	if actual.BuildInfoSum != expected.sum || actual.SumEvidence != "embedded-build-info-sum-matched-the-verified-module-lock" {
		t.Fatalf("embedded module sum report = %#v; want matched locked source sum", actual)
	}
}

func TestMain(tests *testing.M) {
	if os.Getenv("PM_NATIVEPROBE_CHILD") != "1" {
		os.Exit(tests.Run())
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	err := runNativeProbe(ctx, os.Stdout)
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "nativeprobe child: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}
