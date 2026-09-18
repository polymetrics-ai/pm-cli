package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	for _, source := range snapshot.Commands {
		descriptor := source
		if err := validateDescriptorForTree(snapshot.Binding, descriptor); err != nil {
			return err
		}
		if len(descriptor.AvailableLanes) == 0 {
			continue
		}
		for _, name := range append([]string{descriptor.Name}, descriptor.Aliases...) {
			if _, collision := used[name]; collision {
				return fmt.Errorf("%w: generated command name or alias %q collides with the executable tree", errUsage, name)
			}
		}
		command, err := projectedCommand(snapshot.Binding, descriptor, invoke)
		if err != nil {
			return err
		}
		root.AddCommand(command)
		used[descriptor.Name] = struct{}{}
		for _, alias := range descriptor.Aliases {
			used[alias] = struct{}{}
		}
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
			return renderWireResponse(command.OutOrStdout(), bundle.config.output, response)
		},
	})
	return command
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
	options.Catalog = &snapshot
	options.CatalogCache = cache
	options.prepared = &preparedCatalog{bundle: bundle}
	return options, nil
}

func dynamicCommandCandidate(root *cobra.Command, arguments []string) bool {
	if root == nil {
		return false
	}
	for _, token := range arguments {
		present, enabled, valid := explicitHelpFlag(token)
		if present && (!valid || enabled) {
			return false
		}
	}
	for index := 0; index < len(arguments); index++ {
		token := arguments[index]
		if present, _, _ := explicitHelpFlag(token); present {
			continue
		}
		_, inline, kind, recognized := rootControlToken(token)
		if recognized {
			if inline == nil && kind == "string" {
				index++
			}
			continue
		}
		if strings.HasPrefix(token, "-") || token == "help" {
			return false
		}
		for _, command := range root.Commands() {
			if token == command.Name() {
				return false
			}
			for _, alias := range command.Aliases {
				if token == alias {
					return false
				}
			}
		}
		return true
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
	for index := 0; index < len(arguments); index++ {
		token := arguments[index]
		if token == "--" {
			return nil
		}
		name, inline, kind, recognized := rootControlToken(token)
		if !recognized {
			continue
		}
		if kind == "bool" {
			value := "true"
			if inline != nil {
				value = *inline
			}
			if err := setBootstrapBool(flags, name, value); err != nil {
				return err
			}
			continue
		}
		var value string
		if inline != nil {
			value = *inline
		} else {
			if index+1 >= len(arguments) || arguments[index+1] == "" || strings.HasPrefix(arguments[index+1], "--") {
				return fmt.Errorf("%w: --%s requires one bounded non-empty value", errUsage, name)
			}
			index++
			value = arguments[index]
		}
		if err := setBootstrapString(flags, name, value); err != nil {
			return err
		}
	}
	return nil
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
