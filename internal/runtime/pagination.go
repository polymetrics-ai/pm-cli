package runtime

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

var (
	ErrInvalidPaginationFact = errors.New("invalid admitted pagination fact")
	ErrNavigationConflict    = errors.New("raw provider navigation conflicts with PM continuation")
	ErrContinuationRequired  = errors.New("server-owned continuation codec is required")
)

func validatePaginationFact(operation contract.Operation, pagination *contract.OperationPagination) error {
	if pagination == nil {
		return nil
	}
	if err := pagination.Validate(operation); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidPaginationFact, err)
	}
	return nil
}

// PaginationNavigationField returns the one raw provider navigation field
// selected by an admitted pagination strategy. An empty result means that no
// such field is declared.
func PaginationNavigationField(pagination *contract.OperationPagination) string {
	if pagination == nil {
		return ""
	}
	switch pagination.Kind {
	case contract.OperationPaginationCursor:
		return pagination.CursorRequestField
	case contract.OperationPaginationOffset:
		return pagination.OffsetRequestField
	case contract.OperationPaginationPage:
		return pagination.PageRequestField
	case contract.OperationPaginationLink:
		return pagination.LinkRequestField
	default:
		return ""
	}
}

// PaginationLimitField returns the provider page-size field selected by an
// admitted pagination strategy. It is not itself continuation navigation.
func PaginationLimitField(pagination *contract.OperationPagination) string {
	if pagination == nil {
		return ""
	}
	switch pagination.Kind {
	case contract.OperationPaginationOffset, contract.OperationPaginationLink:
		return pagination.LimitRequestField
	default:
		return ""
	}
}

type ContinuationBinding struct {
	Scope             contract.Scope `json:"scope"`
	ConnectionID      string         `json:"connection_id"`
	OperationID       string         `json:"operation_id"`
	OperationRevision string         `json:"operation_revision"`
	RunID             string         `json:"run_id"`
}

type ContinuationState struct {
	MediaType          string                           `json:"media_type"`
	EffectiveWireLimit *uint64                          `json:"effective_wire_limit,omitempty"`
	ProviderTotal      *contract.ExactNumber            `json:"provider_total,omitempty"`
	UnreadTail         []contract.PresentValue          `json:"unread_tail"`
	NextInputs         map[string]contract.PresentValue `json:"next_inputs"`
	// NextTarget is the sealed provider-issued continuation target of a link
	// strategy: the origin, path and query the provider returned for the
	// authorized request, including any opaque cursor parameter the admitted
	// source does not declare. No caller value ever reaches it and it is
	// re-validated against the admitted destination before every reuse.
	NextTarget *LinkContinuationTarget `json:"next_target,omitempty"`
	// RecordIdentity is the accepted resource identity observed at the
	// declared record identity pointer on the records that produced this
	// continuation. Every later continuation response must repeat it.
	RecordIdentity  *contract.PresentValue `json:"record_identity,omitempty"`
	SourceExhausted bool                   `json:"source_exhausted"`
}

// LinkContinuationTarget is one sealed provider-issued continuation target. It
// is bounded opaque server state: the path is stored relative to the admitted
// destination's base path, and the query is preserved verbatim so a provider
// cursor such as `after` survives without ever becoming a typed input.
type LinkContinuationTarget struct {
	Origin      string `json:"origin"`
	EscapedPath string `json:"escaped_path"`
	RawQuery    string `json:"raw_query"`
}

const (
	maxLinkTargetPathBytes  = 2048
	maxLinkTargetQueryBytes = 4096
	maxLinkTargetCursors    = 16
	maxLinkIdentityBytes    = 4096
)

// ValidateLinkContinuationTarget validates the bounded shape of one sealed
// provider continuation target. It is the structural half of reuse validation;
// the origin, path and query are additionally re-checked against the admitted
// destination and the accepted request before any request is built.
func ValidateLinkContinuationTarget(target LinkContinuationTarget) error {
	if target.Origin == "" || len(target.Origin) > 512 {
		return fmt.Errorf("%w: sealed provider target origin is invalid", ErrNavigationConflict)
	}
	if _, err := canonicalOrigin(target.Origin); err != nil {
		return fmt.Errorf("%w: sealed provider target origin is invalid", ErrNavigationConflict)
	}
	if target.EscapedPath == "" || len(target.EscapedPath) > maxLinkTargetPathBytes {
		return fmt.Errorf("%w: sealed provider target path is invalid", ErrNavigationConflict)
	}
	parsed, err := url.Parse(target.EscapedPath)
	if err != nil || parsed.IsAbs() || parsed.Host != "" || parsed.User != nil || parsed.Opaque != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" || !strings.HasPrefix(parsed.Path, "/") ||
		unsafeHTTPPath(parsed.Path) {
		return fmt.Errorf("%w: sealed provider target path is invalid", ErrNavigationConflict)
	}
	if len(target.RawQuery) > maxLinkTargetQueryBytes || strings.ContainsRune(target.RawQuery, '#') {
		return fmt.Errorf("%w: sealed provider target query is invalid", ErrNavigationConflict)
	}
	if _, err := url.ParseQuery(target.RawQuery); err != nil {
		return fmt.Errorf("%w: sealed provider target query is invalid", ErrNavigationConflict)
	}
	return nil
}

// ContinuationCodec owns opaque authenticated continuation state. Runtime code
// never serializes source navigation or unread records into a client token.
type ContinuationCodec interface {
	OpenContinuation(context.Context, ContinuationBinding, contract.PresentValue) (ContinuationState, error)
	SealContinuation(context.Context, ContinuationBinding, ContinuationState) (contract.PresentValue, error)
}

type preparedPagination struct {
	input contract.PresentValue
	state *ContinuationState
}
type pageHTTPContext struct {
	RequestURL     *url.URL
	ResponseHeader http.Header
	Authorization  *contract.OperationAuthorization
	// BasePath is the admitted destination's base path. Provider-issued target
	// paths are validated and sealed relative to it.
	BasePath string
	// RequireTLS repeats the server policy that admitted the destination.
	RequireTLS bool
	// RecordIdentity is the accepted resource identity a continuation response
	// must repeat through the declared record identity pointer.
	RecordIdentity *contract.PresentValue
}

