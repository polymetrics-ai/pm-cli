// Command pmproof executes one connector lane's authored proof cases through
// the actual public PM path and writes immutable receipts plus one derived
// report. It never fabricates evidence: a case that cannot obtain its exact
// required identity, provider observation or assertion outcome fails or stays
// blocked instead of passing.
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
	"syscall"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/proof"
)

const (
	maximumInventoryBytes = 64 << 20
	maximumBundleBytes    = 64 << 20
	usageFailureExit      = 2
	failedExit            = 1
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "pmproof: %v\n", err)
		if errors.Is(err, errUsage) {
			os.Exit(usageFailureExit)
		}
		os.Exit(failedExit)
	}
}

var errUsage = errors.New("invalid command usage")

type options struct {
	connector       string
	lane            string
	mode            string
	serverProfile   string
	profilesPath    string
	serverURL       string
	workspace       string
	connection      string
	bundle          string
	receipts        string
	lanesPath       string
	providerJournal string
	pmBinary        string
	apiBinary       string
	workerBinary    string
	queryHelper     string
	hostClass       string
	sessionMode     string
	requestTimeout  string
	waitTimeout     string
	caseTimeout     string
	cleanupTimeout  string
	storeTimeout    string
}

func run(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
	if ctx == nil || stdout == nil || stderr == nil {
		return fmt.Errorf("%w: proof execution requires a context and output", errUsage)
	}
	var parsed options
	flags := flag.NewFlagSet("pmproof", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&parsed.connector, "connector", "", "connector identity whose lane is proved")
	flags.StringVar(&parsed.lane, "lane", "", "exact lane identity to execute")
	flags.StringVar(&parsed.mode, "mode", "retry", "retry, all or unimplemented")
	flags.StringVar(&parsed.serverProfile, "profile", "", "named non-secret PM server profile")
	flags.StringVar(&parsed.profilesPath, "profiles-path", "", "absolute path to the PM server profile document")
	flags.StringVar(&parsed.serverURL, "server", "", "expected server origin of the selected profile")
	flags.StringVar(&parsed.workspace, "workspace", "", "explicit authorized workspace identity")
	flags.StringVar(&parsed.connection, "connection", "", "explicit authorized connection identity")
	flags.StringVar(&parsed.bundle, "bundle", "", "absolute path of the exact generated execution bundle installed in this deployment")
	flags.StringVar(&parsed.receipts, "receipts", "", "absolute path of the owned proof receipt directory")
	flags.StringVar(&parsed.lanesPath, "lane-inventory", "", "absolute path of the generated lane inventory artifact")
	flags.StringVar(&parsed.providerJournal, "provider-journal", "", "absolute path of the controlled provider observation journal")
	flags.StringVar(&parsed.pmBinary, "pm-binary", "", "absolute path of the actual PM client binary")
	flags.StringVar(&parsed.apiBinary, "api-binary", "", "absolute path of the actual PM API binary")
	flags.StringVar(&parsed.workerBinary, "worker-binary", "", "absolute path of the actual PM worker binary")
	flags.StringVar(&parsed.queryHelper, "query-helper", "", "optional absolute path of the actual query helper binary")
	flags.StringVar(&parsed.hostClass, "host-class", "", "safe execution host classification")
	flags.StringVar(&parsed.sessionMode, "session-store", "os", "os or memory PM session storage")
	flags.StringVar(&parsed.requestTimeout, "request-timeout", "30s", "per-request bound")
	flags.StringVar(&parsed.waitTimeout, "wait-timeout", "5m", "terminal projection bound")
	flags.StringVar(&parsed.caseTimeout, "case-timeout", "10m", "per-case execution bound")
	flags.StringVar(&parsed.cleanupTimeout, "cleanup-timeout", "2m", "per-case cleanup bound")
	flags.StringVar(&parsed.storeTimeout, "store-timeout", "30s", "receipt persistence bound")
	if err := flags.Parse(arguments); err != nil {
		return errUsage
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: unexpected positional arguments", errUsage)
	}
	mode, err := proof.ParseMode(parsed.mode)
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	lane := contract.Lane(parsed.lane)
	if err := contract.ValidateLane(lane); err != nil {
		return fmt.Errorf("%w: --lane must select one supported lane", errUsage)
	}
	if parsed.connector == "" || parsed.workspace == "" || parsed.connection == "" ||
		parsed.bundle == "" || parsed.receipts == "" || parsed.lanesPath == "" ||
		parsed.pmBinary == "" || parsed.apiBinary == "" || parsed.workerBinary == "" ||
		parsed.hostClass == "" || parsed.providerJournal == "" || parsed.serverURL == "" {
		return fmt.Errorf("%w: connector, lane inputs, artifacts, receipts, provider journal and host classification are required", errUsage)
	}
	if parsed.profilesPath == "" {
		parsed.profilesPath, err = defaultProfilesPath()
		if err != nil {
			return err
		}
	}
	timeouts, err := parseTimeouts(parsed)
	if err != nil {
		return err
	}
	bundleDocument, err := readBoundedFile(parsed.bundle, maximumBundleBytes)
	if err != nil {
		return fmt.Errorf("read declared execution bundle: %w", err)
	}
	inventoryDocument, err := readBoundedFile(parsed.lanesPath, maximumInventoryBytes)
	if err != nil {
		return fmt.Errorf("read generated lane inventory: %w", err)
	}
	inventory, err := proof.DecodeLaneInventory(inventoryDocument, bundleDocument, maximumInventoryBytes)
	if err != nil {
		return fmt.Errorf("qualify generated lane inventory: %w", err)
	}
	if inventory.ConnectorID != parsed.connector || inventory.Lane != lane {
		return fmt.Errorf("%w: generated inventory belongs to %s/%s", errUsage, inventory.ConnectorID, inventory.Lane)
	}
	inventoryHash, err := contract.CanonicalDigest(inventoryDocument)
	if err != nil {
		return fmt.Errorf("hash generated lane inventory: %w", err)
	}
	store, err := proof.OpenReceiptStore(parsed.receipts, maximumInventoryBytes)
	if err != nil {
		return err
	}
	defer store.Close()
	executor, err := proof.NewPMPathExecutor(ctx, proof.PMPathConfig{
		ProfilesPath: parsed.profilesPath, ProfileName: parsed.serverProfile,
		Workspace: parsed.workspace, Connection: parsed.connection, SessionMode: parsed.sessionMode,
		PMClientBinary: parsed.pmBinary, APIBinary: parsed.apiBinary, WorkerBinary: parsed.workerBinary,
		QueryHelperBinary: parsed.queryHelper, BundlePath: parsed.bundle,
		ProviderJournal: parsed.providerJournal, MaximumFileBytes: proof.ComponentIdentityHashBound,
		RequestTimeout: timeouts.request, WaitTimeout: timeouts.wait,
	})
	if err != nil {
		return err
	}
	if executor.Observation().ServerURL != parsed.serverURL {
		return fmt.Errorf("%w: selected server profile does not match --server", errUsage)
	}
	observed, err := executor.ObserveDeployment(ctx)
	if err != nil {
		return err
	}
	plans, err := planCases(inventory, observed, executor.Observation())
	if err != nil {
		return err
	}
	if len(plans) == 0 {
		return errors.New("selected lane declares no authored executable case")
	}
	result, err := proof.RunLane(ctx, proof.RunnerConfig{
		Store: store, Executor: executor, Mode: mode, HostClass: parsed.hostClass,
		CaseTimeout: timeouts.caseRun, CleanupTimeout: timeouts.cleanup, PersistenceTimeout: timeouts.store,
	}, inventoryHash, plans)
	for _, entry := range result.Report.Cases {
		_, _ = fmt.Fprintf(stdout, "%s/%s/%s %s %s\n",
			entry.Identity.OperationID, entry.Identity.BehaviorID, entry.Identity.CaseID,
			entry.Availability, entry.Reason)
	}
	if err != nil {
		return err
	}
	if result.Failed {
		return errors.New("at least one required proof case did not pass")
	}
	return nil
}

