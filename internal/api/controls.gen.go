// Package api contains code generated from api/controls.json; DO NOT EDIT.
package api

import "strings"

const WireVersion = "pm.api/v1"
const ProtocolMajor = 1

type ProblemDisposition string

const (
	ProblemConfirmedRefusal       ProblemDisposition = "confirmed_refusal"
	ProblemOutcomeUnknown         ProblemDisposition = "outcome_unknown"
	ProblemResolvedReceiptExpired ProblemDisposition = "resolved_receipt_expired"
)

type AdmissionProblemRule struct {
	Status          int
	Code            string
	SafeRetryAction string
	Disposition     ProblemDisposition
}

var AdmissionProblemRules = [...]AdmissionProblemRule{
	{Status: 400, Code: "invalid_request", SafeRetryAction: "correct_input", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 401, Code: "authentication_required", SafeRetryAction: "refresh_authorization", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 403, Code: "grant_unavailable", SafeRetryAction: "refresh_authorization", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 404, Code: "resource_not_found", SafeRetryAction: "correct_scope", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 409, Code: "foundation_incompatible", SafeRetryAction: "after_capability_change", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 409, Code: "idempotency_conflict", SafeRetryAction: "new_key_for_new_intent", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 409, Code: "resume_unavailable", SafeRetryAction: "refresh_run_state", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 409, Code: "specification_stale", SafeRetryAction: "refresh_specification", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 410, Code: "retry_receipt_expired", SafeRetryAction: "inspect_existing_result", Disposition: ProblemDisposition("resolved_receipt_expired")},
	{Status: 412, Code: "control_revision_mismatch", SafeRetryAction: "refresh_control_revision", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 428, Code: "control_revision_required", SafeRetryAction: "refresh_control_revision", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 429, Code: "workspace_admission_age_limit", SafeRetryAction: "after_backlog_progress", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 429, Code: "workspace_admission_count_limit", SafeRetryAction: "after_backlog_progress", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 503, Code: "admission_outcome_unknown", SafeRetryAction: "same_key_reconcile_only", Disposition: ProblemDisposition("outcome_unknown")},
	{Status: 503, Code: "capability_unavailable", SafeRetryAction: "after_capability_change", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 503, Code: "control_store_unavailable", SafeRetryAction: "same_key_after_dependency_recovery", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 503, Code: "deployment_admission_overload", SafeRetryAction: "after_backlog_progress", Disposition: ProblemDisposition("confirmed_refusal")},
}

func AdmissionProblemRuleFor(status int, code string) (AdmissionProblemRule, bool) {
	for _, rule := range AdmissionProblemRules {
		if rule.Status == status && rule.Code == code {
			return rule, true
		}
	}
	return AdmissionProblemRule{}, false
}

var SupportedFeatures = [...]string{"exact-values", "metadata-only-replay", "typed-result-controls", "wire-problem-v1"}

var ProtocolMetadataRequiredFields = [...]string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}

var ProtocolMetadataOptionalFields = [...]string{}

var ForbiddenPublicFields = [...]string{"access_token", "api_key", "client_secret", "credential_value", "password", "pm_automation_credential", "provider_credential", "provider_secret", "refresh_token", "secret"}

var RunStates = [...]string{"accepted", "awaiting_approval", "blocked", "cancelled", "failed", "partial", "queued", "reconciling", "running", "succeeded"}

var TerminalRunStates = [...]string{"cancelled", "failed", "partial", "succeeded"}

type OperationControl struct {
	OperationID            string
	Method                 string
	Path                   string
	RequiredBodyFields     []string
	OptionalBodyFields     []string
	RequiresIdempotencyKey bool
	RequiresIfMatch        bool
}

