package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

var (
	ErrHandlerInvalid            = errors.New("handler registration or resolution is invalid")
	ErrHandlerConflict           = errors.New("handler registration conflicts")
	ErrHandlerUnavailable        = errors.New("handler is unavailable")
	ErrHandlerIncompatible       = errors.New("handler is incompatible")
	ErrReconciliationUnavailable = errors.New("handler reconciliation is unavailable")
)

// ExecutionRequest is the in-process input to one admitted worker callback. It
// is not a workflow payload and does not carry provider credentials. Its
// unexported physical-call authority can only re-authorize this exact callback.
type ExecutionRequest struct {
	Scope              contract.Scope
	ConnectionID       string
	PermitRef          string
	ReconciliationOnly bool
	Run                contract.RunSpec
	Task               contract.Task
	Dispatch           contract.Dispatch
	Attempt            contract.Attempt
	AttemptNumber      uint32
	Operation          contract.Operation
	HTTPPolicy         contract.HTTPExecutionPolicy
	Input              contract.PresentValue
	Controls           contract.ResultControls
	Deadline           time.Time

	physicalCall *workerPhysicalCallBoundary
	continuation *boundContinuationCodec
}

// ExecutionObservation is the immediate typed observation returned by an
// execution or reconciliation callback. Result is not a durable publication
// witness. Provider-member outcomes remain independently authoritative, while
// the configured worker publishes verified result bytes and their reference
// atomically with settlement.
type ExecutionObservation struct {
	Result   *contract.ActionResult
	Outcomes []contract.MemberOutcome
	HTTP     *HTTPAttemptObservation
}

// ExecutionCallback is implemented by a real shared execution engine.
type ExecutionCallback func(context.Context, ExecutionRequest) (ExecutionObservation, error)

// HandlerKind identifies a shared executor class. It is not a provider name.
type HandlerKind string

const (
	HandlerHTTP    HandlerKind = "http"
	HandlerSQL     HandlerKind = "sql"
	HandlerETL     HandlerKind = "etl"
	HandlerApply   HandlerKind = "apply"
	HandlerReverse HandlerKind = "reverse"
	HandlerBinary  HandlerKind = "binary"
)

// HandlerRegistration binds exact executable capabilities and foundation
// revisions to the lanes that one callback can actually execute.
type HandlerRegistration struct {
	Lanes         []contract.Lane
	Foundations   []contract.FoundationContract
	Capabilities  []contract.CapabilityRequirement
	Execute       ExecutionCallback
	Reconcile     ExecutionCallback
	continuations *ContinuationService
}

// HandlerManifest is a deterministic projection of callbacks that are really
// registered. It cannot be loaded back into a registry or confer execution.
type HandlerManifest struct {
	Handlers []HandlerManifestEntry `json:"handlers"`
}

// HandlerManifestEntry omits callbacks while retaining their exact executable
// compatibility envelope.
type HandlerManifestEntry struct {
	Kind                    HandlerKind                      `json:"kind"`
	Lanes                   []contract.Lane                  `json:"lanes"`
	Foundations             []contract.FoundationContract    `json:"foundations"`
	Capabilities            []contract.CapabilityRequirement `json:"capabilities"`
	ReconciliationAvailable bool                             `json:"reconciliation_available"`
}

// HandlerError classifies safe registration and resolution failures. Cause is
// retained for errors.Is/errors.As without placing rejected values in fields.
type HandlerError struct {
	Kind    error
	Handler HandlerKind
	Lane    contract.Lane
	Cause   error
}