func preparePagination(
	ctx context.Context,
	operation contract.Operation,
	pagination *contract.OperationPagination,
	local bool,
	input contract.PresentValue,
	continuation contract.PresentValue,
	codec ContinuationCodec,
	binding ContinuationBinding,
) (preparedPagination, error) {
	if continuation.Presence() == "" {
		continuation = contract.AbsentValue()
	}
	if err := continuation.Validate(); err != nil {
		return preparedPagination{}, fmt.Errorf("%w: continuation value is invalid", ErrInvalidHTTPInput)
	}
	if pagination == nil && !local {
		if continuation.Presence() != contract.PresenceAbsent {
			return preparedPagination{}, ErrNavigationConflict
		}
		return preparedPagination{input: input}, nil
	}
	if err := validatePaginationFact(operation, pagination); err != nil {
		return preparedPagination{}, err
	}
	if continuation.Presence() == contract.PresenceAbsent {
		return preparedPagination{input: input}, nil
	}
	if codec == nil {
		return preparedPagination{}, ErrContinuationRequired
	}
	fields, ok := input.Fields()
	if !ok {
		return preparedPagination{}, ErrInvalidHTTPInput
	}
	for fieldID := range fields {
		if !operationDeclaresFieldID(operation, fieldID) {
			return preparedPagination{}, fmt.Errorf("%w: continuation input contains an unknown field", ErrInvalidHTTPInput)
		}
	}
	navigationField := PaginationNavigationField(pagination)
	if value, supplied := fields[navigationField]; navigationField != "" && supplied && value.Presence() != contract.PresenceAbsent {
		return preparedPagination{}, ErrNavigationConflict
	}
	state, err := codec.OpenContinuation(ctx, binding, continuation)
	if err != nil {
		return preparedPagination{}, fmt.Errorf("%w: continuation was not accepted", ErrNavigationConflict)
	}
	if err := validateContinuationState(operation, pagination, local, state); err != nil {
		return preparedPagination{}, err
	}
	if len(state.UnreadTail) == 0 && len(state.NextInputs) == 0 {
		return preparedPagination{}, fmt.Errorf("%w: continuation carries no remaining authority", ErrNavigationConflict)
	}
	limitField := PaginationLimitField(pagination)
	if state.EffectiveWireLimit != nil {
		sentLimit, limitErr := effectiveWireLimit(input, pagination)
		// A continuation restores the page size the provider honoured, which may
		// be smaller than the size the producing run sent. A caller that repeats
		// that larger declared size is served at the restored size and never
		// widens the outbound request; declaring a size smaller than the one the
		// continuation was granted still conflicts with this read.
		if limitErr != nil || (sentLimit != nil && *sentLimit < *state.EffectiveWireLimit) {
			return preparedPagination{}, ErrNavigationConflict
		}
	} else if limitField != "" {
		sentLimit, limitErr := effectiveWireLimit(input, pagination)
		if limitErr != nil || sentLimit != nil {
			return preparedPagination{}, ErrNavigationConflict
		}
	}
	merged := make(map[string]contract.PresentValue, len(fields)+len(state.NextInputs))
	for fieldID, value := range fields {
		merged[fieldID] = value
	}
	for fieldID, value := range state.NextInputs {
		merged[fieldID] = value
	}
	if state.EffectiveWireLimit != nil {
		limit, _ := contract.NewIntegerValue(strconv.FormatUint(*state.EffectiveWireLimit, 10))
		merged[limitField] = limit
	}
	effectiveInput := contract.NewObjectValue(merged)
	if len(state.NextInputs) != 0 {
		if err := contract.ValidateOperationInput(operation, effectiveInput); err != nil {
			return preparedPagination{}, fmt.Errorf("%w: restored continuation input is incomplete: %v", ErrInvalidHTTPInput, err)
		}
	}
	return preparedPagination{input: effectiveInput, state: &state}, nil
}

func validateContinuationState(
	operation contract.Operation,
	pagination *contract.OperationPagination,
	local bool,
	state ContinuationState,
) error {
	if pagination == nil && !local {
		return fmt.Errorf("%w: continuation has no admitted pagination authority", ErrNavigationConflict)
	}
	if state.MediaType == "" && (len(state.UnreadTail) != 0 || len(state.NextInputs) != 0) {
		return fmt.Errorf("%w: continuation media type is absent", ErrNavigationConflict)
	}
	if state.MediaType != "" && !isJSONWireMediaType(state.MediaType) {
		return fmt.Errorf("%w: continuation media type is invalid", ErrNavigationConflict)
	}
	if state.MediaType != "" {
		declared := false
		for _, response := range operation.Responses {
			if !response.NoContent && response.MediaType == state.MediaType {
				declared = true
				break
			}
		}
		if !declared {
			return fmt.Errorf("%w: continuation media type is not declared by the selected operation", ErrNavigationConflict)
		}
	}
	if state.EffectiveWireLimit != nil && *state.EffectiveWireLimit == 0 {
		return fmt.Errorf("%w: continuation wire limit is zero", ErrNavigationConflict)
	}
	if PaginationLimitField(pagination) == "" && state.EffectiveWireLimit != nil {
		return fmt.Errorf("%w: continuation carries an undeclared wire limit", ErrNavigationConflict)
	}
	if state.NextInputs == nil {
		return fmt.Errorf("%w: continuation navigation map is not explicit", ErrNavigationConflict)
	}
	if state.ProviderTotal != nil {
		if pagination == nil || pagination.Kind != contract.OperationPaginationOffset ||
			pagination.TotalResponsePointer == "" {
			return fmt.Errorf("%w: continuation carries an undeclared provider total", ErrNavigationConflict)
		}
		value, err := contract.NewIntegerValue(state.ProviderTotal.Text)
		exact, ok := value.ExactNumber()
		if err != nil || !ok || exact != *state.ProviderTotal || strings.HasPrefix(exact.Text, "-") {
			return fmt.Errorf("%w: continuation provider total is invalid", ErrNavigationConflict)
		}
	}
	if state.UnreadTail == nil {
		return fmt.Errorf("%w: continuation tail is not explicit", ErrNavigationConflict)
	}
	for _, value := range state.UnreadTail {
		if err := value.Validate(); err != nil {
			return fmt.Errorf("%w: continuation tail is invalid", ErrNavigationConflict)
		}
	}
	allowedField := PaginationNavigationField(pagination)
	for fieldID, value := range state.NextInputs {
		if fieldID != allowedField || allowedField == "" {
			return fmt.Errorf("%w: continuation carries a non-navigation field", ErrNavigationConflict)
		}
		binding, found := findOperationBinding(operation, fieldID)
		if !found || value.Presence() != contract.PresenceValue {
			return fmt.Errorf("%w: continuation navigation value is invalid", ErrNavigationConflict)
		}
		switch binding.Schema.Type {
		case contract.SchemaTypeString:
			if value.Kind() != contract.ValueKindString {
				return fmt.Errorf("%w: continuation cursor type is invalid", ErrNavigationConflict)
			}
		case contract.SchemaTypeInteger:
			if value.Kind() != contract.ValueKindInteger {
				return fmt.Errorf("%w: continuation counter type is invalid", ErrNavigationConflict)
			}
		default:
			return fmt.Errorf("%w: continuation navigation schema is unsupported", ErrNavigationConflict)
		}
	}
	linkStrategy := pagination != nil && pagination.Kind == contract.OperationPaginationLink
	if state.NextTarget != nil {
		if !linkStrategy || state.SourceExhausted {
			return fmt.Errorf("%w: continuation carries provider navigation without an admitted link strategy", ErrNavigationConflict)
		}
		if err := ValidateLinkContinuationTarget(*state.NextTarget); err != nil {
			return err
		}
	} else if linkStrategy && !state.SourceExhausted {
		return fmt.Errorf("%w: link continuation lacks its sealed provider target", ErrNavigationConflict)
	}
	identityDeclared := linkStrategy && pagination.LinkRecordIdentityPointer != ""
	if identityDeclared {
		if state.RecordIdentity != nil {
			if err := validateLinkRecordIdentity(*state.RecordIdentity); err != nil {
				return err
			}
		} else if state.NextTarget != nil {
			return fmt.Errorf("%w: link continuation lacks its accepted record identity", ErrNavigationConflict)
		}
	} else if state.RecordIdentity != nil {
		return fmt.Errorf("%w: continuation carries an undeclared record identity", ErrNavigationConflict)
	}
	if state.SourceExhausted && len(state.NextInputs) != 0 {
		return fmt.Errorf("%w: exhausted continuation retains provider navigation", ErrNavigationConflict)
	}
	if !state.SourceExhausted && len(state.NextInputs) == 0 {
		return fmt.Errorf("%w: non-exhausted continuation lacks provider navigation", ErrNavigationConflict)
	}
	return nil
}

