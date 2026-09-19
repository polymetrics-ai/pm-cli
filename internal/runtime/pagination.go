package runtime

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
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
	SourceExhausted    bool                             `json:"source_exhausted"`
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
		return nextLinkPaginationState(operation, pagination, input, httpContext)
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
	httpContext pageHTTPContext,
) (*contract.ExactNumber, ContinuationState, bool, *uint64, error) {
	state := ContinuationState{UnreadTail: []contract.PresentValue{}, NextInputs: map[string]contract.PresentValue{}}
	if pagination == nil || httpContext.RequestURL == nil || httpContext.ResponseHeader == nil {
		return nil, state, false, nil, ErrInvalidPaginationFact
	}
	values := httpContext.ResponseHeader.Values(pagination.LinkResponseHeader)
	if len(values) == 0 {
		return nil, state, true, nil, nil
	}
	links, err := parseRFC8288Links(values)
	if err != nil {
		return nil, state, false, nil, fmt.Errorf("%w: Link response is malformed", ErrNavigationConflict)
	}
	var next *rfc8288Link
	for index := range links {
		if !linkHasRelation(links[index], "next") {
			continue
		}
		if next != nil || links[index].anchor {
			return nil, state, false, nil, fmt.Errorf("%w: Link next relation is ambiguous", ErrNavigationConflict)
		}
		next = &links[index]
	}
	if next == nil {
		return nil, state, true, nil, nil
	}
	value, declaredLimit, err := linkNavigationValue(
		operation,
		pagination,
		pagination.LinkRequestField,
		next.target,
		input,
		httpContext.RequestURL,
		httpContext.Authorization,
	)
	if err != nil {
		return nil, state, false, nil, err
	}
	state.NextInputs[pagination.LinkRequestField] = value
	return nil, state, false, declaredLimit, nil
}

func linkNavigationValue(
	operation contract.Operation,
	pagination *contract.OperationPagination,
	fieldID string,
	rawTarget string,
	input contract.PresentValue,
	requestURL *url.URL,
	authorization *contract.OperationAuthorization,
) (contract.PresentValue, *uint64, error) {
	binding, found := findOperationBinding(operation, fieldID)
	if !found || binding.Location != contract.FieldLocationQuery ||
		binding.Sensitivity != contract.SensitivityPublic ||
		(binding.Schema.Type != contract.SchemaTypeString && binding.Schema.Type != contract.SchemaTypeInteger) {
		return contract.PresentValue{}, nil, ErrInvalidPaginationFact
	}
	if rawTarget == "" || strings.Contains(rawTarget, "#") {
		return contract.PresentValue{}, nil, fmt.Errorf("%w: Link next target has a fragment or is empty", ErrNavigationConflict)
	}
	reference, err := url.Parse(rawTarget)
	if err != nil || reference.User != nil || reference.Opaque != "" || reference.Fragment != "" {
		return contract.PresentValue{}, nil, fmt.Errorf("%w: Link next target is invalid", ErrNavigationConflict)
	}
	target := requestURL.ResolveReference(reference)
	if !sameOriginURL(target, requestURL) || target.EscapedPath() != requestURL.EscapedPath() {
		return contract.PresentValue{}, nil, fmt.Errorf("%w: Link next target changes origin or path", ErrNavigationConflict)
	}
	currentQuery, err := url.ParseQuery(requestURL.RawQuery)
	if err != nil {
		return contract.PresentValue{}, nil, fmt.Errorf("%w: sent query is malformed", ErrNavigationConflict)
	}
	nextQuery, err := url.ParseQuery(target.RawQuery)
	if err != nil {
		return contract.PresentValue{}, nil, fmt.Errorf("%w: Link next query is malformed", ErrNavigationConflict)
	}
	navigation, present := nextQuery[binding.WireName]
	if !present || len(navigation) != 1 {
		return contract.PresentValue{}, nil, fmt.Errorf("%w: Link next navigation is absent or duplicated", ErrNavigationConflict)
	}
	if current := currentQuery[binding.WireName]; len(current) > 1 {
		return contract.PresentValue{}, nil, fmt.Errorf("%w: sent navigation is duplicated", ErrNavigationConflict)
	}
	delete(currentQuery, binding.WireName)
	delete(nextQuery, binding.WireName)
	if authorization != nil && authorization.Placement == contract.FieldLocationQuery {
		if strings.EqualFold(authorization.WireName, binding.WireName) {
			return contract.PresentValue{}, nil, ErrInvalidPaginationFact
		}
		if _, embedded := nextQuery[authorization.WireName]; embedded {
			return contract.PresentValue{}, nil, fmt.Errorf("%w: Link next target embeds a credential channel", ErrNavigationConflict)
		}
		delete(currentQuery, authorization.WireName)
	}
	// A provider may clamp the page size it was sent. The target then names a
	// positive integer strictly smaller than the size actually sent for the
	// declared page-size field, and that is the only admitted query difference:
	// the declared field is compared separately here, so every other parameter
	// still has to match the sent query exactly.
	var declaredLimit *uint64
	if limitField := PaginationLimitField(pagination); limitField != "" {
		limitBinding, declared := findOperationBinding(operation, limitField)
		if !declared || limitBinding.Location != contract.FieldLocationQuery || limitBinding.WireName == "" {
			return contract.PresentValue{}, nil, ErrInvalidPaginationFact
		}
		declaredLimit, err = declaredPageSize(
			currentQuery[limitBinding.WireName],
			nextQuery[limitBinding.WireName],
		)
		if err != nil {
			return contract.PresentValue{}, nil, err
		}
		delete(currentQuery, limitBinding.WireName)
		delete(nextQuery, limitBinding.WireName)
	}
	if currentQuery.Encode() != nextQuery.Encode() {
		return contract.PresentValue{}, nil, fmt.Errorf("%w: Link next target changes unrelated query authority", ErrNavigationConflict)
	}
	var value contract.PresentValue
	switch binding.Schema.Type {
	case contract.SchemaTypeString:
		value = contract.NewStringValue(navigation[0])
	case contract.SchemaTypeInteger:
		value, err = contract.NewIntegerValue(navigation[0])
		if err != nil {
			return contract.PresentValue{}, nil, fmt.Errorf("%w: Link next integer is invalid", ErrNavigationConflict)
		}
	default:
		return contract.PresentValue{}, nil, ErrInvalidPaginationFact
	}
	pairs, err := encodeParameter(binding, value)
	if err != nil || len(pairs) != 1 {
		return contract.PresentValue{}, nil, fmt.Errorf("%w: Link next navigation cannot be encoded as one query value", ErrNavigationConflict)
	}
	pairs[0].allowReserved = binding.AllowReserved
	futureQuery, err := url.ParseQuery(encodeQueryPairs(pairs))
	futureNavigation := futureQuery[binding.WireName]
	if err != nil || len(futureQuery) != 1 || len(futureNavigation) != 1 ||
		futureNavigation[0] != navigation[0] {
		return contract.PresentValue{}, nil, fmt.Errorf("%w: Link next navigation changes authority when re-encoded", ErrNavigationConflict)
	}
	fields, _ := input.Fields()
	if current, supplied := fields[fieldID]; supplied && current.Presence() != contract.PresenceAbsent && current.Equal(value) {
		return contract.PresentValue{}, nil, fmt.Errorf("%w: Link next navigation makes no progress", ErrNavigationConflict)
	}
	return value, declaredLimit, nil
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
