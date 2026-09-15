package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	pmtools "github.com/polymetrics-ai/pm-cli/tools"
)

const checkTimeout = 5 * time.Minute

type repeatedStrings []string

func (values *repeatedStrings) String() string {
	return strings.Join(*values, ",")
}

func (values *repeatedStrings) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("path must not be empty")
	}
	*values = append(*values, value)
	return nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("pm-dependency-check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: pm-dependency-check --root ROOT --versions FILE --toolchains FILE --sbom FILE [--sbom FILE ...]")
		fmt.Fprintln(stderr, "Resolve both Go module graphs with GOWORK=off and check current-platform assets plus selected Bazel-module SBOM inventories.")
		fmt.Fprintln(stderr, "Each SBOM subject must use its locked asset id or Bazel-module name and exact version; archive source integrity remains separate from installed-file SHA-256.")
		fmt.Fprintln(stderr, "Recognized packages and hashed installed files are reported separately; file inventory does not assert recognized-package dependency coverage.")
		flags.PrintDefaults()
	}
	root := flags.String("root", "", "repository root containing go.mod and tools/go.mod")
	versions := flags.String("versions", "", "pm.dependencies/v1 policy JSON")
	toolchains := flags.String("toolchains", "", "pm.toolchains/v1 lock JSON")
	var sboms repeatedStrings
	flags.Var(&sboms, "sbom", "CycloneDX package/file or SPDX package inventory; repeat for multiple artifacts")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "pm-dependency-check: unexpected positional arguments: %s\n", strings.Join(flags.Args(), " "))
		return 2
	}
	if *root == "" || *versions == "" || *toolchains == "" {
		fmt.Fprintln(stderr, "pm-dependency-check: --root, --versions, and --toolchains are required")
		flags.Usage()
		return 2
	}
	if len(sboms) == 0 {
		fmt.Fprintln(stderr, "pm-dependency-check: at least one --sbom is required; zero external evidence is not a full dependency-policy result")
		return 2
	}

	signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signalContext, checkTimeout)
	defer cancel()
	result, err := pmtools.CheckDependencies(ctx, pmtools.CheckOptions{
		Root:           *root,
		VersionsPath:   *versions,
		ToolchainsPath: *toolchains,
		SBOMPaths:      sboms,
	})
	if err != nil {
		fmt.Fprintf(stderr, "pm-dependency-check: %v\n", err)
		return 1
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		fmt.Fprintf(stderr, "pm-dependency-check: write result: %v\n", err)
		return 1
	}
	return 0
}