func effectiveWireLimit(input contract.PresentValue, pagination *contract.OperationPagination) (*uint64, error) {
	fieldID := PaginationLimitField(pagination)
	if fieldID == "" {
		return nil, nil
	}
	fields, _ := input.Fields()
	value, sent := fields[fieldID]
	if !sent || value.Presence() == contract.PresenceAbsent {
		return nil, nil
	}
	number, ok := value.ExactNumber()
	if !ok || value.Kind() != contract.ValueKindInteger {
		return nil, fmt.Errorf("%w: sent page size is not an exact integer", ErrInvalidPaginationFact)
	}
	parsed, err := strconv.ParseUint(number.Text, 10, 64)
	if err != nil || parsed == 0 {
		return nil, fmt.Errorf("%w: sent page size is not a positive uint64", ErrInvalidPaginationFact)
	}
	return new(parsed), nil
}

func applyPage(
	ctx context.Context,
	operation contract.Operation,
	decoded DecodedResponse,
	pagination *contract.OperationPagination,
	input contract.PresentValue,
	limits CallLimits,
	controls contract.ResultControls,
	codec ContinuationCodec,
	binding ContinuationBinding,
	httpContext pageHTTPContext,
) (contract.ActionResult, error) {
	if decoded.Result == nil {
		return contract.ActionResult{}, fmt.Errorf("%w: direct read has no action result", ErrUnexpectedHTTPResponse)
	}
	if err := controls.Validate(); err != nil {
		return contract.ActionResult{}, fmt.Errorf("%w: invalid result controls", ErrInvalidHTTPInput)
	}
	if pagination != nil && !responseCarriesPaginatedRecords(operation, decoded) {
		return contract.ApplyResultControls(*decoded.Result, controls)
	}
	if pagination == nil && limits.TotalRecordCap == nil {
		return contract.ApplyResultControls(*decoded.Result, controls)
	}
	if decoded.Result.Data.Presence() == contract.PresenceAbsent {
		if pagination != nil {
			return contract.ActionResult{}, fmt.Errorf("%w: paginated response has no action data", ErrInvalidProviderValue)
		}
		return contract.ApplyResultControls(*decoded.Result, controls)
	}
	records, ok := decoded.Result.Data.Elements()
	if !ok {
		return contract.ActionResult{}, fmt.Errorf("%w: paginated result is not an array", ErrInvalidProviderValue)
	}
	wireLimit, err := effectiveWireLimit(input, pagination)
	if err != nil {
		return contract.ActionResult{}, err
	}
	providerTotal, nextState, exhausted, declaredLimit, err := nextPaginationStateForOperation(
		operation,
		decoded,
		pagination,
		input,
		uint64(len(records)),
		wireLimit,
		httpContext,
	)
	if err != nil {
		return contract.ActionResult{}, err
	}
	// A provider that clamps the declared page size serves fewer records than
	// the runtime sent. The size the provider itself declared for the pages it
	// serves is then the page's effective wire limit, and it is also the size
	// the continuation restores for the next request. A page carrying more
	// records than that declared size is navigation that contradicts the page
	// the provider served, so it is refused instead of reported.
	effectiveLimit := wireLimit
	if declaredLimit != nil {
		if *declaredLimit < uint64(len(records)) {
			return contract.ActionResult{}, fmt.Errorf(
				"%w: declared page size is smaller than the page the provider served", ErrNavigationConflict)
		}
		effectiveLimit = declaredLimit
	}
	emitted, tail, err := applyRecordControls(records, decoded.Result.MediaType, controls, limits.TotalRecordCap)
	if err != nil {
		return contract.ActionResult{}, err
	}
	nextState.MediaType = decoded.Result.MediaType
	nextState.EffectiveWireLimit = cloneUint64(effectiveLimit)
	nextState.ProviderTotal = cloneExactNumber(providerTotal)
	nextState.SourceExhausted = exhausted
	continuedTail := nextState.UnreadTail
	nextState.UnreadTail = make([]contract.PresentValue, 0, len(tail)+len(continuedTail))
	nextState.UnreadTail = append(nextState.UnreadTail, tail...)
	nextState.UnreadTail = append(nextState.UnreadTail, continuedTail...)
	if err := validateContinuationState(operation, pagination, pagination == nil, nextState); err != nil {
		return contract.ActionResult{}, err
	}
	continuation := contract.AbsentValue()
	if len(nextState.UnreadTail) != 0 || len(nextState.NextInputs) != 0 {
		if codec == nil {
			return contract.ActionResult{}, ErrContinuationRequired
		}
		continuation, err = codec.SealContinuation(ctx, binding, nextState)
		if err != nil || continuation.Presence() != contract.PresenceValue {
			return contract.ActionResult{}, fmt.Errorf("%w: continuation could not be sealed", ErrContinuationRequired)
		}
		exhausted = false
	}
	result := contract.ActionResult{
		MediaType: decoded.Result.MediaType,
		Data:      contract.NewArrayValue(emitted),
		Pagination: &contract.ResultPagination{
			EffectiveWireLimit: cloneUint64(effectiveLimit),
			TotalRecordCap:     cloneUint64(limits.TotalRecordCap),
			Continuation:       continuation,
			UnreadTail:         clonePresentValues(tail),
			FetchedCount:       uint64(len(records)),
			ReturnedCount:      uint64(len(records)),
			EmittedCount:       uint64(len(emitted)),
			ProviderTotal:      providerTotal,
			SourceExhausted:    exhausted,
		},
	}
	if err := result.Validate(); err != nil {
		return contract.ActionResult{}, fmt.Errorf("%w: paginated result is invalid: %v", ErrInvalidProviderValue, err)
	}
	return result, nil
}

func nextPaginationState(
	decoded DecodedResponse,
	pagination *contract.OperationPagination,
	input contract.PresentValue,
	returned uint64,
	wireLimit *uint64,
) (*contract.ExactNumber, ContinuationState, bool, *uint64, error) {
	return nextPaginationStateForOperation(
		contract.Operation{},
		decoded,
		pagination,
		input,
		returned,
		wireLimit,
		pageHTTPContext{},
	)
}

