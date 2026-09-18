package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	pmapi "github.com/polymetrics-ai/pm-cli/internal/api"
	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/spf13/cobra"
)

const (
	dynamicOperationAnnotation = "pm.polymetrics.ai/operation-id"
	dynamicRevisionAnnotation  = "pm.polymetrics.ai/operation-revision"
	dynamicCatalogAnnotation   = "pm.polymetrics.ai/catalog-revision"
)

type DynamicInvocation struct {
	Binding    pmapi.CatalogBinding
	Descriptor pmapi.CommandDescriptor
	Lane       contract.Lane
	Input      contract.PresentValue
	RequestKey string
	Output     io.Writer
}

type DynamicInvoker func(context.Context, DynamicInvocation) error

type preparedCatalog struct {
	bundle clientBundle
}

type dynamicFlagValue struct {
	name string
	raw  string
	set  bool
}

func (value *dynamicFlagValue) Set(raw string) error {
	if value.set {
		return fmt.Errorf("%w: --%s may be specified only once", errUsage, value.name)
	}
	if len(raw) > contract.MaxContractDocumentBytes || !utf8.ValidString(raw) {
		return fmt.Errorf("%w: --%s exceeds the typed value boundary", errUsage, value.name)
	}
	value.raw = raw
	value.set = true
	return nil
}

func (value *dynamicFlagValue) String() string { return value.raw }
func (*dynamicFlagValue) Type() string         { return "typed-value" }

func BuildCommandTree(root *cobra.Command, snapshot pmapi.CatalogSnapshot, invoke DynamicInvoker) error {
	if root == nil || invoke == nil {
		return errors.New("dynamic command tree dependencies are required")
	}
	if err := validateSnapshotBinding(snapshot.Binding); err != nil || snapshot.Commands == nil {
		return pmapi.ErrCatalogEnvelope
	}
	used := make(map[string]struct{})
	for _, existing := range root.Commands() {
		used[existing.Name()] = struct{}{}
		for _, alias := range existing.Aliases {
			used[alias] = struct{}{}
		}
	}
	projected := make([]*cobra.Command, 0, len(snapshot.Commands))
	for _, source := range snapshot.Commands {
		descriptor := source
		if err := validateDescriptorForTree(snapshot.Binding, descriptor); err != nil {
			return err
		}
		if len(descriptor.AvailableLanes) == 0 {
			continue
		}
		if _, collision := used[descriptor.Name]; collision {
			return fmt.Errorf("%w: generated command name or alias %q collides with the executable tree", errUsage, descriptor.Name)
		}
		for _, alias := range descriptor.Aliases {
			if _, collision := used[alias]; collision {
				return fmt.Errorf("%w: generated command name or alias %q collides with the executable tree", errUsage, alias)
			}
		}
		command, err := projectedCommand(snapshot.Binding, descriptor, invoke)
		if err != nil {
			return err
		}
		projected = append(projected, command)
		used[descriptor.Name] = struct{}{}
		for _, alias := range descriptor.Aliases {
			used[alias] = struct{}{}
		}
	}
	if len(projected) != 0 {
		root.AddCommand(projected...)
	}
	return nil
}

func projectedCommand(binding pmapi.CatalogBinding, descriptor pmapi.CommandDescriptor, invoke DynamicInvoker) (*cobra.Command, error) {
	values := make([]dynamicFlagValue, len(descriptor.Fields))
	bindings := make([]contract.FieldBinding, len(descriptor.Fields))
	var laneValue, requestKey dynamicFlagValue
	laneValue.name = "lane"
	requestKey.name = "request-key"
	command := &cobra.Command{
		Use:     descriptor.Name,
		Aliases: append([]string(nil), descriptor.Aliases...),
		Short:   "Invoke " + descriptor.OperationId + " through its typed generated command",
		Args:    noArgs,
		Annotations: map[string]string{
			dynamicOperationAnnotation: descriptor.OperationId,
			dynamicRevisionAnnotation:  descriptor.OperationRevision,
			dynamicCatalogAnnotation:   descriptor.CatalogRevision,
		},
		RunE: func(command *cobra.Command, _ []string) error {
			input := make(map[string]contract.PresentValue, len(descriptor.Fields))
			for index, field := range descriptor.Fields {
				if !values[index].set {
					continue
				}
				value, err := parseDynamicValue(values[index].raw, field.Binding.Schema)
				if err != nil {
					return usageFailure(fmt.Errorf("--%s: %w", values[index].name, err))
				}
				input[field.Binding.FieldID] = value
			}
			typedInput := contract.NewObjectValue(input)
			if err := descriptor.RequestBody.ValidateInput(bindings, typedInput); err != nil {
				return usageFailure(err)
			}
			lane := contract.Lane(descriptor.AvailableLanes[0])
			if len(descriptor.AvailableLanes) > 1 {
				if !laneValue.set {
					return usageFailure(errors.New("--lane is required when a command has multiple available lanes"))
				}
				lane = contract.Lane(laneValue.raw)
				available := false
				for _, candidate := range descriptor.AvailableLanes {
					if contract.Lane(candidate) == lane {
						available = true
						break
					}
				}
				if !available {
					return usageFailure(errors.New("--lane does not identify an available lane"))
				}
			}
			invocation := DynamicInvocation{
				Binding:    binding,
				Descriptor: descriptor,
				Lane:       lane,
				Input:      typedInput,
				Output:     command.OutOrStdout(),
			}
			if requestKey.set {
				invocation.RequestKey = requestKey.raw
			}
			return invoke(command.Context(), invocation)
		},
	}
	for index, field := range descriptor.Fields {
		name := strings.TrimPrefix(field.Flag, "--")
		values[index].name = name
		bindings[index] = field.Binding
		command.Flags().Var(&values[index], name, dynamicFieldUsage(field.Binding))
	}
	if len(descriptor.AvailableLanes) > 1 {
		command.Flags().Var(&laneValue, laneValue.name, "Exact available execution lane")
	}
	command.Flags().Var(&requestKey, requestKey.name, "Optional caller-owned recovery key for an intentional replay; omitted keys are durably allocated")
	return command, nil
}

