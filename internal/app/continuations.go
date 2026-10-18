package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"strings"
	"sync"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
	"github.com/polymetrics-ai/pm-cli/internal/objects"
	pmruntime "github.com/polymetrics-ai/pm-cli/internal/runtime"
	"github.com/polymetrics-ai/pm-cli/internal/store"
)

const (
	continuationEnvelopeSchema    = "pm.http-continuation/v1"
	continuationEnvelopeMediaType = "application/vnd.polymetrics.pm-continuation+json"
)

var ErrContinuationInvalid = errors.New("durable continuation is invalid")

// ContinuationService is the server-owned PostgreSQL/S3 continuation codec. It
// accepts no caller-supplied object reference: an opaque token is resolved
// through canonical worker settlement lineage before verified bytes are read.
type ContinuationService struct {
	pool         *store.Pool
	publisher    *objects.Publisher
	backend      *objects.S3
	maximumBytes int64
}

// NewContinuationService binds one concrete store, publisher and verified S3
// reader. maximumBytes bounds both the pre-provider reservation and encoded
// envelope, capped at the strict contract reader's document limit.
func NewContinuationService(
	pool *store.Pool,
	publisher *objects.Publisher,
	backend *objects.S3,
	maximumBytes int64,
) (*ContinuationService, error) {
	if pool == nil || publisher == nil || backend == nil || maximumBytes <= 0 || maximumBytes == math.MaxInt64 {
		return nil, ErrContinuationInvalid
	}
	return &ContinuationService{
		pool: pool, publisher: publisher, backend: backend,
		maximumBytes: min(maximumBytes, contract.MaxContractDocumentBytes),
	}, nil
}

type continuationProducer struct {
	Input              contract.PresentValue   `json:"input"`
	StickyControls     contract.ResultControls `json:"sticky_controls"`
	SelectedHTTPOrigin string                  `json:"selected_http_origin"`
	OperationDigest    contract.SemanticDigest `json:"operation_digest"`
	ConnectionRevision uint64                  `json:"connection_revision"`
	CredentialRef      string                  `json:"credential_ref"`
	ProviderAccountID  string                  `json:"provider_account_id"`
	Provider           string                  `json:"provider"`
	AccountWitness     string                  `json:"account_witness"`
	ProviderAccountRev uint64                  `json:"provider_account_revision"`
}

type continuationEnvelope struct {
	Schema        string                      `json:"schema"`
	ProducerRunID string                      `json:"producer_run_id"`
	Scope         contract.Scope              `json:"scope"`
	ConnectionID  string                      `json:"connection_id"`
	OperationID   string                      `json:"operation_id"`
	OperationRev  string                      `json:"operation_revision"`
	Producer      continuationProducer        `json:"producer"`
	State         pmruntime.ContinuationState `json:"state"`
}

type continuationAuthority struct {
	origin         store.WorkerContinuationOrigin
	producerRun    contract.RunRecord
	producerInput  store.RunInputSnapshot
	producerBudget store.RunBudgetSnapshot
	consumerRun    contract.RunRecord
	consumerInput  store.RunInputSnapshot
	consumerBudget store.RunBudgetSnapshot
	connection     store.Connection
	account        store.ProviderAccount
}

type boundContinuationCodec struct {
	service     *ContinuationService
	actor       contract.Actor
	binding     pmruntime.ContinuationBinding
	producer    continuationProducer
	reservation store.ObjectReservation

	mu          sync.Mutex
	opened      bool
	sealStarted bool
	publication *objects.PreparedPublication
	token       contract.PresentValue
}