// nextPaginationStateForOperation resolves one page's continuation decision: the
// provider-declared total, the next request's navigation and exhaustion state,
// and the page size a provider Link navigation declared for the pages it
// serves. That declared size is nil unless the provider named a positive size
// strictly smaller than the one sent, and the caller then reports and restores
// the declared size instead of the sent one.
func nextPaginationStateForOperation(
	operation contract.Operation,
	decoded DecodedResponse,
	pagination *contract.OperationPagination,
	input contract.PresentValue,
	returned uint64,
	wireLimit *uint64,
	httpContext pageHTTPContext,
) (*contract.ExactNumber, ContinuationState, bool, *uint64, error) {
	state := ContinuationState{UnreadTail: []contract.PresentValue{}, NextInputs: map[string]contract.PresentValue{}}
	if pagination == nil {
		return nil, state, true, nil, nil
	}
	switch pagination.Kind {
	case contract.OperationPaginationCursor:
		value, present, err := optionalValueAtJSONPointer(decoded.Document, pagination.CursorResponsePointer)
		if err != nil {
			return nil, state, false, nil, fmt.Errorf("%w: cursor response is malformed", ErrInvalidProviderValue)
		}
		if !present || value.Presence() == contract.PresenceNull {
			return nil, state, true, nil, nil
		}
		state.NextInputs[pagination.CursorRequestField] = value
		return nil, state, false, nil, nil
	case contract.OperationPaginationOffset:
		var (
			total        *contract.ExactNumber
			totalInteger *big.Int
		)
		if pagination.TotalResponsePointer != "" {
			totalValue, present, err := optionalValueAtJSONPointer(decoded.Document, pagination.TotalResponsePointer)
			if err != nil || !present {
				return nil, state, false, nil, fmt.Errorf("%w: provider total is absent", ErrInvalidProviderValue)
			}
			exact, ok := totalValue.ExactNumber()
			if !ok || totalValue.Kind() != contract.ValueKindInteger || strings.HasPrefix(exact.Text, "-") {
				return nil, state, false, nil, fmt.Errorf("%w: provider total is not an exact nonnegative integer", ErrInvalidProviderValue)
			}
			totalInteger, ok = new(big.Int).SetString(exact.Text, 10)
			if !ok {
				return nil, state, false, nil, ErrInvalidProviderValue
			}
			total = &exact
		}
		current, sent, err := navigationUint64(input, pagination.OffsetRequestField)
		if err != nil {
			return nil, state, false, nil, err
		}
		if !sent {
			if totalInteger != nil && totalInteger.Sign() == 0 && returned == 0 {
				return total, state, true, nil, nil
			}
			return nil, state, false, nil, fmt.Errorf("%w: offset input is absent and no source default supplied a navigation base", ErrInvalidProviderValue)
		}
		if returned == 0 {
			if totalInteger != nil && new(big.Int).SetUint64(current).Cmp(totalInteger) < 0 {
				return nil, state, false, nil, fmt.Errorf("%w: offset pagination made no progress before provider total", ErrInvalidProviderValue)
			}
			return total, state, true, nil, nil
		}
		if ^uint64(0)-current < returned {
			return nil, state, false, nil, fmt.Errorf("%w: next offset overflows uint64", ErrInvalidProviderValue)
		}
		next := current + returned
		if totalInteger != nil && new(big.Int).SetUint64(next).Cmp(totalInteger) >= 0 {
			return total, state, true, nil, nil
		}
		if totalInteger == nil && wireLimit != nil && returned < *wireLimit {
			return nil, state, true, nil, nil
		}
		state.NextInputs[pagination.OffsetRequestField], _ = contract.NewIntegerValue(strconv.FormatUint(next, 10))
		return total, state, false, nil, nil
	case contract.OperationPaginationPage:
		hasLastPageEvidence := pagination.LastPageResponsePointer != ""
		if hasLastPageEvidence {
			lastValue, present, err := optionalValueAtJSONPointer(decoded.Document, pagination.LastPageResponsePointer)
			if err != nil || !present {
				return nil, state, false, nil, fmt.Errorf("%w: last-page evidence is absent", ErrInvalidProviderValue)
			}
			last, ok := lastValue.Boolean()
			if !ok {
				return nil, state, false, nil, fmt.Errorf("%w: last-page evidence is not boolean", ErrInvalidProviderValue)
			}
			if last {
				return nil, state, true, nil, nil
			}
		}
		page, sent, err := navigationUint64(input, pagination.PageRequestField)
		if err != nil {
			return nil, state, false, nil, err
		}
		if !sent {
			return nil, state, false, nil, fmt.Errorf("%w: page input is absent and no source default supplied a navigation base", ErrInvalidProviderValue)
		}
		if returned == 0 {
			if hasLastPageEvidence {
				return nil, state, false, nil, fmt.Errorf("%w: page pagination made no progress before declared last page", ErrInvalidProviderValue)
			}
			return nil, state, true, nil, nil
		}
		if page == ^uint64(0) {
			return nil, state, false, nil, fmt.Errorf("%w: next page overflows uint64", ErrInvalidProviderValue)
		}
		state.NextInputs[pagination.PageRequestField], _ = contract.NewIntegerValue(strconv.FormatUint(page+1, 10))
		return nil, state, false, nil, nil
	case contract.OperationPaginationLink:
		return nextLinkPaginationState(operation, pagination, input, decoded, httpContext)
	default:
		return nil, state, false, nil, ErrInvalidPaginationFact
	}
}

type rfc8288Link struct {
	target string
	rels   []string
	anchor bool
}

func nextLinkPaginationState(
	operation contract.Operation,
	pagination *contract.OperationPagination,
	input contract.PresentValue,
	decoded DecodedResponse,
	httpContext pageHTTPContext,
) (*contract.ExactNumber, ContinuationState, bool, *uint64, error) {
	state := ContinuationState{UnreadTail: []contract.PresentValue{}, NextInputs: map[string]contract.PresentValue{}}
	if pagination == nil || httpContext.RequestURL == nil || httpContext.ResponseHeader == nil {
		return nil, state, false, nil, ErrInvalidPaginationFact
	}
	next, err := nextRFC8288Relation(httpContext.ResponseHeader.Values(pagination.LinkResponseHeader), pagination.LinkResponseHeader)
	if err != nil {
		return nil, state, false, nil, err
	}
	if next == nil {
		if _, err := linkRecordIdentity(pagination, decoded, httpContext, false); err != nil {
			return nil, state, false, nil, err
		}
		return nil, state, true, nil, nil
	}
	target, navigation, declaredLimit, err := sealLinkContinuationTarget(operation, pagination, input, next.target, httpContext)
	if err != nil {
		return nil, state, false, nil, err
	}
	identity, err := linkRecordIdentity(pagination, decoded, httpContext, true)
	if err != nil {
		return nil, state, false, nil, err
	}
	state.NextInputs[pagination.LinkRequestField] = navigation
	state.NextTarget = target
	state.RecordIdentity = identity
	return nil, state, false, declaredLimit, nil
}

// nextRFC8288Relation returns the one unanchored next relation of a Link
// response header, nil when the response carries no next relation, and an
// error for a malformed or ambiguous header. A second next relation or an
// anchored one is never resolved by preference.
func nextRFC8288Relation(values []string, headerName string) (*rfc8288Link, error) {
	if len(values) == 0 {
		return nil, nil
	}
	links, err := parseRFC8288Links(values)
	if err != nil {
		return nil, fmt.Errorf("%w: %s response is malformed", ErrNavigationConflict, headerName)
	}
	var next *rfc8288Link
	for index := range links {
		if !linkHasRelation(links[index], "next") {
			continue
		}
		if next != nil || links[index].anchor {
			return nil, fmt.Errorf("%w: %s next relation is ambiguous", ErrNavigationConflict, headerName)
		}
		next = &links[index]
	}
	return next, nil
}

