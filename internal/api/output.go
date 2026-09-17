package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
)

// OutputFormat is the complete set of presentation formats owned by the thin
// client. Provider data is never reformatted into a lossy provider-specific
// view.
type OutputFormat string

const (
	OutputText OutputFormat = "text"
	OutputJSON OutputFormat = "json"
)

func ParseOutputFormat(value string) (OutputFormat, error) {
	switch OutputFormat(value) {
	case OutputText, OutputJSON:
		return OutputFormat(value), nil
	default:
		return "", errors.New("output must be text or json")
	}
}

// WireResponse is implemented by every generated response wrapper. It keeps
// authored client behavior on top of the generated transport rather than
// introducing a second HTTP client.
type WireResponse interface {
	GetBody() []byte
	StatusCode() int
	ContentType() string
}

// Envelope is one bounded, validated generated-client response. A successful
// body remains exact JSON so additive public fields survive client rendering.
type Envelope struct {
	Status  int
	Body    json.RawMessage
	Problem *DecodedProblem
}

// DecodeEnvelope applies the shared problem decoder to every non-success
// response and checks successful JSON for size, duplicate fields and trailing
// values. Endpoint-specific generated DTOs continue to own semantic decoding.
func DecodeEnvelope(response WireResponse) (Envelope, error) {
	if response == nil {
		return Envelope{}, ErrMalformedEnvelope
	}
	status := response.StatusCode()
	body := response.GetBody()
	if status < 200 || status >= 300 {
		problem, err := DecodeProblem(status, response.ContentType(), body)
		if err != nil {
			return Envelope{}, err
		}
		return Envelope{Status: status, Problem: &problem}, nil
	}
	if len(body) == 0 {
		if status == http.StatusNoContent {
			return Envelope{Status: status}, nil
		}
		return Envelope{}, ErrMalformedEnvelope
	}
	mediaType, _, err := mime.ParseMediaType(response.ContentType())
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return Envelope{}, ErrMalformedEnvelope
	}
	if len(body) > maxPublicEnvelopeBytes || rejectDuplicateJSONFields(body) != nil {
		return Envelope{}, ErrMalformedEnvelope
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil || requireJSONDecoderEOF(decoder) != nil {
		return Envelope{}, ErrMalformedEnvelope
	}
	return Envelope{Status: status, Body: append(json.RawMessage(nil), body...)}, nil
}

// ProblemError is safe for user-facing rendering. It contains only the
// protocol-qualified RFC 9457 envelope and never an underlying transport or
// parser error.
type ProblemError struct {
	Decoded DecodedProblem
}

func (err *ProblemError) Error() string {
	if err == nil {
		return "PM request failed"
	}
	problem := err.Decoded.Value
	return fmt.Sprintf("%s: %s (incident %s)", problem.Code, problem.Detail, problem.IncidentId)
}

func (err *ProblemError) ExitCode() int {
	if err == nil {
		return 6
	}
	return ExitCodeForProblem(err.Decoded)
}

func ExitCodeForProblem(problem DecodedProblem) int {
	if problem.Disposition == ProblemOutcomeUnknown {
		return 7
	}
	switch problem.Value.Status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return 2
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return 3
	case http.StatusConflict, http.StatusPreconditionFailed, http.StatusPreconditionRequired:
		return 4
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return 5
	default:
		return 6
	}
}

func EnvelopeError(envelope Envelope) error {
	if envelope.Problem == nil {
		return nil
	}
	return &ProblemError{Decoded: *envelope.Problem}
}