func validateSnapshotBinding(binding pmapi.CatalogBinding) error {
	if !boundedIdentity(binding.ServerInstanceID, 512) || !boundedIdentity(string(binding.WorkspaceID), 512) ||
		!boundedIdentity(binding.ConnectionID, 512) || contract.SemanticDigest(binding.CatalogRevision).Validate() != nil ||
		!binding.Principal.Kind.Valid() || !boundedIdentity(binding.Principal.PrincipalId, 512) {
		return pmapi.ErrCatalogEnvelope
	}
	return nil
}

func validateDescriptorForTree(binding pmapi.CatalogBinding, descriptor pmapi.CommandDescriptor) error {
	if !commandSegment(descriptor.Name) || !boundedIdentity(descriptor.ConnectorId, 512) ||
		descriptor.ConnectionId != binding.ConnectionID || !boundedIdentity(descriptor.OperationId, 1024) ||
		contract.SemanticDigest(descriptor.OperationRevision).Validate() != nil || descriptor.CatalogRevision != binding.CatalogRevision ||
		descriptor.Aliases == nil || descriptor.Fields == nil || descriptor.AvailableLanes == nil {
		return pmapi.ErrCatalogEnvelope
	}
	seenNames := map[string]struct{}{descriptor.Name: {}}
	for _, alias := range descriptor.Aliases {
		if !commandSegment(alias) {
			return pmapi.ErrCatalogEnvelope
		}
		if _, duplicate := seenNames[alias]; duplicate {
			return fmt.Errorf("%w: generated command alias %q is duplicated", errUsage, alias)
		}
		seenNames[alias] = struct{}{}
	}
	seenFlags := make(map[string]struct{}, len(descriptor.Fields))
	seenFields := make(map[string]struct{}, len(descriptor.Fields))
	bindings := make([]contract.FieldBinding, len(descriptor.Fields))
	type wireUse struct {
		location contract.FieldLocation
		flag     string
	}
	seenWire := make(map[string]wireUse, len(descriptor.Fields))
	for index, field := range descriptor.Fields {
		bindings[index] = field.Binding
		if !longFlag(field.Flag) || field.Binding.Validate() != nil || field.Binding.Sensitivity != contract.SensitivityPublic ||
			(field.Binding.Location == contract.FieldLocationBody && field.Binding.JSONPointer == "") {
			return pmapi.ErrCatalogEnvelope
		}
		if _, reserved := reservedProjectionFlags[field.Flag]; reserved {
			return fmt.Errorf("%w: provider flag %s collides with a PM control", errUsage, field.Flag)
		}
		if _, duplicate := seenFlags[field.Flag]; duplicate {
			return fmt.Errorf("%w: generated flag %s is duplicated", errUsage, field.Flag)
		}
		if _, duplicate := seenFields[field.Binding.FieldID]; duplicate {
			return pmapi.ErrCatalogEnvelope
		}
		wireKey := field.Binding.WireName
		if prior, present := seenWire[wireKey]; present && prior.location != field.Binding.Location && prior.flag == field.Flag {
			return fmt.Errorf("%w: provider wire name %q requires distinct location aliases", errUsage, wireKey)
		}
		seenFlags[field.Flag] = struct{}{}
		seenFields[field.Binding.FieldID] = struct{}{}
		seenWire[wireKey] = wireUse{location: field.Binding.Location, flag: field.Flag}
	}
	if err := descriptor.RequestBody.Validate(bindings); err != nil {
		return pmapi.ErrCatalogEnvelope
	}
	seenLanes := make(map[pmapi.CommandDescriptorAvailableLanes]struct{}, len(descriptor.AvailableLanes))
	for _, lane := range descriptor.AvailableLanes {
		if contract.ValidateLane(contract.Lane(lane)) != nil {
			return pmapi.ErrCatalogEnvelope
		}
		if _, duplicate := seenLanes[lane]; duplicate {
			return pmapi.ErrCatalogEnvelope
		}
		seenLanes[lane] = struct{}{}
	}
	if len(descriptor.AvailableLanes) == 0 && descriptor.UnavailabilityCode == nil {
		return pmapi.ErrCatalogEnvelope
	}
	if len(descriptor.AvailableLanes) != 0 && descriptor.UnavailabilityCode != nil {
		return pmapi.ErrCatalogEnvelope
	}
	return nil
}