// sealLinkContinuationTarget validates one provider-issued next target against
// the authorized request and returns the bounded opaque state that carries it:
// the admitted origin, the path rebased onto the admitted base path and the
// query verbatim. The declared navigation value is extracted as a typed
// continuation input and the page size the provider declared for the pages it
// serves is reported. A target that leaves the declared routes, changes an
// accepted parameter, embeds a credential, carries an undeclared declared
// field or makes no progress is refused instead of followed.
func sealLinkContinuationTarget(
	operation contract.Operation,
	pagination *contract.OperationPagination,
	input contract.PresentValue,
	rawTarget string,
	httpContext pageHTTPContext,
) (*LinkContinuationTarget, contract.PresentValue, *uint64, error) {
	binding, found := findOperationBinding(operation, pagination.LinkRequestField)
	if !found || binding.Location != contract.FieldLocationQuery ||
		binding.Sensitivity != contract.SensitivityPublic ||
		(binding.Schema.Type != contract.SchemaTypeString && binding.Schema.Type != contract.SchemaTypeInteger) {
		return nil, contract.PresentValue{}, nil, ErrInvalidPaginationFact
	}
	if rawTarget == "" || strings.Contains(rawTarget, "#") {
		return nil, contract.PresentValue{}, nil, fmt.Errorf("%w: Link next target has a fragment or is empty", ErrNavigationConflict)
	}
	reference, err := url.Parse(rawTarget)
	if err != nil || reference.User != nil || reference.Opaque != "" || reference.Fragment != "" {
		return nil, contract.PresentValue{}, nil, fmt.Errorf("%w: Link next target is invalid", ErrNavigationConflict)
	}
	target := httpContext.RequestURL.ResolveReference(reference)
	if !sameOriginURL(target, httpContext.RequestURL) {
		return nil, contract.PresentValue{}, nil, fmt.Errorf("%w: Link next target changes origin", ErrNavigationConflict)
	}
	if httpContext.RequireTLS && !strings.EqualFold(target.Scheme, "https") {
		return nil, contract.PresentValue{}, nil, fmt.Errorf("%w: Link next target is not HTTPS", ErrNavigationConflict)
	}
	origin, err := canonicalOrigin(target.Scheme + "://" + target.Host)
	if err != nil {
		return nil, contract.PresentValue{}, nil, fmt.Errorf("%w: Link next origin is invalid", ErrNavigationConflict)
	}
	relativePath, err := rebaseContinuationPath(target.EscapedPath(), httpContext.BasePath)
	if err != nil {
		return nil, contract.PresentValue{}, nil, err
	}
	sentPath, err := rebaseContinuationPath(httpContext.RequestURL.EscapedPath(), httpContext.BasePath)
	if err != nil {
		return nil, contract.PresentValue{}, nil, err
	}
	// The admitted route set is the declared operation path rendered with the
	// accepted path bindings plus every declared additional route template. A
	// provider target that returns to the operation path after a route-template
	// continuation is admitted, and any other path is refused exactly as before.
	renderedPath := ""
	if encoded, encodeErr := EncodeInputs(operation, input); encodeErr == nil {
		renderedPath, _ = rebaseContinuationPath(encoded.EscapedPath, httpContext.BasePath)
	}
	if relativePath != sentPath && relativePath != renderedPath &&
		!linkRouteAdmitsPath(relativePath, pagination.LinkRoutes) {
		return nil, contract.PresentValue{}, nil, fmt.Errorf("%w: Link next target changes the operation path", ErrNavigationConflict)
	}
	if len(target.RawQuery) > maxLinkTargetQueryBytes {
		return nil, contract.PresentValue{}, nil, fmt.Errorf("%w: Link next query exceeds its bound", ErrNavigationConflict)
	}
	currentQuery, err := url.ParseQuery(httpContext.RequestURL.RawQuery)
	if err != nil {
		return nil, contract.PresentValue{}, nil, fmt.Errorf("%w: sent query is malformed", ErrNavigationConflict)
	}
	nextQuery, err := url.ParseQuery(target.RawQuery)
	if err != nil {
		return nil, contract.PresentValue{}, nil, fmt.Errorf("%w: Link next query is malformed", ErrNavigationConflict)
	}
	if authorization := httpContext.Authorization; authorization != nil && authorization.Placement == contract.FieldLocationQuery {
		if strings.EqualFold(authorization.WireName, binding.WireName) {
			return nil, contract.PresentValue{}, nil, ErrInvalidPaginationFact
		}
		if _, embedded := nextQuery[authorization.WireName]; embedded {
			return nil, contract.PresentValue{}, nil, fmt.Errorf("%w: Link next target embeds a credential channel", ErrNavigationConflict)
		}
		delete(currentQuery, authorization.WireName)
	}
	navigation, present := nextQuery[binding.WireName]
	if !present || len(navigation) != 1 {
		return nil, contract.PresentValue{}, nil, fmt.Errorf("%w: Link next navigation is absent or duplicated", ErrNavigationConflict)
	}
	if current := currentQuery[binding.WireName]; len(current) > 1 {
		return nil, contract.PresentValue{}, nil, fmt.Errorf("%w: sent navigation is duplicated", ErrNavigationConflict)
	}
	delete(currentQuery, binding.WireName)
	delete(nextQuery, binding.WireName)
	// A provider may clamp the page size it was sent. The target then names a
	// positive integer strictly smaller than the size actually sent for the
	// declared page-size field, and that is the only admitted change of a
	// declared parameter besides navigation.
	var declaredLimit *uint64
	if limitField := PaginationLimitField(pagination); limitField != "" {
		limitBinding, declared := findOperationBinding(operation, limitField)
		if !declared || limitBinding.Location != contract.FieldLocationQuery || limitBinding.WireName == "" {
			return nil, contract.PresentValue{}, nil, ErrInvalidPaginationFact
		}
		declaredLimit, err = declaredPageSize(
			currentQuery[limitBinding.WireName],
			nextQuery[limitBinding.WireName],
		)
		if err != nil {
			return nil, contract.PresentValue{}, nil, err
		}
		delete(currentQuery, limitBinding.WireName)
		delete(nextQuery, limitBinding.WireName)
	}
	// Every parameter the request carried must survive the provider target
	// unchanged; the target may only add parameters the admitted source does
	// not declare, which are the provider's opaque cursor namespace. A declared
	// parameter the request did not carry is a semantic change, not navigation.
	for name, sentValues := range currentQuery {
		targetValues, carried := nextQuery[name]
		if !carried || !slices.Equal(sentValues, targetValues) {
			return nil, contract.PresentValue{}, nil, fmt.Errorf("%w: Link next target changes unrelated query authority", ErrNavigationConflict)
		}
		delete(nextQuery, name)
	}
	for name := range nextQuery {
		if declaredQueryWireName(operation, name) {
			return nil, contract.PresentValue{}, nil, fmt.Errorf("%w: Link next target adds an unsent declared query field", ErrNavigationConflict)
		}
		if !validLinkCursorName(name) {
			return nil, contract.PresentValue{}, nil, fmt.Errorf("%w: Link next target carries an invalid cursor parameter", ErrNavigationConflict)
		}
	}
	if len(nextQuery) > maxLinkTargetCursors {
		return nil, contract.PresentValue{}, nil, fmt.Errorf("%w: Link next target carries too many cursor parameters", ErrNavigationConflict)
	}
	var value contract.PresentValue
	switch binding.Schema.Type {
	case contract.SchemaTypeString:
		value = contract.NewStringValue(navigation[0])
	case contract.SchemaTypeInteger:
		value, err = contract.NewIntegerValue(navigation[0])
		if err != nil {
			return nil, contract.PresentValue{}, nil, fmt.Errorf("%w: Link next integer is invalid", ErrNavigationConflict)
		}
	}
	pairs, err := encodeParameter(binding, value)
	if err != nil || len(pairs) != 1 {
		return nil, contract.PresentValue{}, nil, fmt.Errorf("%w: Link next navigation cannot be encoded as one query value", ErrNavigationConflict)
	}
	pairs[0].allowReserved = binding.AllowReserved
	futureQuery, err := url.ParseQuery(encodeQueryPairs(pairs))
	futureNavigation := futureQuery[binding.WireName]
	if err != nil || len(futureQuery) != 1 || len(futureNavigation) != 1 ||
		futureNavigation[0] != navigation[0] {
		return nil, contract.PresentValue{}, nil, fmt.Errorf("%w: Link next navigation changes authority when re-encoded", ErrNavigationConflict)
	}
	fields, _ := input.Fields()
	if current, supplied := fields[pagination.LinkRequestField]; supplied &&
		current.Presence() != contract.PresenceAbsent && current.Equal(value) {
		return nil, contract.PresentValue{}, nil, fmt.Errorf("%w: Link next navigation makes no progress", ErrNavigationConflict)
	}
	if relativePath == sentPath && target.RawQuery == httpContext.RequestURL.RawQuery {
		return nil, contract.PresentValue{}, nil, fmt.Errorf("%w: Link next target makes no progress", ErrNavigationConflict)
	}
	sealed := &LinkContinuationTarget{Origin: origin.String(), EscapedPath: relativePath, RawQuery: target.RawQuery}
	if err := ValidateLinkContinuationTarget(*sealed); err != nil {
		return nil, contract.PresentValue{}, nil, err
	}
	return sealed, value, declaredLimit, nil
}

