package temporal

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	failurepb "go.temporal.io/api/failure/v1"
	sdkclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/interceptor"
	sdktemporal "go.temporal.io/sdk/temporal"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

const maxWorkflowPayloadBytes = 64 << 10

type safeWorkflowDataConverter struct {
	base converter.DataConverter
}

func newSafeWorkflowDataConverter() converter.DataConverter {
	return &safeWorkflowDataConverter{base: converter.GetDefaultDataConverter()}
}

func (dataConverter *safeWorkflowDataConverter) ToPayload(value interface{}) (*commonpb.Payload, error) {
	if err := validateWorkflowValue(value); err != nil {
		return nil, err
	}
	payload, err := dataConverter.base.ToPayload(value)
	if err != nil {
		return nil, err
	}
	if err := validateWorkflowPayload(payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func (dataConverter *safeWorkflowDataConverter) FromPayload(payload *commonpb.Payload, valuePtr interface{}) error {
	if !supportedWorkflowValue(valuePtr) || nilWorkflowValue(valuePtr) {
		return errors.New("workflow payload target is not an admitted safe-reference contract")
	}
	if err := validateWorkflowPayload(payload); err != nil {
		return err
	}
	if err := dataConverter.base.FromPayload(payload, valuePtr); err != nil {
		return err
	}
	return validateWorkflowValue(valuePtr)
}

func (dataConverter *safeWorkflowDataConverter) ToPayloads(values ...interface{}) (*commonpb.Payloads, error) {
	if len(values) == 0 {
		return nil, nil
	}
	payloads := &commonpb.Payloads{Payloads: make([]*commonpb.Payload, 0, len(values))}
	totalBytes := 0
	for index, value := range values {
		payload, err := dataConverter.ToPayload(value)
		if err != nil {
			return nil, fmt.Errorf("workflow payload %d: %w", index, err)
		}
		totalBytes += len(payload.GetData())
		if totalBytes > maxWorkflowPayloadBytes {
			return nil, errors.New("workflow payload collection exceeds the admitted safe-reference byte bound")
		}
		payloads.Payloads = append(payloads.Payloads, payload)
	}
	return payloads, nil
}

func (dataConverter *safeWorkflowDataConverter) FromPayloads(payloads *commonpb.Payloads, valuePtrs ...interface{}) error {
	if payloads == nil {
		if len(valuePtrs) != 0 {
			return errors.New("workflow payloads are absent")
		}
		return nil
	}
	if len(payloads.GetPayloads()) != len(valuePtrs) {
		return errors.New("workflow payload count does not match the typed contract")
	}
	totalBytes := 0
	for index, payload := range payloads.GetPayloads() {
		if !supportedWorkflowValue(valuePtrs[index]) || nilWorkflowValue(valuePtrs[index]) {
			return fmt.Errorf("workflow payload %d target is not an admitted safe-reference contract", index)
		}
		if err := validateWorkflowPayload(payload); err != nil {
			return fmt.Errorf("workflow payload %d: %w", index, err)
		}
		totalBytes += len(payload.GetData())
		if totalBytes > maxWorkflowPayloadBytes {
			return errors.New("workflow payload collection exceeds the admitted safe-reference byte bound")
		}
	}
	for index, payload := range payloads.GetPayloads() {
		if err := dataConverter.FromPayload(payload, valuePtrs[index]); err != nil {
			return fmt.Errorf("workflow payload %d: %w", index, err)
		}
	}
	return nil
}

func (*safeWorkflowDataConverter) ToString(*commonpb.Payload) string {
	return "<safe-workflow-payload>"
}

func (*safeWorkflowDataConverter) ToStrings(payloads *commonpb.Payloads) []string {
	if payloads == nil {
		return nil
	}
	redacted := make([]string, len(payloads.GetPayloads()))
	for index := range redacted {
		redacted[index] = "<safe-workflow-payload>"
	}
	return redacted
}

func validateWorkflowPayload(payload *commonpb.Payload) error {
	if payload == nil {
		return errors.New("workflow payload is required")
	}
	metadata := payload.GetMetadata()
	if len(metadata) != 1 || string(metadata[converter.MetadataEncoding]) != converter.MetadataEncodingJSON {
		return errors.New("workflow payload encoding is not canonical JSON")
	}
	if len(payload.GetData()) == 0 || len(payload.GetData()) > maxWorkflowPayloadBytes {
		return errors.New("workflow payload exceeds the admitted safe-reference byte bound")
	}
	return nil
}

func validateWorkflowValue(value interface{}) error {
	if !supportedWorkflowValue(value) || nilWorkflowValue(value) {
		return errors.New("workflow payload type is not an admitted safe-reference contract")
	}
	validator, ok := value.(interface{ Validate() error })
	if !ok {
		return errors.New("workflow payload type has no contract validator")
	}
	return validator.Validate()
}

func nilWorkflowValue(value interface{}) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	return reflected.Kind() == reflect.Ptr && reflected.IsNil()
}

func supportedWorkflowValue(value interface{}) bool {
	switch value.(type) {
	case contract.StartRunInput, *contract.StartRunInput,
		contract.ProjectionRetryCommand, *contract.ProjectionRetryCommand,
		contract.WorkflowRunRef, *contract.WorkflowRunRef,
		contract.WorkflowUnitRef, *contract.WorkflowUnitRef,
		contract.WorkflowCommandRef, *contract.WorkflowCommandRef,
		contract.ActivityTimeouts, *contract.ActivityTimeouts,
		contract.WorkflowRunSnapshot, *contract.WorkflowRunSnapshot,
		contract.WorkflowUnitRequest, *contract.WorkflowUnitRequest,
		contract.WorkflowUnitResult, *contract.WorkflowUnitResult,
		contract.WorkflowCommandRequest, *contract.WorkflowCommandRequest,
		contract.WorkflowCommandResult, *contract.WorkflowCommandResult,
		contract.WorkflowProjectionRequest, *contract.WorkflowProjectionRequest,
		contract.WorkflowProjectionResult, *contract.WorkflowProjectionResult,
		contract.WorkflowProgress, *contract.WorkflowProgress,
		contract.ScheduledRunInput, *contract.ScheduledRunInput:
		return true
	default:
		return false
	}
}

type safeWorkflowFailureConverter struct {
	base converter.FailureConverter
}

func newSafeWorkflowFailureConverter() converter.FailureConverter {
	return &safeWorkflowFailureConverter{
		base: sdktemporal.NewDefaultFailureConverter(sdktemporal.DefaultFailureConverterOptions{
			DataConverter: newSafeWorkflowDataConverter(),
		}),
	}
}

func (failureConverter *safeWorkflowFailureConverter) ErrorToFailure(err error) (sanitized *failurepb.Failure) {
	defer func() {
		if recover() != nil {
			sanitized = safeWorkflowConversionFailure()
		}
	}()
	failure := failureConverter.base.ErrorToFailure(err)
	if failure == nil {
		return nil
	}
	sanitized = proto.Clone(failure).(*failurepb.Failure)
	sanitizeWorkflowFailure(sanitized)
	return sanitized
}

func safeWorkflowConversionFailure() *failurepb.Failure {
	return &failurepb.Failure{
		Message: "workflow application failure",
		FailureInfo: &failurepb.Failure_ApplicationFailureInfo{
			ApplicationFailureInfo: &failurepb.ApplicationFailureInfo{
				Type:         "WorkflowApplicationFailure",
				NonRetryable: true,
			},
		},
	}
}

func (failureConverter *safeWorkflowFailureConverter) FailureToError(failure *failurepb.Failure) (converted error) {
	defer func() {
		if recover() != nil {
			converted = errors.New("workflow application failure")
		}
	}()
	if failure == nil {
		return nil
	}
	clone := proto.Clone(failure).(*failurepb.Failure)
	sanitizeWorkflowFailure(clone)
	return failureConverter.base.FailureToError(clone)
}

func sanitizeWorkflowFailure(failure *failurepb.Failure) {
	if failure == nil {
		return
	}
	summary := workflowFailureSummary(failure)
	scrubProtoStrings(failure.ProtoReflect())
	failure.Message = summary
	failure.Source = ""
	failure.StackTrace = ""
	failure.EncodedAttributes = nil
	failure.Cause = nil
	if info := failure.GetApplicationFailureInfo(); info != nil {
		info.Details = nil
		info.Type = "WorkflowApplicationFailure"
	}
	if info := failure.GetCanceledFailureInfo(); info != nil {
		info.Details = nil
	}
	if info := failure.GetTimeoutFailureInfo(); info != nil {
		info.LastHeartbeatDetails = nil
	}
	if info := failure.GetResetWorkflowFailureInfo(); info != nil {
		info.LastHeartbeatDetails = nil
	}
}

func workflowFailureSummary(failure *failurepb.Failure) string {
	switch {
	case failure.GetCanceledFailureInfo() != nil:
		return "workflow operation canceled"
	case failure.GetTimeoutFailureInfo() != nil:
		return "workflow operation timed out"
	case failure.GetActivityFailureInfo() != nil:
		return "workflow activity failed"
	case failure.GetChildWorkflowExecutionFailureInfo() != nil:
		return "child workflow failed"
	case failure.GetTerminatedFailureInfo() != nil:
		return "workflow terminated"
	case failure.GetServerFailureInfo() != nil:
		return "Temporal server operation failed"
	default:
		return "workflow application failure"
	}
}

func scrubProtoStrings(message protoreflect.Message) {
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		switch {
		case field.IsMap():
			message.Clear(field)
		case field.IsList():
			list := value.List()
			for index := range list.Len() {
				switch field.Kind() {
				case protoreflect.StringKind:
					list.Set(index, protoreflect.ValueOfString(""))
				case protoreflect.BytesKind:
					list.Set(index, protoreflect.ValueOfBytes(nil))
				case protoreflect.MessageKind, protoreflect.GroupKind:
					scrubProtoStrings(list.Get(index).Message())
				}
			}
		case field.Kind() == protoreflect.StringKind || field.Kind() == protoreflect.BytesKind:
			message.Clear(field)
		case field.Kind() == protoreflect.MessageKind || field.Kind() == protoreflect.GroupKind:
			scrubProtoStrings(value.Message())
		}
		return true
	})
}

