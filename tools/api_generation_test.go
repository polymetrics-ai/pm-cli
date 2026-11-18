package tools

import (
	"strings"
	"testing"
)

// strictProjectionSource is a reduced oapi-codegen strict server source with
// two operations. SelectThing answers 200 with a declared response header and
// a oneOf body, so its strict response is a local struct with Body and Headers
// members, and its visitor encodes the unexported union payload of the body.
// UpdateThing answers 200 with a oneOf body and no header, so its strict
// response is an alias of the body DTO.
const strictProjectionSource = `package httpapi

type StrictServerInterface interface {
	SelectThing(ctx context.Context, request SelectThingRequestObject) (SelectThingResponseObject, error)
	UpdateThing(ctx context.Context, request UpdateThingRequestObject) (UpdateThingResponseObject, error)
}

type strictHandler struct {
	ssi StrictServerInterface
}

func (sh *strictHandler) SelectThing(ctx *gin.Context) {
	response, err := sh.ssi.SelectThing(ctx, request)
	_, _ = response, err
}

func (sh *strictHandler) UpdateThing(ctx *gin.Context) {
	response, err := sh.ssi.UpdateThing(ctx, request)
	_, _ = response, err
}

type SelectThing200ResponseHeaders struct {
	ETag *string
}

type SelectThing200JSONResponse struct {
	Body    SelectThing200JSONResponseBody
	Headers SelectThing200ResponseHeaders
}

func (response SelectThing200JSONResponse) VisitSelectThingResponse(w http.ResponseWriter) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(response.Body.union); err != nil {
		return err
	}
	if response.Headers.ETag != nil {
		w.Header().Set("ETag", fmt.Sprint(*response.Headers.ETag))
	}
	return nil
}

type UpdateThing200JSONResponse = UpdateThing200JSONResponseBody

func (response UpdateThing200JSONResponse) VisitUpdateThingResponse(w http.ResponseWriter) error {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(response.union); err != nil {
		return err
	}
	return nil
}
`

// Check: a strict response with a declared header and a oneOf body encodes its
// body through the canonical DTO, and the projection of a response with no
// header does not change. Defect it catches: a generated visitor that refers
// to the unexported union member of a DTO in internal/api, so internal/httpapi
// does not build (the ETag trial of #285 slice 285c-1).
func TestStrictProjectionEncodesAHeaderedUnionBodyThroughItsDTO(t *testing.T) {
	projected, err := projectServerBoundary(strictProjectionSource)
	if err != nil {
		t.Fatalf("projectServerBoundary: %v", err)
	}
	output := string(projected)
	for _, want := range []string{
		"json.NewEncoder(&buf).Encode(response.Body)",
		"type UpdateThing200JSONResponse UpdateThing200JSONResponseBody",
		"sh.ssi.SelectThing(ctx.Request.Context(), request)",
		"sh.ssi.UpdateThing(ctx.Request.Context(), request)",
		`w.Header().Set("ETag", fmt.Sprint(*response.Headers.ETag))`,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("projected source does not contain %q", want)
		}
	}
	// The alias projection inserts a call that has no source position, so the
	// printer can break the line before its closing parenthesis. Compare the
	// call without white space and without that trailing comma.
	compact := strings.ReplaceAll(strings.Join(strings.Fields(output), ""), ",)", ")")
	if !strings.Contains(compact, "json.NewEncoder(&buf).Encode(UpdateThing200JSONResponseBody(response))") {
		t.Errorf("projected source does not encode the alias response through its DTO:\n%s", output)
	}
	if strings.Contains(output, ".union") {
		t.Errorf("projected source still refers to an unexported union member:\n%s", output)
	}
}

// Check: a headered visitor that refers to a union payload in any other form
// is refused, not left in the output. Defect it catches: a projection that
// passes an unsupported generated shape through to a source that does not
// build.
func TestStrictProjectionRefusesAnUnsupportedHeaderedUnionPayload(t *testing.T) {
	source := strings.Replace(strictProjectionSource,
		"Encode(response.Body.union)", "Encode(response.Headers.union)", 1)
	if source == strictProjectionSource {
		t.Fatal("the unsupported shape was not built")
	}
	_, err := projectServerBoundary(source)
	if err == nil || !strings.Contains(err.Error(), "SelectThing200JSONResponse has an unsupported canonical payload projection") {
		t.Fatalf("projectServerBoundary of an unsupported payload = %v; want the unsupported projection refusal", err)
	}
}