// continuationRecordIdentity returns the accepted resource identity one
// continuation response must repeat through the declared record identity
// pointer, or nil when this request is not a continuation.
func continuationRecordIdentity(state *ContinuationState) *contract.PresentValue {
	if state == nil {
		return nil
	}
	return state.RecordIdentity
}

// applyContinuationTarget re-validates one sealed provider target against the
// admitted destination and the request PM is about to send, then applies it:
// the provider's path replaces the rendered operation path and the provider's
// opaque cursor parameters are appended verbatim. Every parameter PM would send
// must survive the target unchanged, so a tampered sealed target can never
// change an accepted query, leave the admitted origin, or address a route the
// admitted source does not declare.
func applyContinuationTarget(
	encoded *EncodedInputs,
	operation contract.Operation,
	pagination *contract.OperationPagination,
	state ContinuationState,
	origin *url.URL,
	requireTLS bool,
	authorization *contract.OperationAuthorization,
) error {
	target := state.NextTarget
	if target == nil {
		return nil
	}
	if encoded == nil || pagination == nil || pagination.Kind != contract.OperationPaginationLink {
		return fmt.Errorf("%w: sealed provider target has no admitted link strategy", ErrNavigationConflict)
	}
	if err := ValidateLinkContinuationTarget(*target); err != nil {
		return err
	}
	sealed, err := canonicalOrigin(target.Origin)
	if err != nil || origin == nil || !sameOrigin(sealed, origin) {
		return fmt.Errorf("%w: sealed provider target is not on the admitted origin", ErrNavigationConflict)
	}
	if requireTLS && !strings.EqualFold(sealed.Scheme, "https") {
		return fmt.Errorf("%w: sealed provider target is not HTTPS", ErrNavigationConflict)
	}
	if target.EscapedPath != encoded.EscapedPath && !linkRouteAdmitsPath(target.EscapedPath, pagination.LinkRoutes) {
		return fmt.Errorf("%w: sealed provider target changes the operation path", ErrNavigationConflict)
	}
	targetQuery, err := url.ParseQuery(target.RawQuery)
	if err != nil {
		return fmt.Errorf("%w: sealed provider target query is malformed", ErrNavigationConflict)
	}
	encodedQuery, err := url.ParseQuery(encoded.RawQuery)
	if err != nil {
		return fmt.Errorf("%w: encoded request query is malformed", ErrNavigationConflict)
	}
	for name, values := range encodedQuery {
		targetValues, carried := targetQuery[name]
		if !carried || !slices.Equal(values, targetValues) {
			return fmt.Errorf("%w: sealed provider target changes the accepted query", ErrNavigationConflict)
		}
	}
	for name := range targetQuery {
		if !declaredQueryWireName(operation, name) {
			continue
		}
		if _, accepted := encodedQuery[name]; !accepted {
			return fmt.Errorf("%w: sealed provider target adds an unaccepted declared query field", ErrNavigationConflict)
		}
	}
	cursors, err := linkTargetCursorPairs(operation, target.RawQuery, authorization)
	if err != nil {
		return err
	}
	encoded.EscapedPath = target.EscapedPath
	if len(cursors) != 0 {
		joined := strings.Join(cursors, "&")
		if encoded.RawQuery == "" {
			encoded.RawQuery = joined
		} else {
			encoded.RawQuery += "&" + joined
		}
	}
	return nil
}

// linkTargetCursorPairs returns the provider-issued query parameters that name
// no declared input field, verbatim and in their issued order. They are the
// provider's opaque cursor namespace: a cursor such as `after` survives exactly
// as the provider wrote it and never becomes a typed input.
func linkTargetCursorPairs(
	operation contract.Operation,
	rawQuery string,
	authorization *contract.OperationAuthorization,
) ([]string, error) {
	if rawQuery == "" {
		return nil, nil
	}
	cursors := make([]string, 0, maxLinkTargetCursors)
	for _, pair := range strings.Split(rawQuery, "&") {
		name, _, _ := strings.Cut(pair, "=")
		decoded, err := url.QueryUnescape(name)
		if err != nil || decoded == "" {
			return nil, fmt.Errorf("%w: sealed provider target query is malformed", ErrNavigationConflict)
		}
		if authorization != nil && authorization.Placement == contract.FieldLocationQuery &&
			decoded == authorization.WireName {
			return nil, fmt.Errorf("%w: sealed provider target embeds a credential channel", ErrNavigationConflict)
		}
		if declaredQueryWireName(operation, decoded) {
			continue
		}
		if !validLinkCursorName(decoded) {
			return nil, fmt.Errorf("%w: sealed provider target carries an invalid cursor parameter", ErrNavigationConflict)
		}
		if len(cursors) == maxLinkTargetCursors {
			return nil, fmt.Errorf("%w: sealed provider target carries too many cursor parameters", ErrNavigationConflict)
		}
		cursors = append(cursors, pair)
	}
	return cursors, nil
}

// linkRecordIdentity resolves the declared resource identity of one page from
// the records the provider returned. Every record must carry the declared
// field, all records of one page must agree, and a continuation response must
// repeat the identity an earlier page sealed. An empty page carries no new
// evidence: it keeps the accepted identity, and a page that must produce a
// continuation without any identity evidence is refused.
func linkRecordIdentity(
	pagination *contract.OperationPagination,
	decoded DecodedResponse,
	httpContext pageHTTPContext,
	required bool,
) (*contract.PresentValue, error) {
	if pagination.LinkRecordIdentityPointer == "" {
		return nil, nil
	}
	var records []contract.PresentValue
	if decoded.Result != nil {
		if elements, ok := decoded.Result.Data.Elements(); ok {
			records = elements
		}
	}
	var observed *contract.PresentValue
	for _, record := range records {
		value, present, err := optionalValueAtJSONPointer(record, pagination.LinkRecordIdentityPointer)
		if err != nil || !present || value.Presence() != contract.PresenceValue {
			return nil, fmt.Errorf("%w: continuation record does not identify its resource", ErrNavigationConflict)
		}
		if err := validateLinkRecordIdentity(value); err != nil {
			return nil, err
		}
		if observed == nil {
			observed = &value
			continue
		}
		if !observed.Equal(value) {
			return nil, fmt.Errorf("%w: continuation page records identify different resources", ErrNavigationConflict)
		}
	}
	if observed == nil {
		if httpContext.RecordIdentity != nil {
			return clonePresentValue(httpContext.RecordIdentity), nil
		}
		if required {
			return nil, fmt.Errorf("%w: link continuation has no accepted record identity", ErrNavigationConflict)
		}
		return nil, nil
	}
	if httpContext.RecordIdentity != nil && !observed.Equal(*httpContext.RecordIdentity) {
		return nil, fmt.Errorf("%w: continuation records identify a different resource", ErrNavigationConflict)
	}
	return observed, nil
}