func (e *HandlerError) Error() string {
	if e == nil {
		return "<nil>"
	}
	message := e.Kind.Error()
	if e.Handler != "" {
		message += fmt.Sprintf(" for %s handler", e.Handler)
	}
	if e.Lane != "" {
		message += fmt.Sprintf(" on %s lane", e.Lane)
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *HandlerError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *HandlerError) Is(target error) bool {
	return e != nil && (target == e.Kind || errors.Is(e.Cause, target))
}

// ResolvedHandler retains the exact registered callbacks selected for one
// operation and lane. The zero value is unavailable.
type ResolvedHandler struct {
	Kind                    HandlerKind
	Lane                    contract.Lane
	ReconciliationAvailable bool
	execute                 ExecutionCallback
	reconcile               ExecutionCallback
	continuations           *ContinuationService
}

// Execute invokes the selected registered execution callback.
func (handler ResolvedHandler) Execute(ctx context.Context, request ExecutionRequest) (ExecutionObservation, error) {
	if handler.execute == nil {
		return ExecutionObservation{}, newHandlerError(ErrHandlerUnavailable, handler.Kind, handler.Lane, nil)
	}
	return handler.execute(ctx, request)
}

// Reconcile invokes the selected reconciliation callback or reports its
// explicit absence. It never substitutes a successful no-op.
func (handler ResolvedHandler) Reconcile(ctx context.Context, request ExecutionRequest) (ExecutionObservation, error) {
	if handler.reconcile == nil {
		return ExecutionObservation{}, newHandlerError(ErrReconciliationUnavailable, handler.Kind, handler.Lane, nil)
	}
	return handler.reconcile(ctx, request)
}

// HandlerRegistry is the one shared API/worker callback registry. Its zero
// value is an empty, truthful registry.
type HandlerRegistry struct {
	mu            sync.RWMutex
	registrations []registeredHandler
}

type registeredHandler struct {
	kind            HandlerKind
	lanes           []contract.Lane
	foundations     []contract.FoundationContract
	capabilities    []contract.CapabilityRequirement
	capabilityIndex map[capabilityIdentity]struct{}
	execute         ExecutionCallback
	continuations   *ContinuationService
	reconcile       ExecutionCallback
}

type capabilityIdentity struct {
	name              string
	foundationID      string
	foundationVersion string
}

// RegisterHTTP installs an HTTP execution callback for its exact declared
// lanes and capability closure.
func (registry *HandlerRegistry) RegisterHTTP(registration HandlerRegistration) error {
	return registry.register(HandlerHTTP, registration)
}

// RegisterSQL installs a native SQL execution callback for its exact declared
// lanes and capability closure.
func (registry *HandlerRegistry) RegisterSQL(registration HandlerRegistration) error {
	return registry.register(HandlerSQL, registration)
}

// RegisterETL installs an ETL execution callback for its exact declared lanes
// and capability closure.
func (registry *HandlerRegistry) RegisterETL(registration HandlerRegistration) error {
	return registry.register(HandlerETL, registration)
}

// RegisterApply installs a destination-apply execution callback for its exact
// declared lanes and capability closure.
func (registry *HandlerRegistry) RegisterApply(registration HandlerRegistration) error {
	return registry.register(HandlerApply, registration)
}

// RegisterReverse installs a reverse-mapping execution callback for its exact
// declared lanes and capability closure.
func (registry *HandlerRegistry) RegisterReverse(registration HandlerRegistration) error {
	return registry.register(HandlerReverse, registration)
}

// RegisterBinary installs a binary-transfer execution callback for its exact
// declared lanes and capability closure.
func (registry *HandlerRegistry) RegisterBinary(registration HandlerRegistration) error {
	return registry.register(HandlerBinary, registration)
}

func (registry *HandlerRegistry) register(kind HandlerKind, registration HandlerRegistration) error {
	if registry == nil {
		return newHandlerError(ErrHandlerInvalid, kind, "", errors.New("handler registry is nil"))
	}
	normalized, err := normalizeHandlerRegistration(kind, registration)
	if err != nil {
		return err
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	for _, existing := range registry.registrations {
		if registrationsConflict(existing, normalized) {
			return newHandlerError(ErrHandlerConflict, kind, "", nil)
		}
	}
	registry.registrations = append(registry.registrations, normalized)
	return nil
}

// Resolve returns one real callback only when the operation is valid, the lane
// is eligible, every required foundation revision is installed by that
// callback, and every declared capability is implemented by it.
func (registry *HandlerRegistry) Resolve(operation contract.Operation, lane contract.Lane) (ResolvedHandler, error) {
	requiredFoundations := foundationReferences(operation.CapabilityRequirements)
	if err := operation.Validate(shapeFoundations(requiredFoundations)); err != nil {
		return ResolvedHandler{}, newHandlerError(ErrHandlerInvalid, "", lane, err)
	}
	if !operationAllowsLane(operation, lane) {
		return ResolvedHandler{}, newHandlerError(ErrHandlerUnavailable, "", lane, nil)
	}
	if registry == nil {
		return ResolvedHandler{}, newHandlerError(ErrHandlerUnavailable, "", lane, nil)
	}

	registry.mu.RLock()
	defer registry.mu.RUnlock()
	var selected registeredHandler
	selectedFound := false
	laneReachable := false
	var incompatibility error
	for _, registration := range registry.registrations {
		if !slices.Contains(registration.lanes, lane) || !registrationMatchesTransport(registration.kind, operation.Transport.Kind) {
			continue
		}
		laneReachable = true
		if err := contract.CheckFoundationCompatibility(requiredFoundations, registration.foundations); err != nil {
			if incompatibility == nil {
				incompatibility = err
			}
			continue
		}
		if !registrationImplements(registration, operation.CapabilityRequirements) {
			continue
		}
		if selectedFound {
			return ResolvedHandler{}, newHandlerError(ErrHandlerConflict, "", lane, nil)
		}
		selected = registration
		selectedFound = true
	}
	if !selectedFound {
		if laneReachable {
			return ResolvedHandler{}, newHandlerError(ErrHandlerIncompatible, "", lane, incompatibility)
		}
		return ResolvedHandler{}, newHandlerError(ErrHandlerUnavailable, "", lane, nil)
	}
	return ResolvedHandler{
		Kind:                    selected.kind,
		Lane:                    lane,
		ReconciliationAvailable: selected.reconcile != nil,
		execute:                 selected.execute,
		continuations:           selected.continuations,
		reconcile:               selected.reconcile,
	}, nil
}

// BuildHandlerManifest snapshots registered executable callbacks in canonical
// order. A nil or empty registry produces an explicit empty list.
func BuildHandlerManifest(registry *HandlerRegistry) HandlerManifest {
	manifest := HandlerManifest{Handlers: []HandlerManifestEntry{}}
	if registry == nil {
		return manifest
	}

	registry.mu.RLock()
	defer registry.mu.RUnlock()
	manifest.Handlers = make([]HandlerManifestEntry, 0, len(registry.registrations))
	for _, registration := range registry.registrations {
		manifest.Handlers = append(manifest.Handlers, HandlerManifestEntry{
			Kind:                    registration.kind,
			Lanes:                   slices.Clone(registration.lanes),
			Foundations:             cloneFoundations(registration.foundations),
			Capabilities:            slices.Clone(registration.capabilities),
			ReconciliationAvailable: registration.reconcile != nil,
		})
	}
	slices.SortFunc(manifest.Handlers, compareManifestEntries)
	return manifest
}

func normalizeHandlerRegistration(kind HandlerKind, registration HandlerRegistration) (registeredHandler, error) {
	if !validHandlerKind(kind) {
		return registeredHandler{}, newHandlerError(ErrHandlerInvalid, kind, "", errors.New("handler kind is unsupported"))
	}
	if registration.Execute == nil {
		return registeredHandler{}, newHandlerError(ErrHandlerInvalid, kind, "", errors.New("execution callback is required"))
	}
	if len(registration.Lanes) == 0 {
		return registeredHandler{}, newHandlerError(ErrHandlerInvalid, kind, "", errors.New("at least one lane is required"))
	}
	lanes := slices.Clone(registration.Lanes)
	for _, lane := range lanes {
		if !validHandlerLane(lane) {
			return registeredHandler{}, newHandlerError(ErrHandlerInvalid, kind, lane, errors.New("lane is unsupported"))
		}
	}
	slices.SortFunc(lanes, compareLanes)
	for index := 1; index < len(lanes); index++ {
		if lanes[index] == lanes[index-1] {
			return registeredHandler{}, newHandlerError(ErrHandlerInvalid, kind, lanes[index], errors.New("lane is duplicated"))
		}
	}

	if len(registration.Capabilities) == 0 {
		return registeredHandler{}, newHandlerError(ErrHandlerInvalid, kind, "", errors.New("at least one executable capability is required"))
	}
	capabilities := slices.Clone(registration.Capabilities)
	slices.SortFunc(capabilities, compareCapabilityRequirements)
	capabilityIndex := make(map[capabilityIdentity]struct{}, len(capabilities))
	for _, capability := range capabilities {
		if err := capability.Validate(); err != nil {
			return registeredHandler{}, newHandlerError(ErrHandlerInvalid, kind, "", err)
		}
		identity := capabilityKey(capability)
		if _, duplicate := capabilityIndex[identity]; duplicate {
			return registeredHandler{}, newHandlerError(ErrHandlerInvalid, kind, "", errors.New("executable capability is duplicated"))
		}
		capabilityIndex[identity] = struct{}{}
	}

	foundations := cloneFoundations(registration.Foundations)
	slices.SortFunc(foundations, compareFoundationContracts)
	if err := contract.CheckFoundationCompatibility(foundationReferences(capabilities), foundations); err != nil {
		return registeredHandler{}, newHandlerError(ErrHandlerInvalid, kind, "", err)
	}
	return registeredHandler{
		kind:            kind,
		lanes:           lanes,
		foundations:     foundations,
		capabilities:    capabilities,
		capabilityIndex: capabilityIndex,
		continuations:   registration.continuations,
		execute:         registration.Execute,
		reconcile:       registration.Reconcile,
	}, nil
}

func registrationsConflict(left, right registeredHandler) bool {
	if !lanesOverlap(left.lanes, right.lanes) || !transportDomainsOverlap(left.kind, right.kind) {
		return false
	}
	for identity := range left.capabilityIndex {
		if _, duplicate := right.capabilityIndex[identity]; duplicate {
			return true
		}
	}
	return false
}

func registrationImplements(registration registeredHandler, requirements []contract.CapabilityRequirement) bool {
	for _, requirement := range requirements {
		if _, implemented := registration.capabilityIndex[capabilityKey(requirement)]; !implemented {
			return false
		}
	}
	return true
}

func capabilityKey(requirement contract.CapabilityRequirement) capabilityIdentity {
	return capabilityIdentity{
		name:              requirement.Capability,
		foundationID:      requirement.Foundation.ID,
		foundationVersion: requirement.Foundation.Version,
	}
}

func foundationReferences(capabilities []contract.CapabilityRequirement) []contract.FoundationRef {
	foundations := make([]contract.FoundationRef, 0, len(capabilities))
	seen := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		key := capability.Foundation.ID + "\x00" + capability.Foundation.Version
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		foundations = append(foundations, capability.Foundation)
	}
	slices.SortFunc(foundations, compareFoundationRefs)
	return foundations
}

func shapeFoundations(references []contract.FoundationRef) []contract.FoundationContract {
	foundations := make([]contract.FoundationContract, 0, len(references))
	for _, reference := range references {
		foundations = append(foundations, contract.FoundationContract{
			ID:           reference.ID,
			Version:      reference.Version,
			Dependencies: []contract.FoundationRef{},
		})
	}
	return foundations
}

func operationAllowsLane(operation contract.Operation, lane contract.Lane) bool {
	for _, eligibility := range operation.LaneEligibility {
		if eligibility.Lane == lane {
			return eligibility.Status == contract.LaneEligible
		}
	}
	return false
}

func registrationMatchesTransport(kind HandlerKind, transport string) bool {
	switch kind {
	case HandlerHTTP:
		return transport == contract.TransportKindHTTP
	case HandlerSQL:
		return transport == contract.TransportKindPostgreSQL
	default:
		return true
	}
}

func transportDomainsOverlap(left, right HandlerKind) bool {
	return !((left == HandlerHTTP && right == HandlerSQL) || (left == HandlerSQL && right == HandlerHTTP))
}

func lanesOverlap(left, right []contract.Lane) bool {
	for _, lane := range left {
		if slices.Contains(right, lane) {
			return true
		}
	}
	return false
}

func cloneFoundations(foundations []contract.FoundationContract) []contract.FoundationContract {
	cloned := slices.Clone(foundations)
	for index := range cloned {
		cloned[index].Dependencies = slices.Clone(cloned[index].Dependencies)
		slices.SortFunc(cloned[index].Dependencies, compareFoundationRefs)
	}
	return cloned
}

func compareManifestEntries(left, right HandlerManifestEntry) int {
	if comparison := cmp.Compare(handlerKindRank(left.Kind), handlerKindRank(right.Kind)); comparison != 0 {
		return comparison
	}
	if comparison := compareLaneLists(left.Lanes, right.Lanes); comparison != 0 {
		return comparison
	}
	if comparison := compareCapabilityLists(left.Capabilities, right.Capabilities); comparison != 0 {
		return comparison
	}
	if comparison := compareFoundationLists(left.Foundations, right.Foundations); comparison != 0 {
		return comparison
	}
	if left.ReconciliationAvailable == right.ReconciliationAvailable {
		return 0
	}
	if !left.ReconciliationAvailable {
		return -1
	}
	return 1
}

func compareLaneLists(left, right []contract.Lane) int {
	return slices.CompareFunc(left, right, compareLanes)
}

func compareCapabilityLists(left, right []contract.CapabilityRequirement) int {
	return slices.CompareFunc(left, right, compareCapabilityRequirements)
}

func compareFoundationLists(left, right []contract.FoundationContract) int {
	return slices.CompareFunc(left, right, compareFoundationContracts)
}

func compareLanes(left, right contract.Lane) int {
	return cmp.Compare(handlerLaneRank(left), handlerLaneRank(right))
}

func compareCapabilityRequirements(left, right contract.CapabilityRequirement) int {
	if comparison := cmp.Compare(left.Capability, right.Capability); comparison != 0 {
		return comparison
	}
	if comparison := cmp.Compare(left.Foundation.ID, right.Foundation.ID); comparison != 0 {
		return comparison
	}
	return cmp.Compare(left.Foundation.Version, right.Foundation.Version)
}

func compareFoundationRefs(left, right contract.FoundationRef) int {
	if comparison := cmp.Compare(left.ID, right.ID); comparison != 0 {
		return comparison
	}
	return cmp.Compare(left.Version, right.Version)
}

func compareFoundationContracts(left, right contract.FoundationContract) int {
	if comparison := cmp.Compare(left.ID, right.ID); comparison != 0 {
		return comparison
	}
	if comparison := cmp.Compare(left.Version, right.Version); comparison != 0 {
		return comparison
	}
	if comparison := cmp.Compare(left.SchemaVersion, right.SchemaVersion); comparison != 0 {
		return comparison
	}
	if comparison := cmp.Compare(left.Name, right.Name); comparison != 0 {
		return comparison
	}
	if comparison := cmp.Compare(left.Owner, right.Owner); comparison != 0 {
		return comparison
	}
	return slices.CompareFunc(left.Dependencies, right.Dependencies, compareFoundationRefs)
}

func validHandlerKind(kind HandlerKind) bool {
	switch kind {
	case HandlerHTTP, HandlerSQL, HandlerETL, HandlerApply, HandlerReverse, HandlerBinary:
		return true
	default:
		return false
	}
}

func validHandlerLane(lane contract.Lane) bool {
	switch lane {
	case contract.LaneDirectRead, contract.LaneDirectWrite, contract.LaneETL, contract.LaneReverseETL,
		contract.LaneBinaryDownload, contract.LaneBinaryUpload, contract.LaneSyncTransport:
		return true
	default:
		return false
	}
}

func handlerKindRank(kind HandlerKind) int {
	switch kind {
	case HandlerHTTP:
		return 0
	case HandlerSQL:
		return 1
	case HandlerETL:
		return 2
	case HandlerApply:
		return 3
	case HandlerReverse:
		return 4
	case HandlerBinary:
		return 5
	default:
		return 6
	}
}

func handlerLaneRank(lane contract.Lane) int {
	switch lane {
	case contract.LaneDirectRead:
		return 0
	case contract.LaneDirectWrite:
		return 1
	case contract.LaneETL:
		return 2
	case contract.LaneReverseETL:
		return 3
	case contract.LaneBinaryDownload:
		return 4
	case contract.LaneBinaryUpload:
		return 5
	case contract.LaneSyncTransport:
		return 6
	default:
		return 7
	}
}

func newHandlerError(kind error, handler HandlerKind, lane contract.Lane, cause error) error {
	return &HandlerError{Kind: kind, Handler: handler, Lane: lane, Cause: cause}
}