func (codec *boundContinuationCodec) OpenContinuation(
	ctx context.Context,
	binding pmruntime.ContinuationBinding,
	token contract.PresentValue,
) (pmruntime.ContinuationState, error) {
	if codec == nil || codec.service == nil || !continuationBindingsEqual(codec.binding, binding) {
		return pmruntime.ContinuationState{}, ErrContinuationInvalid
	}
	codec.mu.Lock()
	if codec.opened {
		codec.mu.Unlock()
		return pmruntime.ContinuationState{}, fmt.Errorf("%w: continuation was opened more than once", ErrContinuationInvalid)
	}
	codec.opened = true
	codec.mu.Unlock()

	text, ok := token.TextValue()
	if !ok || text == "" {
		return pmruntime.ContinuationState{}, ErrContinuationInvalid
	}
	authority, err := codec.loadAuthority(ctx, text)
	if err != nil {
		return pmruntime.ContinuationState{}, fmt.Errorf("%w: %v", ErrContinuationInvalid, err)
	}
	object := authority.origin.Object
	if object.MediaType != continuationEnvelopeMediaType || object.ByteLength <= 0 || object.ByteLength > codec.service.maximumBytes {
		return pmruntime.ContinuationState{}, fmt.Errorf("%w: continuation object descriptor is outside the admitted envelope", ErrContinuationInvalid)
	}
	reader, err := codec.service.backend.OpenVerified(ctx, object, objects.ByteRange{Length: object.ByteLength})
	if err != nil {
		return pmruntime.ContinuationState{}, fmt.Errorf("open verified continuation: %w", err)
	}
	encoded, readErr := io.ReadAll(io.LimitReader(reader, codec.service.maximumBytes+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		return pmruntime.ContinuationState{}, fmt.Errorf("read verified continuation: %w", errors.Join(readErr, closeErr))
	}
	if int64(len(encoded)) != object.ByteLength || int64(len(encoded)) > codec.service.maximumBytes {
		return pmruntime.ContinuationState{}, fmt.Errorf("%w: continuation object length changed", ErrContinuationInvalid)
	}
	var envelope continuationEnvelope
	if err := contract.DecodeStrictRequiredObject(
		encoded,
		&envelope,
		"schema",
		"producer_run_id",
		"scope",
		"connection_id",
		"operation_id",
		"operation_revision",
		"producer",
		"state",
	); err != nil {
		return pmruntime.ContinuationState{}, fmt.Errorf("%w: decode continuation envelope: %v", ErrContinuationInvalid, err)
	}
	if err := codec.validateEnvelope(envelope, authority); err != nil {
		return pmruntime.ContinuationState{}, err
	}
	return envelope.State, nil
}

func (codec *boundContinuationCodec) SealContinuation(
	ctx context.Context,
	binding pmruntime.ContinuationBinding,
	state pmruntime.ContinuationState,
) (contract.PresentValue, error) {
	if codec == nil || codec.service == nil || !continuationBindingsEqual(codec.binding, binding) ||
		codec.reservation.Request.Purpose != store.ArtifactPurposeContinuation ||
		codec.reservation.State != store.ObjectReservationHeld {
		return contract.PresentValue{}, ErrContinuationInvalid
	}
	codec.mu.Lock()
	defer codec.mu.Unlock()
	if codec.sealStarted {
		return contract.PresentValue{}, fmt.Errorf("%w: continuation was sealed more than once", ErrContinuationInvalid)
	}
	codec.sealStarted = true
	if state.MediaType == "" || state.UnreadTail == nil || state.NextInputs == nil {
		return contract.PresentValue{}, fmt.Errorf("%w: incomplete continuation state", ErrContinuationInvalid)
	}
	for _, value := range state.UnreadTail {
		if err := value.Validate(); err != nil {
			return contract.PresentValue{}, fmt.Errorf("%w: invalid continuation tail", ErrContinuationInvalid)
		}
	}
	for _, value := range state.NextInputs {
		if err := value.Validate(); err != nil {
			return contract.PresentValue{}, fmt.Errorf("%w: invalid continuation navigation", ErrContinuationInvalid)
		}
	}
	if state.NextTarget != nil {
		if err := pmruntime.ValidateLinkContinuationTarget(*state.NextTarget); err != nil {
			return contract.PresentValue{}, fmt.Errorf("%w: invalid continuation target", ErrContinuationInvalid)
		}
	}
	if state.RecordIdentity != nil {
		if err := state.RecordIdentity.Validate(); err != nil {
			return contract.PresentValue{}, fmt.Errorf("%w: invalid continuation record identity", ErrContinuationInvalid)
		}
	}
	envelope := continuationEnvelope{
		Schema:        continuationEnvelopeSchema,
		ProducerRunID: binding.RunID,
		Scope:         binding.Scope,
		ConnectionID:  binding.ConnectionID,
		OperationID:   binding.OperationID,
		OperationRev:  binding.OperationRevision,
		Producer:      codec.producer,
		State:         state,
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return contract.PresentValue{}, fmt.Errorf("encode continuation envelope: %w", err)
	}
	if int64(len(encoded)) > codec.service.maximumBytes {
		return contract.PresentValue{}, objects.ErrObjectLimit
	}
	attempt, err := objects.NewAttempt(
		binding.Scope,
		codec.reservation.Request.ArtifactID,
		continuationEnvelopeMediaType,
		encoded,
		codec.service.maximumBytes,
	)
	if err != nil {
		return contract.PresentValue{}, err
	}
	publication, err := codec.service.publisher.Upload(
		ctx,
		codec.actor,
		binding.Scope,
		codec.reservation.Request.ReservationID,
		attempt,
	)
	if err != nil {
		_ = attempt.Close()
		return contract.PresentValue{}, err
	}
	token := contract.NewStringValue(codec.reservation.Request.ArtifactID)
	codec.publication = &publication
	codec.token = token
	return token, nil
}

func (codec *boundContinuationCodec) preparedForResult(result *contract.ActionResult) (*objects.PreparedPublication, error) {
	if codec == nil {
		if continuationResultToken(result).Presence() == contract.PresenceValue {
			return nil, fmt.Errorf("%w: result names a continuation without a bound codec", ErrContinuationInvalid)
		}
		return nil, nil
	}
	codec.mu.Lock()
	defer codec.mu.Unlock()
	resultToken := continuationResultToken(result)
	if codec.publication == nil {
		if resultToken.Presence() == contract.PresenceValue {
			return nil, fmt.Errorf("%w: result names an unprepared continuation", ErrContinuationInvalid)
		}
		return nil, nil
	}
	if result == nil || !resultToken.Equal(codec.token) {
		return nil, fmt.Errorf("%w: prepared continuation does not match the worker result", ErrContinuationInvalid)
	}
	publication := *codec.publication
	return &publication, nil
}

func (codec *boundContinuationCodec) loadAuthority(ctx context.Context, artifactID string) (continuationAuthority, error) {
	var authority continuationAuthority
	err := store.WithinScope(ctx, codec.service.pool, codec.binding.Scope, codec.actor, func(ctx context.Context, tx *store.Transaction) error {
		var err error
		authority.origin, err = store.LoadWorkerContinuationOrigin(ctx, tx, artifactID)
		if err != nil {
			return err
		}
		authority.producerRun, err = store.LoadRun(ctx, tx, authority.origin.RunID)
		if err != nil {
			return err
		}
		authority.producerInput, err = store.LoadRunInputSnapshot(ctx, tx, authority.origin.RunID)
		if err != nil {
			return err
		}
		authority.producerBudget, err = store.LoadRunBudgetSnapshot(ctx, tx, authority.origin.RunID)
		if err != nil {
			return err
		}
		authority.consumerRun, err = store.LoadRun(ctx, tx, codec.binding.RunID)
		if err != nil {
			return err
		}
		authority.consumerInput, err = store.LoadRunInputSnapshot(ctx, tx, codec.binding.RunID)
		if err != nil {
			return err
		}
		authority.consumerBudget, err = store.LoadRunBudgetSnapshot(ctx, tx, codec.binding.RunID)
		if err != nil {
			return err
		}
		authority.connection, err = store.LoadConnection(ctx, tx, codec.binding.ConnectionID)
		if err != nil {
			return err
		}
		authority.account, err = store.LoadProviderAccount(ctx, tx, authority.connection.ProviderAccountID)
		return err
	})
	if err != nil {
		return continuationAuthority{}, err
	}
	return authority, nil
}

func (codec *boundContinuationCodec) validateEnvelope(envelope continuationEnvelope, authority continuationAuthority) error {
	producerRef := authority.producerRun.Spec.OperationOrPlanRef
	consumerRef := authority.consumerRun.Spec.OperationOrPlanRef
	producerControls := stickyResultControls(authority.producerInput.ResultControls)
	consumerControls := stickyResultControls(authority.consumerInput.ResultControls)
	consumerToken, consumerHasToken := authority.consumerInput.ResultControls.ContinuationValue().TextValue()
	if envelope.Schema != continuationEnvelopeSchema ||
		envelope.ProducerRunID != authority.origin.RunID ||
		!envelope.Scope.Equal(codec.binding.Scope) ||
		envelope.ConnectionID != codec.binding.ConnectionID ||
		envelope.OperationID != codec.binding.OperationID ||
		envelope.OperationRev != codec.binding.OperationRevision ||
		producerRef.Kind != contract.OperationReference ||
		producerRef.ID != codec.binding.OperationID ||
		producerRef.Revision != codec.binding.OperationRevision ||
		consumerRef.Kind != contract.OperationReference ||
		consumerRef.ID != codec.binding.OperationID ||
		consumerRef.Revision != codec.binding.OperationRevision ||
		authority.producerInput.ConnectionID != codec.binding.ConnectionID ||
		authority.consumerInput.ConnectionID != codec.binding.ConnectionID ||
		authority.producerInput.Lane != contract.LaneDirectRead ||
		authority.consumerInput.Lane != contract.LaneDirectRead ||
		!consumerHasToken ||
		consumerToken != authority.origin.Object.ArtifactID ||
		!envelope.Producer.Input.Equal(authority.producerInput.Input) ||
		!authority.consumerInput.Input.Equal(authority.producerInput.Input) ||
		!equalStickyResultControls(envelope.Producer.StickyControls, producerControls) ||
		!equalStickyResultControls(consumerControls, producerControls) ||
		!envelope.Producer.Input.Equal(codec.producer.Input) ||
		!equalStickyResultControls(envelope.Producer.StickyControls, codec.producer.StickyControls) ||
		envelope.Producer.SelectedHTTPOrigin != codec.producer.SelectedHTTPOrigin ||
		envelope.Producer.OperationDigest != codec.producer.OperationDigest ||
		envelope.Producer.ConnectionRevision != authority.connection.Revision ||
		envelope.Producer.ConnectionRevision != codec.producer.ConnectionRevision ||
		envelope.Producer.CredentialRef != authority.connection.CredentialRef ||
		envelope.Producer.CredentialRef != codec.producer.CredentialRef ||
		envelope.Producer.ProviderAccountID != authority.connection.ProviderAccountID ||
		envelope.Producer.ProviderAccountID != authority.account.AccountID ||
		envelope.Producer.ProviderAccountID != codec.producer.ProviderAccountID ||
		envelope.Producer.AccountWitness != authority.account.AccountWitness ||
		envelope.Producer.AccountWitness != codec.producer.AccountWitness ||
		envelope.Producer.Provider != authority.account.Provider ||
		envelope.Producer.Provider != codec.producer.Provider ||
		envelope.Producer.ProviderAccountRev != authority.account.Revision ||
		envelope.Producer.ProviderAccountRev != codec.producer.ProviderAccountRev ||
		authority.producerBudget.Policy.HTTP == nil ||
		authority.consumerBudget.Policy.HTTP == nil ||
		authority.producerBudget.Policy.HTTP.SelectedOrigin != envelope.Producer.SelectedHTTPOrigin ||
		authority.consumerBudget.Policy.HTTP.SelectedOrigin != envelope.Producer.SelectedHTTPOrigin {
		return fmt.Errorf("%w: continuation lineage no longer matches current durable authority", ErrContinuationInvalid)
	}
	// A sealed provider continuation target is bound to the durable selected
	// origin at rest: the runtime re-checks it against the admitted destination
	// before any request is built, and here the envelope must already name the
	// same origin the producing run was admitted to.
	if envelope.State.NextTarget != nil {
		if err := pmruntime.ValidateLinkContinuationTarget(*envelope.State.NextTarget); err != nil {
			return fmt.Errorf("%w: continuation target is invalid", ErrContinuationInvalid)
		}
		if !sameContinuationTargetOrigin(envelope.State.NextTarget.Origin, envelope.Producer.SelectedHTTPOrigin) {
			return fmt.Errorf("%w: continuation target origin is not the admitted selected origin", ErrContinuationInvalid)
		}
	}
	return nil
}

// sameContinuationTargetOrigin reports whether one sealed provider target origin
// names the same scheme and host as the durable selected HTTP origin, which may
// carry a base path the target origin never repeats.
func sameContinuationTargetOrigin(target, selected string) bool {
	targetURL, targetErr := url.Parse(target)
	selectedURL, selectedErr := url.Parse(selected)
	if targetErr != nil || selectedErr != nil || targetURL.Host == "" || selectedURL.Host == "" {
		return false
	}
	return strings.EqualFold(targetURL.Scheme, selectedURL.Scheme) &&
		strings.EqualFold(targetURL.Host, selectedURL.Host)
}

func continuationBindingsEqual(left, right pmruntime.ContinuationBinding) bool {
	return left.Scope.Equal(right.Scope) &&
		left.ConnectionID == right.ConnectionID &&
		left.OperationID == right.OperationID &&
		left.OperationRevision == right.OperationRevision &&
		left.RunID == right.RunID
}

func continuationResultToken(result *contract.ActionResult) contract.PresentValue {
	if result == nil || result.Pagination == nil {
		return contract.AbsentValue()
	}
	return result.Pagination.Continuation
}

func stickyResultControls(controls contract.ResultControls) contract.ResultControls {
	sticky := contract.ResultControls{}
	if controls.Selection != nil {
		selection := contract.FieldSelection{Paths: make([]contract.FieldPath, len(controls.Selection.Paths))}
		for index, path := range controls.Selection.Paths {
			selection.Paths[index] = append(contract.FieldPath(nil), path...)
		}
		sticky.Selection = &selection
	}
	if controls.Filter != nil {
		filter := *controls.Filter
		filter.Path = append(contract.FieldPath(nil), controls.Filter.Path...)
		if controls.Filter.Value != nil {
			value := *controls.Filter.Value
			filter.Value = &value
		}
		sticky.Filter = &filter
	}
	if controls.TotalRecordCap != nil {
		cap := *controls.TotalRecordCap
		sticky.TotalRecordCap = &cap
	}
	return sticky
}

func equalStickyResultControls(left, right contract.ResultControls) bool {
	left = stickyResultControls(left)
	right = stickyResultControls(right)
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func restoreContinuationInvocation(
	ctx context.Context,
	tx *store.Transaction,
	invocation RunInvocation,
	operation contract.Operation,
	policy store.RunBudgetPolicy,
) (RunInvocation, error) {
	if !invocation.ResultControls.HasContinuation() {
		return invocation, nil
	}
	if invocation.Lane != contract.LaneDirectRead || operation.Execution == nil ||
		operation.Execution.HTTP == nil || policy.HTTP == nil {
		return RunInvocation{}, invalidRequestFailure()
	}
	projection, err := contract.ProjectOperationRequest(operation)
	if err != nil {
		return RunInvocation{}, capabilityUnavailableFailure(err)
	}
	fields := operationRequestFields(operation)
	if err := projection.ValidateSuppliedInput(fields, invocation.Input); err != nil {
		return RunInvocation{}, invalidRequestFailure()
	}
	suppliedFields, ok := invocation.Input.Fields()
	if !ok {
		return RunInvocation{}, invalidRequestFailure()
	}
	navigationField := pmruntime.PaginationNavigationField(operation.Execution.Pagination)
	if navigation, supplied := suppliedFields[navigationField]; navigationField != "" && supplied &&
		navigation.Presence() != contract.PresenceAbsent {
		return RunInvocation{}, invalidRequestFailure()
	}
	token, ok := invocation.ResultControls.ContinuationValue().TextValue()
	if !ok || token == "" {
		return RunInvocation{}, invalidRequestFailure()
	}
	origin, err := store.LoadWorkerContinuationOrigin(ctx, tx, token)
	if err != nil {
		return RunInvocation{}, invalidRequestFailure()
	}
	producerRun, err := store.LoadRun(ctx, tx, origin.RunID)
	if err != nil {
		return RunInvocation{}, invalidRequestFailure()
	}
	producerInput, err := store.LoadRunInputSnapshot(ctx, tx, origin.RunID)
	if err != nil {
		return RunInvocation{}, invalidRequestFailure()
	}
	producerBudget, err := store.LoadRunBudgetSnapshot(ctx, tx, origin.RunID)
	if err != nil {
		return RunInvocation{}, invalidRequestFailure()
	}
	reference := producerRun.Spec.OperationOrPlanRef
	if reference.Kind != contract.OperationReference ||
		reference.ID != invocation.OperationRef ||
		reference.Revision != invocation.ExpectedRevision ||
		origin.Object.OrgID != producerInput.Scope.OrgID ||
		origin.Object.WorkspaceID != producerInput.Scope.WorkspaceID ||
		producerInput.ConnectionID != invocation.ConnectionRef ||
		producerInput.Lane != invocation.Lane ||
		producerBudget.Policy.HTTP == nil ||
		producerBudget.Policy.HTTP.SelectedOrigin != policy.HTTP.SelectedOrigin {
		return RunInvocation{}, invalidRequestFailure()
	}
	effectiveInput, err := restoreContinuationInput(producerInput.Input, invocation.Input, navigationField)
	if err != nil {
		return RunInvocation{}, invalidRequestFailure()
	}
	effectiveControls, err := restoreContinuationControls(
		producerInput.ResultControls,
		invocation.ResultControls,
	)
	if err != nil {
		return RunInvocation{}, invalidRequestFailure()
	}
	if err := contract.ValidateOperationInput(operation, effectiveInput); err != nil {
		return RunInvocation{}, invalidRequestFailure()
	}
	invocation.Input = effectiveInput
	invocation.ResultControls = effectiveControls
	return invocation, nil
}

func operationRequestFields(operation contract.Operation) []contract.FieldBinding {
	count := len(operation.Inputs)
	for _, variant := range operation.RequestVariants {
		count += len(variant.Inputs)
	}
	fields := make([]contract.FieldBinding, 0, count)
	fields = append(fields, operation.Inputs...)
	for _, variant := range operation.RequestVariants {
		fields = append(fields, variant.Inputs...)
	}
	return fields
}

func restoreContinuationInput(
	producer contract.PresentValue,
	supplied contract.PresentValue,
	navigationField string,
) (contract.PresentValue, error) {
	producerFields, ok := producer.Fields()
	if !ok {
		return contract.PresentValue{}, ErrContinuationInvalid
	}
	suppliedFields, ok := supplied.Fields()
	if !ok {
		return contract.PresentValue{}, ErrContinuationInvalid
	}
	for fieldID, value := range suppliedFields {
		if value.Presence() == contract.PresenceAbsent {
			continue
		}
		if fieldID == navigationField && navigationField != "" {
			return contract.PresentValue{}, ErrContinuationInvalid
		}
		original, exists := producerFields[fieldID]
		if !exists || !value.Equal(original) {
			return contract.PresentValue{}, ErrContinuationInvalid
		}
	}
	return contract.NewObjectValue(producerFields), nil
}

func restoreContinuationControls(
	producer contract.ResultControls,
	supplied contract.ResultControls,
) (contract.ResultControls, error) {
	producerSticky := stickyResultControls(producer)
	suppliedSticky := stickyResultControls(supplied)
	if supplied.Selection != nil {
		selectionOnly := contract.ResultControls{Selection: suppliedSticky.Selection}
		if !equalStickyResultControls(selectionOnly, contract.ResultControls{Selection: producerSticky.Selection}) {
			return contract.ResultControls{}, ErrContinuationInvalid
		}
	}
	if supplied.Filter != nil {
		filterOnly := contract.ResultControls{Filter: suppliedSticky.Filter}
		if !equalStickyResultControls(filterOnly, contract.ResultControls{Filter: producerSticky.Filter}) {
			return contract.ResultControls{}, ErrContinuationInvalid
		}
	}
	if supplied.TotalRecordCap != nil &&
		(producerSticky.TotalRecordCap == nil || *supplied.TotalRecordCap != *producerSticky.TotalRecordCap) {
		return contract.ResultControls{}, ErrContinuationInvalid
	}
	effective := producerSticky
	continuation := supplied.ContinuationValue()
	effective.Continuation = &continuation
	if err := effective.Validate(); err != nil {
		return contract.ResultControls{}, err
	}
	return effective, nil
}

func (service *ContinuationService) reserveWorkerContinuation(
	ctx context.Context,
	tx *store.Transaction,
	plan workerUnitPlan,
	operation contract.Operation,
	connection store.Connection,
	account store.ProviderAccount,
) (*boundContinuationCodec, error) {
	if service == nil || plan.reconcile || !continuationRequiredForHTTP(operation, plan.state.input.ResultControls) ||
		plan.state.budget.Policy.HTTP == nil {
		return nil, workerFailure(ErrWorkerCapabilityUnavailable, ErrContinuationInvalid)
	}
	identity := struct {
		Scope      contract.Scope      `json:"scope"`
		RunID      string              `json:"run_id"`
		TaskID     string              `json:"task_id"`
		DispatchID contract.DispatchID `json:"dispatch_id"`
		OwnerID    string              `json:"owner_id"`
		Fence      uint64              `json:"fence"`
	}{
		plan.state.route.Scope,
		plan.task.RunID,
		plan.task.TaskID,
		plan.dispatch.Intent.DispatchID,
		plan.lease.OwnerID,
		plan.lease.Fence,
	}
	id, err := workerRecordID("worker-continuation-v1-", identity)
	if err != nil {
		return nil, err
	}
	reservation, err := service.publisher.ReserveInTransaction(ctx, tx, store.ObjectReservationRequest{
		ReservationID: id,
		ConnectionID:  plan.state.input.ConnectionID,
		ArtifactID:    id,
		Purpose:       store.ArtifactPurposeContinuation,
		MaximumBytes:  uint64(service.maximumBytes),
	})
	if err != nil {
		return nil, err
	}
	if reservation.State != store.ObjectReservationHeld {
		return nil, store.ErrObjectReservationClosed
	}
	operationDigest, err := canonicalIntent(operation)
	if err != nil {
		return nil, err
	}
	return &boundContinuationCodec{
		service: service,
		actor:   plan.state.route.Actor,
		binding: pmruntime.ContinuationBinding{
			Scope:             plan.state.route.Scope,
			ConnectionID:      plan.state.input.ConnectionID,
			OperationID:       operation.OperationID,
			OperationRevision: plan.dispatch.Intent.OperationRevision,
			RunID:             plan.state.record.Spec.RunID,
		},
		producer: continuationProducer{
			Input:              plan.state.input.Input,
			StickyControls:     stickyResultControls(plan.state.input.ResultControls),
			SelectedHTTPOrigin: plan.state.budget.Policy.HTTP.SelectedOrigin,
			OperationDigest:    operationDigest,
			ConnectionRevision: connection.Revision,
			CredentialRef:      connection.CredentialRef,
			ProviderAccountID:  connection.ProviderAccountID,
			Provider:           account.Provider,
			AccountWitness:     account.AccountWitness,
			ProviderAccountRev: account.Revision,
		},
		reservation: reservation,
		token:       contract.AbsentValue(),
	}, nil
}

func commitWorkerContinuation(
	ctx context.Context,
	tx *store.Transaction,
	plan workerUnitPlan,
	publication *objects.PreparedPublication,
	resultArtifactID string,
) (string, error) {
	codec := plan.request.continuation
	if codec == nil {
		if publication != nil {
			return "", store.ErrImmutableConflict
		}
		return "", nil
	}
	reservationID := codec.reservation.Request.ReservationID
	if publication != nil && resultArtifactID != "" {
		receipt, err := publication.Commit(ctx, tx)
		if err != nil {
			return "", err
		}
		if receipt.ReservationID != reservationID ||
			receipt.ConnectionID != plan.state.input.ConnectionID ||
			receipt.Purpose != store.ArtifactPurposeContinuation ||
			receipt.Object.ArtifactID != codec.reservation.Request.ArtifactID {
			return "", store.ErrImmutableConflict
		}
		return receipt.Object.ArtifactID, nil
	}
	reservation, err := store.LoadObjectReservation(ctx, tx, reservationID)
	if err != nil {
		return "", err
	}
	switch reservation.State {
	case store.ObjectReservationHeld:
		return "", store.CancelObjectReservation(ctx, tx, reservationID)
	case store.ObjectReservationBound:
		// A failed upload, or a successfully verified private upload whose public
		// result could not be committed, remains charged and unpublished.
		return "", nil
	default:
		return "", errors.New("unsettled worker continuation reservation has incompatible state")
	}
}