func validateLinkRecordIdentity(value contract.PresentValue) error {
	switch value.Kind() {
	case contract.ValueKindString:
		text, ok := value.TextValue()
		if !ok || text == "" || len(text) > maxLinkIdentityBytes {
			return fmt.Errorf("%w: continuation record identity is invalid", ErrNavigationConflict)
		}
	case contract.ValueKindInteger:
		number, ok := value.ExactNumber()
		if !ok || len(number.Text) > maxLinkIdentityBytes {
			return fmt.Errorf("%w: continuation record identity is invalid", ErrNavigationConflict)
		}
	default:
		return fmt.Errorf("%w: continuation record identity is not a string or integer", ErrNavigationConflict)
	}
	return nil
}

// rebaseContinuationPath strips the admitted destination's base path from one
// admitted request path. Provider targets are sealed relative to that base, so
// a sealed path can never address a prefix outside the admitted destination.
func rebaseContinuationPath(escapedPath, basePath string) (string, error) {
	if escapedPath == "" || escapedPath[0] != '/' {
		return "", fmt.Errorf("%w: continuation target path is invalid", ErrNavigationConflict)
	}
	if basePath == "" || basePath == "/" {
		return escapedPath, nil
	}
	if escapedPath != basePath && !strings.HasPrefix(escapedPath, basePath+"/") {
		return "", fmt.Errorf("%w: continuation target leaves the admitted base path", ErrNavigationConflict)
	}
	relative := strings.TrimPrefix(escapedPath, basePath)
	if relative == "" {
		relative = "/"
	}
	return relative, nil
}

// linkRouteAdmitsPath reports whether one rebased provider path matches an
// additional route template the admitted source declares for the same resource.
func linkRouteAdmitsPath(escapedPath string, routes []string) bool {
	for _, route := range routes {
		if contract.LinkRouteMatches(escapedPath, route) {
			return true
		}
	}
	return false
}

// declaredQueryWireName reports whether one wire name belongs to a declared
// query binding of the operation, including its request variants.
func declaredQueryWireName(operation contract.Operation, name string) bool {
	for _, binding := range operation.Inputs {
		if binding.Location == contract.FieldLocationQuery && binding.WireName == name {
			return true
		}
	}
	for _, variant := range operation.RequestVariants {
		for _, binding := range variant.Inputs {
			if binding.Location == contract.FieldLocationQuery && binding.WireName == name {
				return true
			}
		}
	}
	return false
}

// validLinkCursorName bounds one provider cursor parameter name to printable
// ASCII that cannot re-parse as a query separator.
func validLinkCursorName(name string) bool {
	if name == "" {
		return false
	}
	for index := range len(name) {
		character := name[index]
		if character <= 0x20 || character >= 0x7f ||
			character == '&' || character == '=' || character == '#' ||
			character == '%' || character == '+' || character == '?' || character == '\\' {
			return false
		}
	}
	return true
}

// declaredPageSize compares the sent and the declared values of one page-size
// query parameter. Both absent, and a declared value that repeats the sent one
// verbatim, declare no size. A value written differently is admitted only as
// the provider's clamp: it must parse as a positive integer strictly smaller
// than the sent one. An increased or rewritten size, a size only one side
// carries, a duplicate and a value that is no positive integer all conflict
// with the sent request.
func declaredPageSize(sent, declared []string) (*uint64, error) {
	switch {
	case len(sent) == 0 && len(declared) == 0:
		return nil, nil
	case len(sent) == 0:
		return nil, fmt.Errorf("%w: Link next target declares a page size that was not sent", ErrNavigationConflict)
	case len(declared) == 0:
		return nil, fmt.Errorf("%w: Link next target drops the sent page size", ErrNavigationConflict)
	case len(sent) != 1:
		return nil, fmt.Errorf("%w: sent page size is duplicated", ErrNavigationConflict)
	case len(declared) != 1:
		return nil, fmt.Errorf("%w: Link next page size is duplicated", ErrNavigationConflict)
	case sent[0] == declared[0]:
		return nil, nil
	}
	sentSize, ok := queryPageSize(sent[0])
	if !ok {
		return nil, fmt.Errorf("%w: sent page size is not a positive integer", ErrInvalidPaginationFact)
	}
	declaredSize, ok := queryPageSize(declared[0])
	if !ok {
		return nil, fmt.Errorf("%w: Link next page size is not a positive integer", ErrNavigationConflict)
	}
	switch {
	case declaredSize > sentSize:
		return nil, fmt.Errorf("%w: Link next target increases the sent page size", ErrNavigationConflict)
	case declaredSize == sentSize:
		return nil, fmt.Errorf("%w: Link next page size is not the sent size", ErrNavigationConflict)
	default:
		return &declaredSize, nil
	}
}

// queryPageSize parses one page-size query value as a positive integer.
func queryPageSize(value string) (uint64, bool) {
	size, err := strconv.ParseUint(value, 10, 64)
	if err != nil || size == 0 {
		return 0, false
	}
	return size, true
}

func parseRFC8288Links(values []string) ([]rfc8288Link, error) {
	links := make([]rfc8288Link, 0)
	for _, value := range values {
		parts, err := splitLinkHeaderValues(value)
		if err != nil {
			return nil, err
		}
		for _, part := range parts {
			link, err := parseRFC8288Link(part)
			if err != nil {
				return nil, err
			}
			links = append(links, link)
		}
	}
	if len(links) == 0 {
		return nil, ErrNavigationConflict
	}
	return links, nil
}

func splitLinkHeaderValues(value string) ([]string, error) {
	parts := make([]string, 0, 1)
	start := 0
	inTarget := false
	inQuote := false
	escaped := false
	for index := range len(value) {
		character := value[index]
		switch {
		case escaped:
			escaped = false
		case inQuote && character == '\\':
			escaped = true
		case !inTarget && character == '"':
			inQuote = !inQuote
		case !inQuote && character == '<':
			if inTarget {
				return nil, ErrNavigationConflict
			}
			inTarget = true
		case !inQuote && character == '>':
			if !inTarget {
				return nil, ErrNavigationConflict
			}
			inTarget = false
		case !inTarget && !inQuote && character == ',':
			part := strings.TrimSpace(value[start:index])
			if part == "" {
				return nil, ErrNavigationConflict
			}
			parts = append(parts, part)
			start = index + 1
		}
	}
	if escaped || inTarget || inQuote {
		return nil, ErrNavigationConflict
	}
	part := strings.TrimSpace(value[start:])
	if part == "" {
		return nil, ErrNavigationConflict
	}
	return append(parts, part), nil
}

