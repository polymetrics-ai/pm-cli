package runtime

import (
	"context"
	"net/http"
	"testing"

	"github.com/polymetrics-ai/pm-cli/internal/contract"
)

func TestRecordLimitIsNotPageSize(t *testing.T) {
	t.Run("Gong POST has no invented limit wire field", func(t *testing.T) {
		operation := testBodyOperation("gong-post-read")
		operation.Transport.Path = "/v2/gong/search"
		input := contract.NewObjectValue(map[string]contract.PresentValue{
			"body": contract.NewObjectValue(map[string]contract.PresentValue{"enabled": contract.NewBooleanValue(false)}),
		})
		encoded, err := EncodeInputs(operation, input)
		if err != nil {
			t.Fatalf("EncodeInputs: %v", err)
		}
		if encoded.RawQuery != "" {
			t.Fatalf("invented query = %q", encoded.RawQuery)
		}
		if string(encoded.Body) != `{"enabled":false}` {
			t.Fatalf("body = %s", encoded.Body)
		}
		for _, binding := range operation.Inputs {
			if binding.WireName == "limit" || binding.FieldID == "limit" {
				t.Fatalf("invented limit binding = %#v", binding)
			}
		}
	})

	t.Run("local truncation retains unread rows behind server continuation", func(t *testing.T) {
		codec := newMemoryContinuationCodec()
		binding := ContinuationBinding{
			Scope:        contract.Scope{OrgID: "org-fixture", WorkspaceID: "workspace-fixture"},
			ConnectionID: "connection-fixture", OperationID: "gong-post-read",
			OperationRevision: "operation-revision-fixture", RunID: "run-fixture",
		}
		records := []contract.PresentValue{
			testPageRecord(t, "1", true), testPageRecord(t, "2", true),
			testPageRecord(t, "3", true), testPageRecord(t, "4", true),
		}
		decoded := DecodedResponse{
			StatusCode: 200,
			Header:     make(http.Header),
			Document:   contract.NewArrayValue(records),
			Result:     &contract.ActionResult{MediaType: "application/json", Data: contract.NewArrayValue(records)},
		}
		cap := uint64(2)
		operation := testBodyOperation("gong-post-read")
		first, err := applyPage(
			context.Background(), operation, decoded, nil, contract.NewObjectValue(nil),
			CallLimits{TotalRecordCap: &cap}, contract.ResultControls{}, codec, binding, pageHTTPContext{},
		)
		if err != nil {
			t.Fatalf("first page: %v", err)
		}
		if first.Pagination == nil || first.Pagination.EffectiveWireLimit != nil || first.Pagination.TotalRecordCap == nil ||
			*first.Pagination.TotalRecordCap != 2 || first.Pagination.SourceExhausted || len(first.Pagination.UnreadTail) != 2 {
			t.Fatalf("first pagination = %#v", first.Pagination)
		}
		if got := pageRecordIDs(first.Pagination.UnreadTail); got != "3,4" {
			t.Fatalf("unread IDs = %s", got)
		}
		prepared, err := preparePagination(
			context.Background(), operation, nil, true,
			contract.NewObjectValue(nil), first.Pagination.Continuation, codec, binding,
		)
		if err != nil {
			t.Fatalf("prepare continuation: %v", err)
		}
		if prepared.state == nil || len(prepared.state.UnreadTail) != 2 {
			t.Fatalf("prepared continuation = %#v", prepared.state)
		}
		second, err := serveContinuationTail(context.Background(), *prepared.state, CallLimits{TotalRecordCap: &cap}, contract.ResultControls{}, codec, binding)
		if err != nil {
			t.Fatalf("serve tail: %v", err)
		}
		if got, _ := second.Data.Elements(); pageRecordIDs(got) != "3,4" {
			t.Fatalf("resumed rows = %s", pageRecordIDs(got))
		}
		if !second.Pagination.SourceExhausted || second.Pagination.Continuation.Presence() != contract.PresenceAbsent || len(second.Pagination.UnreadTail) != 0 {
			t.Fatalf("resumed pagination = %#v", second.Pagination)
		}
	})

	t.Run("selected POST body cursor survives filtering without an invented limit", func(t *testing.T) {
		operation := testListOperation("body-cursor-list", http.MethodPost, "/v2/search")
		bodySchema := contract.Schema{
			Type:     contract.SchemaTypeObject,
			Required: []string{"enabled"},
			Properties: map[string]contract.Schema{
				"cursor":  {Type: contract.SchemaTypeString},
				"enabled": {Type: contract.SchemaTypeBool},
			},
		}
		operation.RequestVariants = []contract.RequestVariant{{
			VariantID: "json-search", MediaType: "application/json", Required: true, Schema: bodySchema,
			Inputs: []contract.FieldBinding{
				{
					FieldID: "cursor", Location: contract.FieldLocationBody, JSONPointer: "/cursor",
					Required: false, Nullable: false, Style: "json", Explode: false,
					Sensitivity: contract.SensitivityPublic, Schema: contract.Schema{Type: contract.SchemaTypeString},
				},
				{
					FieldID: "enabled", Location: contract.FieldLocationBody, JSONPointer: "/enabled",
					Required: true, Nullable: false, Style: "json", Explode: false,
					Sensitivity: contract.SensitivityPublic, Schema: contract.Schema{Type: contract.SchemaTypeBool},
				},
			},
			SourceRefs: testSourceRefs("#/requestBody"),
		}}
		recordsSchema := *operation.Responses[0].Schema
		operation.Responses[0].Schema = &contract.Schema{
			Type: contract.SchemaTypeObject,
			Properties: map[string]contract.Schema{
				"records": recordsSchema,
				"next":    {Type: contract.SchemaTypeString},
			},
		}
		recordPointer := "/records"
		operation.Responses[0].RecordPointer = &recordPointer
		operation.Execution = &contract.OperationExecution{Pagination: &contract.OperationPagination{
			Kind:                  contract.OperationPaginationCursor,
			CursorRequestField:    "cursor",
			CursorResponsePointer: "/next",
			Witness:               testExecutionWitness(),
		}}
		pagination := operation.Execution.Pagination
		if err := pagination.Validate(operation); err != nil {
			t.Fatalf("pagination fact: %v", err)
		}
		codec := newMemoryContinuationCodec()
		binding := ContinuationBinding{
			Scope:        contract.Scope{OrgID: "org-fixture", WorkspaceID: "workspace-fixture"},
			ConnectionID: "connection-fixture", OperationID: operation.OperationID,
			OperationRevision: "operation-revision-fixture", RunID: "run-fixture",
		}
		records := []contract.PresentValue{testPageRecord(t, "1", false), testPageRecord(t, "2", false)}
		decoded := DecodedResponse{
			StatusCode: 200, Header: make(http.Header),
			Document: contract.NewObjectValue(map[string]contract.PresentValue{
				"records": contract.NewArrayValue(records), "next": contract.NewStringValue("cursor-2"),
			}),
			Result: &contract.ActionResult{MediaType: "application/json", Data: contract.NewArrayValue(records)},
		}
		input := contract.NewObjectValue(map[string]contract.PresentValue{"enabled": contract.NewBooleanValue(false)})
		match := contract.NewBooleanValue(true)
		result, err := applyPage(
			context.Background(), operation, decoded, pagination, input,
			CallLimits{}, contract.ResultControls{Filter: &contract.RecordFilter{
				Path: contract.FieldPath{"keep"}, Operator: contract.FilterEqual, Value: &match,
			}}, codec, binding, pageHTTPContext{},
		)
		if err != nil {
			t.Fatalf("applyPage: %v", err)
		}
		if result.Pagination.EmittedCount != 0 || result.Pagination.SourceExhausted ||
			result.Pagination.EffectiveWireLimit != nil ||
			result.Pagination.Continuation.Presence() != contract.PresenceValue {
			t.Fatalf("filtered pagination = %#v", result.Pagination)
		}
		prepared, err := preparePagination(
			context.Background(), operation, pagination, false,
			input, result.Pagination.Continuation, codec, binding,
		)
		if err != nil {
			t.Fatalf("prepare body cursor: %v", err)
		}
		encoded, err := EncodeInputs(operation, prepared.input)
		if err != nil {
			t.Fatalf("encode body cursor: %v", err)
		}
		if encoded.RawQuery != "" || string(encoded.Body) != `{"cursor":"cursor-2","enabled":false}` {
			t.Fatalf("body cursor wire query=%q body=%s", encoded.RawQuery, encoded.Body)
		}
		state := codec.mustState(t, binding, result.Pagination.Continuation)
		cursor, ok := state.NextInputs["cursor"].TextValue()
		if !ok || cursor != "cursor-2" {
			t.Fatalf("retained cursor = %#v", state.NextInputs)
		}
	})
}