type resolvedTimeouts struct {
	request time.Duration
	wait    time.Duration
	caseRun time.Duration
	cleanup time.Duration
	store   time.Duration
}

func parseTimeouts(parsed options) (resolvedTimeouts, error) {
	var result resolvedTimeouts
	for value, destination := range map[string]*time.Duration{
		parsed.requestTimeout: &result.request, parsed.waitTimeout: &result.wait,
		parsed.caseTimeout: &result.caseRun, parsed.cleanupTimeout: &result.cleanup,
		parsed.storeTimeout: &result.store,
	} {
		parsedValue, err := time.ParseDuration(value)
		if err != nil || parsedValue <= 0 {
			return resolvedTimeouts{}, fmt.Errorf("%w: every timeout must be a positive duration", errUsage)
		}
		*destination = parsedValue
	}
	return result, nil
}

func planCases(inventory proof.LaneInventory, observed proof.DeploymentObservation, artifacts proof.ExecutorObservation) ([]proof.CasePlan, error) {
	plans := make([]proof.CasePlan, 0, len(inventory.Operations))
	for _, operation := range inventory.Operations {
		for _, candidate := range operation.Cases {
			scenario := candidate.Scenario
			assertionDocument, err := json.Marshal(scenario.Assertions)
			if err != nil {
				return nil, err
			}
			assertionHash, err := contract.CanonicalDigest(assertionDocument)
			if err != nil {
				return nil, err
			}
			versions, observedVersions := observed.HandlerVersions[scenario.OperationID]
			if !observedVersions {
				return nil, errors.New("generated lane declares an operation the deployment does not expose")
			}
			current := proof.Currentness{
				Identity: proof.CaseIdentity{
					ConnectorID: observed.ConnectorID, Lane: invocationLane(inventory), OperationID: scenario.OperationID,
					BehaviorID: candidate.BehaviorID, CaseID: candidate.CaseID,
					ExecutionKind: "fake", ScopeHash: observed.ScopeHash,
				},
				SourceHashes:    observed.SourceHashes,
				BundleHash:      artifacts.BundleHash,
				SchemaHash:      artifacts.BundleHash,
				PMClientHash:    artifacts.PMClientHash,
				PMAPIHash:       artifacts.APIHash,
				PMWorkerHash:    artifacts.WorkerHash,
				AssertionHash:   assertionHash,
				QueryHelperHash: artifacts.QueryHelperHash,
				HandlerVersions: versions,
			}
			plans = append(plans, proof.CasePlan{
				Candidate: proof.CaseCandidate{Current: current, Availability: operation.Status, Reason: operation.Reason},
				Scenario:  candidateScenario(candidate), StepIdentities: map[string]proof.Currentness{},
			})
		}
	}
	return plans, nil
}

func candidateScenario(candidate proof.InventoryCase) *contract.Scenario {
	owned := candidate.Scenario
	return &owned
}

func invocationLane(inventory proof.LaneInventory) contract.Lane {
	return inventory.Lane
}

func defaultProfilesPath() (string, error) {
	directory, err := os.UserConfigDir()
	if err != nil || directory == "" {
		return "", errUsage
	}
	return directory + "/pm/profiles.json", nil
}

func readBoundedFile(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximum {
		return nil, errors.New("declared artifact is not a bounded regular file")
	}
	document, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(document)) != info.Size() {
		return nil, errors.New("declared artifact changed while it was read")
	}
	return document, nil
}