type safeWorkflowClientInterceptor struct {
	interceptor.ClientInterceptorBase
}

var _ interceptor.ClientInterceptor = (*safeWorkflowClientInterceptor)(nil)

func (*safeWorkflowClientInterceptor) InterceptClient(
	next interceptor.ClientOutboundInterceptor,
) interceptor.ClientOutboundInterceptor {
	return &safeWorkflowClientOutboundInterceptor{
		ClientOutboundInterceptorBase: interceptor.ClientOutboundInterceptorBase{Next: next},
	}
}

type safeWorkflowClientOutboundInterceptor struct {
	interceptor.ClientOutboundInterceptorBase
}

var _ interceptor.ClientOutboundInterceptor = (*safeWorkflowClientOutboundInterceptor)(nil)

func (outbound *safeWorkflowClientOutboundInterceptor) ExecuteWorkflow(
	ctx context.Context,
	input *interceptor.ClientExecuteWorkflowInput,
) (sdkclient.WorkflowRun, error) {
	if input == nil || input.Options == nil {
		return nil, errors.New("workflow start options are required")
	}
	if err := validateWorkflowStartMetadata(*input.Options); err != nil {
		return nil, err
	}
	return outbound.Next.ExecuteWorkflow(ctx, input)
}

func (outbound *safeWorkflowClientOutboundInterceptor) SignalWithStartWorkflow(
	ctx context.Context,
	input *interceptor.ClientSignalWithStartWorkflowInput,
) (sdkclient.WorkflowRun, error) {
	if input == nil || input.Options == nil {
		return nil, errors.New("workflow start options are required")
	}
	if err := validateWorkflowStartMetadata(*input.Options); err != nil {
		return nil, err
	}
	return outbound.Next.SignalWithStartWorkflow(ctx, input)
}