var operationControls = map[string]OperationControl{
	"createDelegation":     {OperationID: "createDelegation", Method: "POST", Path: "/v1/workspaces/{workspace_id}/delegations", RequiredBodyFields: []string{"delegation", "protocol"}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: true, RequiresIfMatch: true},
	"createServiceAccount": {OperationID: "createServiceAccount", Method: "POST", Path: "/v1/workspaces/{workspace_id}/service-accounts", RequiredBodyFields: []string{"protocol", "service_account"}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: true, RequiresIfMatch: true},
	"grantMembership":      {OperationID: "grantMembership", Method: "POST", Path: "/v1/workspaces/{workspace_id}/memberships", RequiredBodyFields: []string{"membership", "protocol"}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: true, RequiresIfMatch: true},
	"revokeDelegation":     {OperationID: "revokeDelegation", Method: "POST", Path: "/v1/workspaces/{workspace_id}/delegations/{id}/revoke", RequiredBodyFields: []string{"delegation", "protocol"}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: true, RequiresIfMatch: true},
	"revokeMembership":     {OperationID: "revokeMembership", Method: "POST", Path: "/v1/workspaces/{workspace_id}/memberships/{id}/revoke", RequiredBodyFields: []string{"membership", "protocol"}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: true, RequiresIfMatch: true},
	"revokeServiceAccount": {OperationID: "revokeServiceAccount", Method: "POST", Path: "/v1/workspaces/{workspace_id}/service-accounts/{id}/revoke", RequiredBodyFields: []string{"protocol", "service_account"}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: true, RequiresIfMatch: true},
	"submitInvocation":     {OperationID: "submitInvocation", Method: "POST", Path: "/v1/workspaces/{workspace_id}/invocations", RequiredBodyFields: []string{"connection_ref", "input", "lane", "operation_ref", "protocol", "result_controls"}, OptionalBodyFields: []string{"approval_ref"}, RequiresIdempotencyKey: true, RequiresIfMatch: false},
}

var operationIDsByRoute = map[string]string{
	"GET /v1/info": "getServerInfo",
	"POST /v1/workspaces/{workspace_id}/delegations":                  "createDelegation",
	"POST /v1/workspaces/:workspace_id/delegations":                   "createDelegation",
	"POST /v1/workspaces/{workspace_id}/delegations/{id}/revoke":      "revokeDelegation",
	"POST /v1/workspaces/:workspace_id/delegations/:id/revoke":        "revokeDelegation",
	"POST /v1/workspaces/{workspace_id}/invocations":                  "submitInvocation",
	"POST /v1/workspaces/:workspace_id/invocations":                   "submitInvocation",
	"POST /v1/workspaces/{workspace_id}/memberships":                  "grantMembership",
	"POST /v1/workspaces/:workspace_id/memberships":                   "grantMembership",
	"POST /v1/workspaces/{workspace_id}/memberships/{id}/revoke":      "revokeMembership",
	"POST /v1/workspaces/:workspace_id/memberships/:id/revoke":        "revokeMembership",
	"POST /v1/workspaces/{workspace_id}/service-accounts":             "createServiceAccount",
	"POST /v1/workspaces/:workspace_id/service-accounts":              "createServiceAccount",
	"POST /v1/workspaces/{workspace_id}/service-accounts/{id}/revoke": "revokeServiceAccount",
	"POST /v1/workspaces/:workspace_id/service-accounts/:id/revoke":   "revokeServiceAccount",
}

type JSONFieldControl struct {
	AllowedFields  []string
	RequiredFields []string
}

