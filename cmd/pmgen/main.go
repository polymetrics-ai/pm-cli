package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	pmcompile "github.com/polymetrics-ai/pm-cli/internal/compile"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const usageText = `usage: pmgen compile [options]

Required:
  --source-lock PATH           immutable connector source lock
  --capture SOURCE_ID=PATH     captured source bytes; repeat per source
  --license SOURCE_ID=PATH     captured license bytes; repeat per source
  --bundle-out PATH            deterministic runtime bundle
  --report-out PATH            raw/normalized fidelity report

Optional:
  --commands-out PATH          safe command manifest
  --semantics PATH             source-linked semantic amendments
  --previous-source-digest D   prior digest when source identity changed
  --source-diff PATH           explicit pointer-level source revision diff
  --foundation PATH            exact target FoundationContract; repeat
`

type repeatedFlag []string

func (values *repeatedFlag) String() string {
	return strings.Join(*values, ",")
}

func (values *repeatedFlag) Set(value string) error {
	*values = append(*values, value)
	return nil
}

type compileOptions struct {
	sourceLock           string
	captures             repeatedFlag
	licenses             repeatedFlag
	bundleOut            string
	reportOut            string
	commandsOut          string
	semantics            string
	previousSourceDigest string
	sourceDiff           string
	foundations          repeatedFlag
}

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "compile" {
		fmt.Fprint(stderr, usageText)
		return 2
	}
	options, err := parseCompileOptions(args[1:], stderr)
	if err != nil {
		fmt.Fprintf(stderr, "pmgen: %v\n", err)
		return 2
	}
	input, err := loadCompilationInput(options)
	if err != nil {
		fmt.Fprintf(stderr, "pmgen: %v\n", err)
		return 1
	}
	artifacts, err := pmcompile.Compile(ctx, input)
	if err != nil {
		fmt.Fprintf(stderr, "pmgen: compile: %v\n", err)
		return 1
	}
	outputs := []struct {
		path string
		data []byte
	}{
		{path: options.bundleOut, data: artifacts.Bundle},
		{path: options.reportOut, data: artifacts.Report},
	}
	if options.commandsOut != "" {
		outputs = append(outputs, struct {
			path string
			data []byte
		}{path: options.commandsOut, data: artifacts.CommandManifest})
	}
	for _, output := range outputs {
		if err := writeAtomic(output.path, output.data); err != nil {
			fmt.Fprintf(stderr, "pmgen: write %s: %v\n", output.path, err)
			return 1
		}
	}
	fmt.Fprintf(stdout, "compiled connector source into %s\n", options.bundleOut)
	return 0
}

func parseCompileOptions(args []string, stderr io.Writer) (compileOptions, error) {
	var options compileOptions
	set := flag.NewFlagSet("pmgen compile", flag.ContinueOnError)
	set.SetOutput(stderr)
	set.StringVar(&options.sourceLock, "source-lock", "", "immutable connector source lock")
	set.Var(&options.captures, "capture", "SOURCE_ID=PATH captured source bytes")
	set.Var(&options.licenses, "license", "SOURCE_ID=PATH captured license bytes")
	set.StringVar(&options.bundleOut, "bundle-out", "", "runtime bundle output")
	set.StringVar(&options.reportOut, "report-out", "", "fidelity report output")
	set.StringVar(&options.commandsOut, "commands-out", "", "safe command manifest output")
	set.StringVar(&options.semantics, "semantics", "", "semantic amendment document")
	set.StringVar(&options.previousSourceDigest, "previous-source-digest", "", "prior source digest")
	set.StringVar(&options.sourceDiff, "source-diff", "", "pointer-level source revision diff")
	set.Var(&options.foundations, "foundation", "exact target FoundationContract")
	set.Usage = func() { fmt.Fprint(stderr, usageText) }
	if err := set.Parse(args); err != nil {
		return compileOptions{}, err
	}
	if set.NArg() != 0 {
		return compileOptions{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(set.Args(), " "))
	}
	if options.sourceLock == "" || len(options.captures) == 0 || len(options.licenses) == 0 || options.bundleOut == "" || options.reportOut == "" {
		return compileOptions{}, errors.New("source-lock, capture, license, bundle-out and report-out are required")
	}
	paths := []string{options.bundleOut, options.reportOut}
	if options.commandsOut != "" {
		paths = append(paths, options.commandsOut)
	}
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return compileOptions{}, err
		}
		if _, duplicate := seen[absolute]; duplicate {
			return compileOptions{}, errors.New("output paths must be distinct")
		}
		seen[absolute] = struct{}{}
	}
	return options, nil
}