var reservedProjectionFlags = map[string]struct{}{
	"--connection":      {},
	"--help":            {},
	"--json":            {},
	"--lane":            {},
	"--output":          {},
	"--request-key":     {},
	"--request-timeout": {},
	"--server-profile":  {},
	"--session-store":   {},
	"--wait":            {},
	"--workspace":       {},
}

func dynamicFieldUsage(binding contract.FieldBinding) string {
	usage := "Typed " + string(binding.Schema.Type) + " value for " + binding.FieldID + " (" + string(binding.Location) + ":" + binding.WireName + ")"
	if binding.Nullable {
		usage += "; use @null for explicit null"
	}
	return usage
}

func parseDynamicValue(raw string, schema contract.Schema) (contract.PresentValue, error) {
	var value contract.PresentValue
	var err error
	if raw == "@null" {
		value = contract.NullValue()
	} else {
		switch schema.Type {
		case contract.SchemaTypeString:
			value = contract.NewStringValue(raw)
		case contract.SchemaTypeBool:
			if raw != "true" && raw != "false" {
				return contract.PresentValue{}, errors.New("boolean must be exactly true or false")
			}
			value = contract.NewBooleanValue(raw == "true")
		case contract.SchemaTypeInteger:
			value, err = contract.NewIntegerValue(raw)
		case contract.SchemaTypeDecimal:
			precision, scale := decimalShape(raw)
			value, err = contract.NewDecimalValue(raw, precision, scale)
		case contract.SchemaTypeDate:
			value, err = contract.NewDateValue(raw)
		case contract.SchemaTypeTimestampInstant:
			value, err = contract.NewInstantValue(raw)
		case contract.SchemaTypeTimestampLocal:
			value, err = contract.NewLocalTimestampValue(raw)
		case contract.SchemaTypeArray, contract.SchemaTypeObject, contract.SchemaTypeUnion, contract.SchemaTypeBinaryRef:
			value, err = contract.DecodePresentValue([]byte(raw), "application/json")
		default:
			return contract.PresentValue{}, errors.New("descriptor type is unsupported")
		}
	}
	if err != nil {
		return contract.PresentValue{}, err
	}
	if err := contract.ValidateValueAgainstSchema(value, schema); err != nil {
		return contract.PresentValue{}, err
	}
	return value, nil
}

func decimalShape(value string) (int, int) {
	unsigned := strings.TrimPrefix(value, "-")
	parts := strings.Split(unsigned, ".")
	if len(parts) != 2 {
		return 0, 0
	}
	return len(parts[0]) + len(parts[1]), len(parts[1])
}

func commandSegment(value string) bool {
	return boundedIdentity(value, 512) && !strings.ContainsAny(value, " /\\") && !strings.HasPrefix(value, "-")
}