func (*safeWorkflowClientOutboundInterceptor) UpdateWithStartWorkflow(
	context.Context,
	*interceptor.ClientUpdateWithStartWorkflowInput,
) (sdkclient.WorkflowUpdateHandle, error) {
	return nil, errors.New("workflow update-with-start is not admitted")
}

func (*safeWorkflowClientOutboundInterceptor) CreateSchedule(
	context.Context,
	*interceptor.ScheduleClientCreateInput,
) (sdkclient.ScheduleHandle, error) {
	return nil, errors.New("workflow schedule creation is not admitted")
}

func validateWorkflowStartMetadata(options sdkclient.StartWorkflowOptions) error {
	if len(options.Memo) != 0 ||
		len(options.SearchAttributes) != 0 ||
		options.TypedSearchAttributes.Size() != 0 ||
		options.StaticSummary != "" ||
		options.StaticDetails != "" {
		return errors.New("workflow start memo and search attributes are not admitted")
	}
	return nil
}

// DialWorkerClient installs the closed safe-reference payload and failure
// converters, rejects unowned persisted start metadata, and makes one
// context-bounded Temporal connection attempt.
func DialWorkerClient(
	ctx context.Context,
	options sdkclient.Options,
	timeout time.Duration,
) (sdkclient.Client, error) {
	if ctx == nil {
		return nil, errors.New("Temporal worker dial context is required")
	}
	if timeout <= 0 || timeout > 5*time.Minute {
		return nil, errors.New("Temporal worker dial timeout is invalid")
	}
	if options.ConnectionOptions.TLS != nil {
		options.ConnectionOptions.TLS = options.ConnectionOptions.TLS.Clone()
	}
	options.DataConverter = newSafeWorkflowDataConverter()
	options.FailureConverter = newSafeWorkflowFailureConverter()
	interceptors := make([]interceptor.ClientInterceptor, len(options.Interceptors), len(options.Interceptors)+1)
	copy(interceptors, options.Interceptors)
	options.Interceptors = append(interceptors, &safeWorkflowClientInterceptor{})
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return sdkclient.DialContext(dialCtx, options)
}