func loadCompilationInput(options compileOptions) (pmcompile.CompilationInput, error) {
	sourceLock, err := readRegularFile(options.sourceLock, contract.MaxContractDocumentBytes)
	if err != nil {
		return pmcompile.CompilationInput{}, fmt.Errorf("read source lock: %w", err)
	}
	captures, err := readAssignments(options.captures, pmcompile.MaxSourceDocumentBytes)
	if err != nil {
		return pmcompile.CompilationInput{}, fmt.Errorf("read capture: %w", err)
	}
	licenses, err := readAssignments(options.licenses, contract.MaxContractDocumentBytes)
	if err != nil {
		return pmcompile.CompilationInput{}, fmt.Errorf("read license: %w", err)
	}
	input := pmcompile.CompilationInput{
		SourceLock: sourceLock, Captures: pmcompile.Captures{Documents: captures, Licenses: licenses},
		PreviousSourceDigest: options.previousSourceDigest,
	}
	if options.semantics != "" {
		input.Semantics, err = readRegularFile(options.semantics, contract.MaxContractDocumentBytes)
		if err != nil {
			return pmcompile.CompilationInput{}, fmt.Errorf("read semantics: %w", err)
		}
	}
	if options.sourceDiff != "" {
		input.RevisionDiff, err = readRegularFile(options.sourceDiff, contract.MaxContractDocumentBytes)
		if err != nil {
			return pmcompile.CompilationInput{}, fmt.Errorf("read source diff: %w", err)
		}
	}
	for _, path := range options.foundations {
		document, readErr := readRegularFile(path, contract.MaxContractDocumentBytes)
		if readErr != nil {
			return pmcompile.CompilationInput{}, fmt.Errorf("read foundation %s: %w", path, readErr)
		}
		foundation, decodeErr := contract.DecodeFoundationContract(document)
		if decodeErr != nil {
			return pmcompile.CompilationInput{}, fmt.Errorf("decode foundation %s: %w", path, decodeErr)
		}
		input.TargetFoundations = append(input.TargetFoundations, foundation)
	}
	return input, nil
}

func readAssignments(values []string, maximum int64) (map[string][]byte, error) {
	result := make(map[string][]byte, len(values))
	for _, assignment := range values {
		pieces := strings.SplitN(assignment, "=", 2)
		if len(pieces) != 2 || pieces[0] == "" || pieces[1] == "" {
			return nil, fmt.Errorf("assignment %q must be SOURCE_ID=PATH", assignment)
		}
		if _, duplicate := result[pieces[0]]; duplicate {
			return nil, fmt.Errorf("source identity %q is duplicated", pieces[0])
		}
		data, err := readRegularFile(pieces[1], maximum)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", pieces[0], err)
		}
		result[pieces[0]] = data
	}
	return result, nil
}

func readRegularFile(path string, maximum int64) ([]byte, error) {
	entry, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !entry.Mode().IsRegular() {
		return nil, errors.New("path must name a regular file without symlinks")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(entry, opened) {
		return nil, errors.New("file identity changed while opening")
	}
	if opened.Size() > maximum {
		return nil, fmt.Errorf("file exceeds %d-byte limit", maximum)
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("file exceeds %d-byte limit", maximum)
	}
	return data, nil
}

func writeAtomic(path string, data []byte) error {
	if len(data) == 0 {
		return errors.New("refusing to write an empty artifact")
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".pmgen-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	clean := func() {
		temporary.Close()
		os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(0o644); err != nil {
		clean()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		clean()
		return err
	}
	if err := temporary.Sync(); err != nil {
		clean()
		return err
	}
	if err := temporary.Close(); err != nil {
		os.Remove(temporaryPath)
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		os.Remove(temporaryPath)
		return err
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer directoryHandle.Close()
	return directoryHandle.Sync()
}