func longFlag(value string) bool {
	if !strings.HasPrefix(value, "--") || len(value) < 3 || strings.ContainsAny(value, " =\t\r\n") {
		return false
	}
	for _, character := range strings.TrimPrefix(value, "--") {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

func boundedIdentity(value string, maximum int) bool {
	if value == "" || len(value) > maximum || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character <= 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func newConnectorsCommand(runtime *commandRuntime) *cobra.Command {
	command := &cobra.Command{Use: "connectors", Short: "Inspect the authorized immutable connector catalog", Args: noArgs}
	command.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List safe connector descriptors and actual lane availability",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			response, err := bundle.client.API().ListCatalogConnectorsWithResponse(ctx, pmapi.WorkspaceID(bundle.config.workspace), nil)
			if err != nil {
				return requestError(ctx, err)
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	})
	command.AddCommand(&cobra.Command{
		Use:   "inspect CONNECTOR_ID",
		Short: "Inspect one safe connector descriptor",
		Args:  exactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			response, err := bundle.client.API().GetCatalogConnectorWithResponse(ctx, pmapi.WorkspaceID(bundle.config.workspace), arguments[0])
			if err != nil {
				return requestError(ctx, err)
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	})
	return command
}

func newCommandsCommand(runtime *commandRuntime) *cobra.Command {
	var filter singleString
	filter.name = "filter"
	command := &cobra.Command{Use: "commands", Short: "Inspect connection-authorized generated commands", Args: noArgs}
	list := &cobra.Command{
		Use:   "list",
		Short: "List safe command descriptors for the selected connection",
		Args:  noArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			if bundle.config.connection == "" {
				return usageFailure(errors.New("--connection is required for this command"))
			}
			params := &pmapi.ListCatalogCommandsParams{ConnectionId: bundle.config.connection}
			if filter.set {
				params.Filter = &filter.value
			}
			response, err := bundle.client.API().ListCatalogCommandsWithResponse(ctx, pmapi.WorkspaceID(bundle.config.workspace), params)
			if err != nil {
				return requestError(ctx, err)
			}
			if response.JSON200 != nil {
				runtime.cacheDiscoveredCatalog(ctx, bundle, command.ErrOrStderr())
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	}
	list.Flags().Var(&filter, filter.name, "Exact server-side command name or operation filter")
	command.AddCommand(list)
	command.AddCommand(&cobra.Command{
		Use:   "inspect OPERATION_ID",
		Short: "Inspect one available command for the selected connection",
		Args:  exactArgs(1),
		RunE: func(command *cobra.Command, arguments []string) error {
			ctx, cancel, bundle, err := runtime.requestBundle(command, true, true)
			if err != nil {
				return err
			}
			defer cancel()
			if bundle.config.connection == "" {
				return usageFailure(errors.New("--connection is required for this command"))
			}
			params := &pmapi.GetCatalogCommandParams{ConnectionId: bundle.config.connection}
			response, err := bundle.client.API().GetCatalogCommandWithResponse(ctx, pmapi.WorkspaceID(bundle.config.workspace), arguments[0], params)
			if err != nil {
				return requestError(ctx, err)
			}
			if response.JSON200 != nil {
				runtime.cacheDiscoveredCatalog(ctx, bundle, command.ErrOrStderr())
			}
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	})
	return command
}

func (runtime *commandRuntime) cacheDiscoveredCatalog(ctx context.Context, bundle clientBundle, writer io.Writer) {
	// The requested discovery response may be one page, a filtered result, or
	// one operation. Only a complete, unfiltered, qualified snapshot may replace
	// offline documentation; preserve the original requested output separately.
	snapshot, err := pmapi.LoadCatalog(
		ctx, bundle.client, runtime.options.CatalogCache,
		pmapi.WorkspaceID(bundle.config.workspace), bundle.config.connection, "",
	)
	if err == nil {
		err = storeCatalogProjection(runtime.options, bundle.config.profile, snapshot)
	}
	if err != nil {
		_, _ = fmt.Fprintln(writer, "notice: read-only discovery succeeded, but a complete offline catalog projection is unavailable")
	}
}

func prepareDynamicCatalog(ctx context.Context, options Options) (Options, error) {
	flags := &rootFlags{}
	if err := applyBootstrapFlags(options.Args, flags); err != nil {
		return options, usageFailure(err)
	}
	runtime := &commandRuntime{options: options, flags: flags}
	config, err := runtime.resolveConfig(true)
	if err != nil {
		return options, err
	}
	if config.connection == "" {
		return options, usageFailure(errors.New("--connection is required for a generated command"))
	}
	requestContext, cancel := context.WithTimeout(ctx, config.requestTimeout)
	defer cancel()
	bundle, err := runtime.clientForConfig(requestContext, config, true)
	if err != nil {
		return options, err
	}
	cache := options.CatalogCache
	if cache == nil {
		cache = pmapi.NewCatalogCache()
	}
	snapshot, err := pmapi.LoadCatalog(requestContext, bundle.client, cache, pmapi.WorkspaceID(config.workspace), config.connection, "")
	if err != nil {
		return options, err
	}
	if err := storeCatalogProjection(options, config.profile, snapshot); err != nil {
		writer := options.Stderr
		if writer == nil {
			writer = io.Discard
		}
		_, _ = fmt.Fprintln(writer, "notice: fresh catalog was qualified, but its offline documentation projection could not be stored safely")
	}
	options.Catalog = &snapshot
	options.CatalogCache = cache
	options.prepared = &preparedCatalog{bundle: bundle}
	return options, nil
}

func dynamicCommandCandidate(root *cobra.Command, arguments []string) bool {
	if root == nil {
		return false
	}
	scan, err := scanBoundedArguments(arguments, nil)
	if err != nil || scan.helpEnabled || scan.helpInvalid || len(scan.positionals) == 0 {
		return false
	}
	name := scan.positionals[0]
	return name != "help" && !rootCommandNamed(root, name)
}

func offlineDocumentationCandidate(root *cobra.Command, arguments []string) bool {
	if root == nil {
		return false
	}
	scan, err := scanBoundedArguments(arguments, nil)
	if err != nil || scan.helpInvalid {
		return false
	}
	if len(scan.positionals) == 0 {
		return scan.helpEnabled
	}
	switch scan.positionals[0] {
	case "manual", "completion":
		return true
	case "help":
		if len(scan.positionals) == 1 {
			return true
		}
		return !rootCommandNamed(root, scan.positionals[1])
	default:
		return scan.helpEnabled && !rootCommandNamed(root, scan.positionals[0])
	}
}

func rootCommandNamed(root *cobra.Command, name string) bool {
	for _, command := range root.Commands() {
		if name == command.Name() {
			return true
		}
		for _, alias := range command.Aliases {
			if name == alias {
				return true
			}
		}
	}
	return false
}

func explicitHelpFlag(token string) (present, enabled, valid bool) {
	if token == "-h" || token == "--help" {
		return true, true, true
	}
	for _, prefix := range []string{"-h=", "--help="} {
		if !strings.HasPrefix(token, prefix) {
			continue
		}
		enabled, err := strconv.ParseBool(strings.TrimPrefix(token, prefix))
		return true, enabled, err == nil
	}
	return false, false, true
}

func applyBootstrapFlags(arguments []string, flags *rootFlags) error {
	if flags == nil {
		return errUsage
	}
	_, err := scanBoundedArguments(arguments, func(name, kind, value string) error {
		if kind == "bool" {
			return setBootstrapBool(flags, name, value)
		}
		return setBootstrapString(flags, name, value)
	})
	return err
}

func rootControlToken(token string) (string, *string, string, bool) {
	for _, candidate := range []struct {
		name string
		kind string
	}{
		{name: "server-profile", kind: "string"}, {name: "workspace", kind: "string"},
		{name: "connection", kind: "string"}, {name: "output", kind: "string"},
		{name: "json", kind: "bool"}, {name: "wait", kind: "bool"},
		{name: "request-timeout", kind: "string"}, {name: "session-store", kind: "string"},
	} {
		plain := "--" + candidate.name
		if token == plain {
			return candidate.name, nil, candidate.kind, true
		}
		if strings.HasPrefix(token, plain+"=") {
			value := strings.TrimPrefix(token, plain+"=")
			return candidate.name, &value, candidate.kind, true
		}
	}
	return "", nil, "", false
}

func setBootstrapString(flags *rootFlags, name, value string) error {
	var target *singleString
	switch name {
	case "server-profile":
		target = &flags.serverProfile
	case "workspace":
		target = &flags.workspace
	case "connection":
		target = &flags.connection
	case "output":
		target = &flags.output
	case "request-timeout":
		target = &flags.requestTimeout
	case "session-store":
		target = &flags.sessionStore
	default:
		return errUsage
	}
	target.name = name
	return target.Set(value)
}

func setBootstrapBool(flags *rootFlags, name, value string) error {
	var target *singleBool
	switch name {
	case "json":
		target = &flags.json
	case "wait":
		target = &flags.wait
	default:
		return errUsage
	}
	target.name = name
	return target.Set(value)
}

const (
	catalogProjectionSchemaVersion = "pm.cli-catalog-projection/v1"
	catalogProjectionDirectory     = "catalog-projections"
	maximumCatalogProjectionBytes  = contract.MaxContractDocumentBytes
)

// A persisted projection is always stale: only fresh server qualification can
// establish execution authority. Its full binding is retained solely to make
// offline command documentation explicit and auditable.
type catalogProjectionStatus string

const (
	catalogProjectionMissing catalogProjectionStatus = "missing"
	catalogProjectionStale   catalogProjectionStatus = "stale"
	catalogProjectionCorrupt catalogProjectionStatus = "corrupt"
	catalogProjectionForeign catalogProjectionStatus = "foreign"
)

type catalogProjectionWire struct {
	SchemaVersion    string                    `json:"schema_version"`
	ProfileName      string                    `json:"profile_name"`
	ServerURL        string                    `json:"server_url"`
	SessionHandle    string                    `json:"session_handle"`
	ServerInstanceID string                    `json:"server_instance_id"`
	Principal        pmapi.Principal           `json:"principal"`
	WorkspaceID      pmapi.WorkspaceID         `json:"workspace_id"`
	ConnectionID     string                    `json:"connection_id"`
	CatalogRevision  string                    `json:"catalog_revision"`
	Commands         []pmapi.CommandDescriptor `json:"commands"`
}

type offlineCatalogProjection struct {
	snapshot *pmapi.CatalogSnapshot
	status   catalogProjectionStatus
}

func projectOfflineCatalog(root *cobra.Command, options Options) {
	projection := loadOfflineCatalogProjection(options)
	if projection.snapshot != nil {
		err := BuildCommandTree(root, *projection.snapshot, func(context.Context, DynamicInvocation) error {
			return usageFailure(errors.New("cached catalog metadata cannot authorize execution"))
		})
		if err != nil {
			projection = offlineCatalogProjection{status: catalogProjectionCorrupt}
		}
	}
	reportOfflineCatalogProjection(options.Stderr, projection)
}

func reportOfflineCatalogProjection(writer io.Writer, projection offlineCatalogProjection) {
	if writer == nil {
		writer = io.Discard
	}
	switch projection.status {
	case catalogProjectionStale:
		binding := projection.snapshot.Binding
		_, _ = fmt.Fprintf(
			writer,
			"notice: generated documentation uses cached catalog revision %s bound to principal %s/%s; cached metadata is stale until server revalidation and is not current execution authority\n",
			binding.CatalogRevision,
			binding.Principal.Kind,
			binding.Principal.PrincipalId,
		)
	case catalogProjectionCorrupt:
		_, _ = fmt.Fprintln(writer, "notice: generated command metadata is unavailable because the scoped cached catalog projection is corrupt or unsafe")
	case catalogProjectionForeign:
		_, _ = fmt.Fprintln(writer, "notice: generated command metadata is unavailable because the cached projection does not match the explicit server, profile, workspace, and connection")
	default:
		_, _ = fmt.Fprintln(writer, "notice: generated command metadata is unavailable because no matching scoped cached catalog projection was selected")
	}
}

func loadOfflineCatalogProjection(options Options) offlineCatalogProjection {
	selectors, err := ParseSelectors(options.Args)
	if err != nil || selectors.ServerProfile == "" || selectors.Workspace == "" || selectors.Connection == "" {
		return offlineCatalogProjection{status: catalogProjectionMissing}
	}
	profilesPath, err := catalogProfilesPath(options)
	if err != nil {
		return offlineCatalogProjection{status: catalogProjectionMissing}
	}
	profile, err := pmapi.LoadServerProfile(profilesPath, selectors.ServerProfile)
	if err != nil {
		return offlineCatalogProjection{status: catalogProjectionMissing}
	}
	path, err := catalogProjectionPath(
		profilesPath,
		profile,
		pmapi.WorkspaceID(selectors.Workspace),
		selectors.Connection,
	)
	if err != nil {
		return offlineCatalogProjection{status: catalogProjectionMissing}
	}
	wire, status := readCatalogProjection(path)
	if status != catalogProjectionStale {
		return offlineCatalogProjection{status: status}
	}
	if wire.ProfileName != profile.Name || wire.ServerURL != profile.ServerURL ||
		wire.SessionHandle != profile.SessionHandle || wire.ServerInstanceID != profile.InstanceID ||
		wire.WorkspaceID != pmapi.WorkspaceID(selectors.Workspace) || wire.ConnectionID != selectors.Connection {
		return offlineCatalogProjection{status: catalogProjectionForeign}
	}
	snapshot := pmapi.CatalogSnapshot{
		Binding: pmapi.CatalogBinding{
			ServerInstanceID: wire.ServerInstanceID,
			Principal:        wire.Principal,
			WorkspaceID:      wire.WorkspaceID,
			ConnectionID:     wire.ConnectionID,
			CatalogRevision:  wire.CatalogRevision,
		},
		Commands: wire.Commands,
	}
	copy, err := validatedCatalogProjectionSnapshot(snapshot)
	if err != nil {
		return offlineCatalogProjection{status: catalogProjectionCorrupt}
	}
	return offlineCatalogProjection{snapshot: &copy, status: catalogProjectionStale}
}

func storeCatalogProjection(options Options, profile pmapi.ServerProfile, snapshot pmapi.CatalogSnapshot) error {
	copy, err := validatedCatalogProjectionSnapshot(snapshot)
	if err != nil || copy.Binding.ServerInstanceID != profile.InstanceID {
		return pmapi.ErrCatalogEnvelope
	}
	profilesPath, err := catalogProfilesPath(options)
	if err != nil {
		return err
	}
	path, err := catalogProjectionPath(profilesPath, profile, copy.Binding.WorkspaceID, copy.Binding.ConnectionID)
	if err != nil {
		return err
	}
	wire := catalogProjectionWire{
		SchemaVersion:    catalogProjectionSchemaVersion,
		ProfileName:      profile.Name,
		ServerURL:        profile.ServerURL,
		SessionHandle:    profile.SessionHandle,
		ServerInstanceID: copy.Binding.ServerInstanceID,
		Principal:        copy.Binding.Principal,
		WorkspaceID:      copy.Binding.WorkspaceID,
		ConnectionID:     copy.Binding.ConnectionID,
		CatalogRevision:  copy.Binding.CatalogRevision,
		Commands:         copy.Commands,
	}
	document, err := json.Marshal(wire)
	if err != nil || len(document)+1 > maximumCatalogProjectionBytes {
		return pmapi.ErrCatalogEnvelope
	}
	document = append(document, '\n')
	return writeCatalogProjection(path, document)
}

func validatedCatalogProjectionSnapshot(snapshot pmapi.CatalogSnapshot) (pmapi.CatalogSnapshot, error) {
	cache := pmapi.NewCatalogCache()
	if err := cache.Store(snapshot); err != nil {
		return pmapi.CatalogSnapshot{}, err
	}
	copy, present := cache.Load(snapshot.Binding)
	if !present {
		return pmapi.CatalogSnapshot{}, pmapi.ErrCatalogEnvelope
	}
	return copy, nil
}

func catalogProfilesPath(options Options) (string, error) {
	path := options.ProfilesPath
	if path == "" {
		var err error
		path, err = pmapi.DefaultProfilesPath()
		if err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(path) {
		return "", errors.New("catalog projection requires an absolute profile path")
	}
	return filepath.Clean(path), nil
}

func catalogProjectionPath(
	profilesPath string,
	profile pmapi.ServerProfile,
	workspaceID pmapi.WorkspaceID,
	connectionID string,
) (string, error) {
	if profilesPath == "" || !filepath.IsAbs(profilesPath) || profile.Name == "" ||
		profile.ServerURL == "" || profile.InstanceID == "" || profile.SessionHandle == "" ||
		!boundedIdentity(string(workspaceID), 512) || !boundedIdentity(connectionID, 512) {
		return "", pmapi.ErrCatalogEnvelope
	}
	digest := sha256.New()
	for _, part := range []string{
		catalogProjectionSchemaVersion,
		profile.Name,
		profile.ServerURL,
		profile.InstanceID,
		profile.SessionHandle,
		string(workspaceID),
		connectionID,
	} {
		_, _ = io.WriteString(digest, part)
		_, _ = digest.Write([]byte{0})
	}
	name := hex.EncodeToString(digest.Sum(nil)) + ".json"
	return filepath.Join(filepath.Dir(profilesPath), catalogProjectionDirectory, name), nil
}

func readCatalogProjection(path string) (catalogProjectionWire, catalogProjectionStatus) {
	directoryInformation, directoryErr := os.Lstat(filepath.Dir(path))
	if errors.Is(directoryErr, os.ErrNotExist) {
		return catalogProjectionWire{}, catalogProjectionMissing
	}
	if directoryErr != nil || directoryInformation.Mode()&os.ModeSymlink != 0 ||
		!directoryInformation.IsDir() || directoryInformation.Mode().Perm()&0o077 != 0 {
		return catalogProjectionWire{}, catalogProjectionCorrupt
	}
	information, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return catalogProjectionWire{}, catalogProjectionMissing
	}
	if err != nil || information.Mode()&os.ModeSymlink != 0 || !information.Mode().IsRegular() ||
		information.Mode().Perm()&0o077 != 0 || information.Size() <= 0 ||
		information.Size() > int64(maximumCatalogProjectionBytes) {
		return catalogProjectionWire{}, catalogProjectionCorrupt
	}
	file, err := os.Open(path)
	if err != nil {
		return catalogProjectionWire{}, catalogProjectionCorrupt
	}
	defer file.Close()
	document, err := io.ReadAll(io.LimitReader(file, int64(maximumCatalogProjectionBytes)+1))
	if err != nil || len(document) == 0 || len(document) > maximumCatalogProjectionBytes {
		return catalogProjectionWire{}, catalogProjectionCorrupt
	}
	var wire catalogProjectionWire
	if err := contract.DecodeStrictRequiredObject(
		document,
		&wire,
		"schema_version",
		"profile_name",
		"server_url",
		"session_handle",
		"server_instance_id",
		"principal",
		"workspace_id",
		"connection_id",
		"catalog_revision",
		"commands",
	); err != nil || wire.SchemaVersion != catalogProjectionSchemaVersion {
		return catalogProjectionWire{}, catalogProjectionCorrupt
	}
	return wire, catalogProjectionStale
}

func writeCatalogProjection(path string, document []byte) error {
	if path == "" || !filepath.IsAbs(path) || len(document) == 0 || len(document) > maximumCatalogProjectionBytes {
		return errors.New("catalog projection destination is invalid")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return errors.New("catalog projection directory is unavailable")
	}
	information, err := os.Lstat(directory)
	if err != nil || information.Mode()&os.ModeSymlink != 0 || !information.IsDir() ||
		information.Mode().Perm()&0o077 != 0 {
		return errors.New("catalog projection directory is unsafe")
	}
	if information, err := os.Lstat(path); err == nil {
		if information.Mode()&os.ModeSymlink != 0 || !information.Mode().IsRegular() ||
			information.Mode().Perm()&0o077 != 0 {
			return errors.New("existing catalog projection is unsafe")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("catalog projection destination is unavailable")
	}
	temporary, err := os.CreateTemp(directory, ".catalog-*")
	if err != nil {
		return errors.New("catalog projection temporary file is unavailable")
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return errors.New("catalog projection permissions could not be set")
	}
	if _, err := temporary.Write(document); err != nil {
		temporary.Close()
		return errors.New("catalog projection could not be written")
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return errors.New("catalog projection could not be synchronized")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("catalog projection could not be closed")
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return errors.New("catalog projection could not be committed")
	}
	return nil
}

func (runtime *commandRuntime) invokeDynamic(ctx context.Context, invocation DynamicInvocation) error {
	if runtime == nil || runtime.options.prepared == nil || ctx == nil || invocation.Output == nil {
		return usageFailure(errors.New("generated command execution requires a freshly qualified catalog"))
	}
	config, err := runtime.resolveConfig(true)
	if err != nil {
		return err
	}
	prepared := runtime.options.prepared.bundle
	if config.workspace != string(invocation.Binding.WorkspaceID) || config.connection != invocation.Binding.ConnectionID ||
		config.profile.InstanceID != invocation.Binding.ServerInstanceID || config.profile.ServerURL != prepared.config.profile.ServerURL ||
		config.profile.SessionHandle != prepared.config.profile.SessionHandle || config.sessionMode != prepared.config.sessionMode {
		return usageFailure(errors.New("execution selectors differ from the qualified catalog binding"))
	}
	payload := struct {
		ConnectionRef  string                  `json:"connection_ref"`
		Input          contract.PresentValue   `json:"input"`
		Lane           contract.Lane           `json:"lane"`
		OperationRef   string                  `json:"operation_ref"`
		ResultControls contract.ResultControls `json:"result_controls"`
	}{
		ConnectionRef:  invocation.Binding.ConnectionID,
		Input:          invocation.Input,
		Lane:           invocation.Lane,
		OperationRef:   invocation.Descriptor.OperationId,
		ResultControls: contract.ResultControls{},
	}
	target := invocation.Binding.ConnectionID + "/" + invocation.Descriptor.OperationId
	canonical, err := json.Marshal(struct {
		ExpectedRevision string `json:"expected_revision"`
		Operation        string `json:"operation"`
		Payload          any    `json:"payload"`
		Target           string `json:"target"`
		Workspace        string `json:"workspace"`
	}{
		ExpectedRevision: invocation.Descriptor.OperationRevision,
		Operation:        "invocation.submit",
		Payload:          payload,
		Target:           target,
		Workspace:        config.workspace,
	})
	if err != nil {
		return err
	}
	intentDigest, err := contract.CanonicalDigest(canonical)
	if err != nil {
		return err
	}
	selection, err := pmapi.SelectRequestKey(invocation.RequestKey)
	if err != nil {
		if errors.Is(err, pmapi.ErrInvalidRequestKey) {
			return usageFailure(errors.New("--request-key is invalid"))
		}
		return err
	}
	requestKey := selection.RequestKey
	protocol := pmapi.ProtocolMetadata{
		ExpectedRevision: invocation.Descriptor.OperationRevision,
		IntentDigest:     string(intentDigest),
		RequestKey:       requestKey,
		RequiredFeatures: []string{"catalog-v1", "exact-values"},
		WireVersion:      pmapi.ProtocolMetadataWireVersion(pmapi.WireVersion),
	}
	request := pmapi.InvocationRequest{
		ConnectionRef:  invocation.Binding.ConnectionID,
		Input:          invocation.Input,
		Lane:           pmapi.InvocationRequestLane(invocation.Lane),
		OperationRef:   invocation.Descriptor.OperationId,
		Protocol:       protocol,
		ResultControls: contract.ResultControls{},
	}
	recovery, err := runtime.recoveryStore()
	if err != nil {
		return err
	}
	response, err := prepared.client.SubmitInvocationWithRecovery(ctx, recovery, invocation.Binding.WorkspaceID, &pmapi.SubmitInvocationParams{
		IdempotencyKey: pmapi.IdempotencyKey(requestKey),
	}, request)
	accepted, err := recoveredMutationResponse(requestKey, response, err)
	if err != nil {
		return err
	}
	return renderWireResponse(invocation.Output, config.output, accepted)
}