func parseRFC8288Link(value string) (rfc8288Link, error) {
	if !strings.HasPrefix(value, "<") {
		return rfc8288Link{}, ErrNavigationConflict
	}
	end := strings.IndexByte(value, '>')
	if end <= 1 {
		return rfc8288Link{}, ErrNavigationConflict
	}
	link := rfc8288Link{target: value[1:end]}
	parameters, err := splitLinkParameters(value[end+1:])
	if err != nil {
		return rfc8288Link{}, err
	}
	seenRel := false
	seenAnchor := false
	for _, parameter := range parameters {
		name, raw, found := strings.Cut(parameter, "=")
		name = strings.TrimSpace(name)
		raw = strings.TrimSpace(raw)
		if !validHTTPToken(name) {
			return rfc8288Link{}, ErrNavigationConflict
		}
		switch {
		case strings.EqualFold(name, "rel"):
			if !found || seenRel {
				return rfc8288Link{}, ErrNavigationConflict
			}
			decoded, err := decodeLinkParameter(raw)
			if err != nil {
				return rfc8288Link{}, err
			}
			seenRel = true
			link.rels = strings.Fields(decoded)
			if len(link.rels) == 0 {
				return rfc8288Link{}, ErrNavigationConflict
			}
		case strings.EqualFold(name, "anchor"):
			if !found || seenAnchor {
				return rfc8288Link{}, ErrNavigationConflict
			}
			if _, err := decodeLinkParameter(raw); err != nil {
				return rfc8288Link{}, err
			}
			seenAnchor = true
			link.anchor = true
		case found:
			if _, err := decodeLinkParameter(raw); err != nil {
				return rfc8288Link{}, err
			}
		}
	}
	return link, nil
}

func splitLinkParameters(value string) ([]string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return []string{}, nil
	}
	if value[0] != ';' {
		return nil, ErrNavigationConflict
	}
	parameters := make([]string, 0, 1)
	start := 1
	inQuote := false
	escaped := false
	for index := 1; index < len(value); index++ {
		character := value[index]
		switch {
		case escaped:
			escaped = false
		case inQuote && character == '\\':
			escaped = true
		case character == '"':
			inQuote = !inQuote
		case !inQuote && character == ';':
			parameter := strings.TrimSpace(value[start:index])
			if parameter == "" {
				return nil, ErrNavigationConflict
			}
			parameters = append(parameters, parameter)
			start = index + 1
		}
	}
	if escaped || inQuote {
		return nil, ErrNavigationConflict
	}
	parameter := strings.TrimSpace(value[start:])
	if parameter == "" {
		return nil, ErrNavigationConflict
	}
	return append(parameters, parameter), nil
}

func decodeLinkParameter(value string) (string, error) {
	if value == "" {
		return "", ErrNavigationConflict
	}
	if value[0] != '"' {
		if !validHTTPToken(value) {
			return "", ErrNavigationConflict
		}
		return value, nil
	}
	if len(value) < 2 || value[len(value)-1] != '"' {
		return "", ErrNavigationConflict
	}
	decoded := make([]byte, 0, len(value)-2)
	for index := 1; index < len(value)-1; index++ {
		character := value[index]
		if character == '\\' {
			index++
			if index >= len(value)-1 {
				return "", ErrNavigationConflict
			}
			character = value[index]
		}
		if character < 0x20 || character == 0x7f {
			return "", ErrNavigationConflict
		}
		decoded = append(decoded, character)
	}
	return string(decoded), nil
}

func linkHasRelation(link rfc8288Link, relation string) bool {
	for _, candidate := range link.rels {
		if strings.EqualFold(candidate, relation) {
			return true
		}
	}
	return false
}

func applyRecordControls(
	records []contract.PresentValue,
	mediaType string,
	controls contract.ResultControls,
	cap *uint64,
) ([]contract.PresentValue, []contract.PresentValue, error) {
	emitted := make([]contract.PresentValue, 0, len(records))
	for index, record := range records {
		if cap != nil && uint64(len(emitted)) >= *cap {
			return emitted, clonePresentValues(records[index:]), nil
		}
		one := contract.ActionResult{MediaType: mediaType, Data: contract.NewArrayValue([]contract.PresentValue{record})}
		controlled, err := contract.ApplyResultControls(one, controls)
		if err != nil {
			return nil, nil, err
		}
		values, ok := controlled.Data.Elements()
		if !ok || len(values) > 1 {
			return nil, nil, ErrInvalidProviderValue
		}
		emitted = append(emitted, values...)
	}
	return emitted, []contract.PresentValue{}, nil
}

func responseCarriesPaginatedRecords(operation contract.Operation, decoded DecodedResponse) bool {
	if decoded.Result == nil {
		return false
	}
	exactStatus := strconv.Itoa(decoded.StatusCode)
	hasExact := false
	for _, response := range operation.Responses {
		if response.Status != exactStatus {
			continue
		}
		hasExact = true
		if responseCarriesRecords(response, decoded.Result.MediaType) {
			return true
		}
	}
	if hasExact {
		return false
	}
	for _, response := range operation.Responses {
		if response.Status == "default" && responseCarriesRecords(response, decoded.Result.MediaType) {
			return true
		}
	}
	return false
}

func responseCarriesRecords(response contract.Response, mediaType string) bool {
	return response.MediaType == mediaType && response.CarriesPaginatedRecords()
}

func navigationUint64(input contract.PresentValue, fieldID string) (uint64, bool, error) {
	fields, _ := input.Fields()
	value, present := fields[fieldID]
	if !present || value.Presence() == contract.PresenceAbsent {
		return 0, false, nil
	}
	number, ok := value.ExactNumber()
	if !ok || value.Kind() != contract.ValueKindInteger {
		return 0, false, ErrInvalidPaginationFact
	}
	parsed, err := strconv.ParseUint(number.Text, 10, 64)
	if err != nil {
		return 0, false, ErrInvalidPaginationFact
	}
	return parsed, true, nil
}

func sameOriginURL(left, right *url.URL) bool {
	if left == nil || right == nil {
		return false
	}
	leftOrigin, err := canonicalOrigin(left.Scheme + "://" + left.Host)
	if err != nil {
		return false
	}
	rightOrigin, err := canonicalOrigin(right.Scheme + "://" + right.Host)
	return err == nil && sameOrigin(leftOrigin, rightOrigin)
}

func findOperationBinding(operation contract.Operation, fieldID string) (contract.FieldBinding, bool) {
	for _, binding := range operation.Inputs {
		if binding.FieldID == fieldID {
			return binding, true
		}
	}
	for _, variant := range operation.RequestVariants {
		for _, binding := range variant.Inputs {
			if binding.FieldID == fieldID {
				return binding, true
			}
		}
	}
	return contract.FieldBinding{}, false
}

func operationDeclaresFieldID(operation contract.Operation, fieldID string) bool {
	for _, binding := range operation.Inputs {
		if binding.FieldID == fieldID {
			return true
		}
	}
	for _, variant := range operation.RequestVariants {
		for _, binding := range variant.Inputs {
			if binding.FieldID == fieldID {
				return true
			}
		}
	}
	return false
}

func cloneUint64(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	return new(*value)
}

func clonePresentValues(values []contract.PresentValue) []contract.PresentValue {
	if values == nil {
		return nil
	}
	cloned := make([]contract.PresentValue, len(values))
	copy(cloned, values)
	return cloned
}

func clonePresentValue(value *contract.PresentValue) *contract.PresentValue {
	if value == nil {
		return nil
	}
	return new(*value)
}