var operationObjectControls = map[string]map[string]JSONFieldControl{
	"createDelegation": {
		"":            {AllowedFields: []string{"delegation", "protocol"}, RequiredFields: []string{"delegation", "protocol"}},
		"/delegation": {AllowedFields: []string{"connection_scopes", "effective_actor_ref", "expires_at", "operation_scopes", "requested_actor_ref", "workspace_scopes"}, RequiredFields: []string{"connection_scopes", "effective_actor_ref", "expires_at", "operation_scopes", "requested_actor_ref", "workspace_scopes"}},
		"/protocol":   {AllowedFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, RequiredFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}},
	},
	"createServiceAccount": {
		"":                 {AllowedFields: []string{"protocol", "service_account"}, RequiredFields: []string{"protocol", "service_account"}},
		"/protocol":        {AllowedFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, RequiredFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}},
		"/service_account": {AllowedFields: []string{"connection_scopes", "display_name", "expires_at", "operation_scopes", "workspace_scopes"}, RequiredFields: []string{"connection_scopes", "display_name", "expires_at", "operation_scopes", "workspace_scopes"}},
	},
	"grantMembership": {
		"":            {AllowedFields: []string{"membership", "protocol"}, RequiredFields: []string{"membership", "protocol"}},
		"/membership": {AllowedFields: []string{"expires_at", "principal_ref", "resource_grants", "role"}, RequiredFields: []string{"expires_at", "principal_ref", "resource_grants", "role"}},
		"/protocol":   {AllowedFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, RequiredFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}},
	},
	"revokeDelegation": {
		"":            {AllowedFields: []string{"delegation", "protocol"}, RequiredFields: []string{"delegation", "protocol"}},
		"/delegation": {AllowedFields: []string{"delegation_ref", "effective_actor_ref", "expires_at", "requested_actor_ref", "scope_digest"}, RequiredFields: []string{"delegation_ref", "effective_actor_ref", "expires_at", "requested_actor_ref", "scope_digest"}},
		"/protocol":   {AllowedFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, RequiredFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}},
	},
	"revokeMembership": {
		"":            {AllowedFields: []string{"membership", "protocol"}, RequiredFields: []string{"membership", "protocol"}},
		"/membership": {AllowedFields: []string{"bound_scope_digest", "expires_at", "membership_ref"}, RequiredFields: []string{"bound_scope_digest", "expires_at", "membership_ref"}},
		"/protocol":   {AllowedFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, RequiredFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}},
	},
	"revokeServiceAccount": {
		"":                 {AllowedFields: []string{"protocol", "service_account"}, RequiredFields: []string{"protocol", "service_account"}},
		"/protocol":        {AllowedFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, RequiredFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}},
		"/service_account": {AllowedFields: []string{"expires_at", "grant_revision", "service_account_ref"}, RequiredFields: []string{"expires_at", "grant_revision", "service_account_ref"}},
	},
	"submitInvocation": {
		"":                           {AllowedFields: []string{"approval_ref", "connection_ref", "input", "lane", "operation_ref", "protocol", "result_controls"}, RequiredFields: []string{"connection_ref", "input", "lane", "operation_ref", "protocol", "result_controls"}},
		"/protocol":                  {AllowedFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, RequiredFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}},
		"/result_controls":           {AllowedFields: []string{"filter", "selection"}, RequiredFields: []string{}},
		"/result_controls/filter":    {AllowedFields: []string{"operator", "path", "value"}, RequiredFields: []string{"operator", "path"}},
		"/result_controls/selection": {AllowedFields: []string{"paths"}, RequiredFields: []string{"paths"}},
	},
}

func OperationControlFor(operationID string) (OperationControl, bool) {
	control, ok := operationControls[operationID]
	if !ok {
		return OperationControl{}, false
	}
	control.RequiredBodyFields = append([]string(nil), control.RequiredBodyFields...)
	control.OptionalBodyFields = append([]string(nil), control.OptionalBodyFields...)
	return control, true
}

func OperationIDForRoute(method, path string) (string, bool) {
	operationID, ok := operationIDsByRoute[strings.ToUpper(method)+" "+path]
	return operationID, ok
}

func IsTerminalRunState[T ~string](state T) bool {
	switch state {
	case "cancelled", "failed", "partial", "succeeded":
		return true
	default:
		return false
	}
}

func IsSupportedFeature(feature string) bool {
	switch feature {
	case "exact-values", "metadata-only-replay", "typed-result-controls", "wire-problem-v1":
		return true
	default:
		return false
	}
}

func IsForbiddenPublicField(field string) bool {
	switch strings.ToLower(field) {
	case "access_token", "api_key", "client_secret", "credential_value", "password", "pm_automation_credential", "provider_credential", "provider_secret", "refresh_token", "secret":
		return true
	default:
		return false
	}
}
