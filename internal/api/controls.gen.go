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
	ProblemUnclassifiedFailure    ProblemDisposition = "unclassified_failure"
)

type ProblemRule struct {
	Status          int
	Code            string
	SafeRetryAction string
	Disposition     ProblemDisposition
}

var ProblemRules = [...]ProblemRule{
	{Status: 400, Code: "invalid_request", SafeRetryAction: "correct_input", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 400, Code: "invalid_event_cursor", SafeRetryAction: "correct_input", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 401, Code: "authentication_required", SafeRetryAction: "refresh_authorization", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 403, Code: "grant_unavailable", SafeRetryAction: "refresh_authorization", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 404, Code: "resource_not_found", SafeRetryAction: "correct_scope", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 409, Code: "foundation_incompatible", SafeRetryAction: "after_capability_change", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 409, Code: "idempotency_conflict", SafeRetryAction: "new_key_for_new_intent", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 409, Code: "resume_unavailable", SafeRetryAction: "refresh_run_state", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 409, Code: "specification_stale", SafeRetryAction: "refresh_specification", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 410, Code: "retry_receipt_expired", SafeRetryAction: "inspect_existing_result", Disposition: ProblemDisposition("resolved_receipt_expired")},
	{Status: 412, Code: "control_revision_mismatch", SafeRetryAction: "refresh_control_revision", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 416, Code: "artifact_range_not_satisfiable", SafeRetryAction: "correct_input", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 428, Code: "control_revision_required", SafeRetryAction: "refresh_control_revision", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 429, Code: "workspace_admission_age_limit", SafeRetryAction: "after_backlog_progress", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 429, Code: "workspace_admission_count_limit", SafeRetryAction: "after_backlog_progress", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 503, Code: "admission_outcome_unknown", SafeRetryAction: "same_key_reconcile_only", Disposition: ProblemDisposition("outcome_unknown")},
	{Status: 503, Code: "capability_unavailable", SafeRetryAction: "after_capability_change", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 503, Code: "control_store_unavailable", SafeRetryAction: "same_key_after_dependency_recovery", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 503, Code: "deployment_admission_overload", SafeRetryAction: "after_backlog_progress", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 400, Code: "login_challenge_invalid", SafeRetryAction: "restart_login", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 401, Code: "oidc_verification_failed", SafeRetryAction: "restart_login", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 403, Code: "inbound_policy_rejected", SafeRetryAction: "use_trusted_transport", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 403, Code: "csrf_validation_failed", SafeRetryAction: "refresh_browser_session", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 403, Code: "device_authorization_denied", SafeRetryAction: "restart_login", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 403, Code: "reauthentication_required", SafeRetryAction: "reauthenticate", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 409, Code: "resource_conflict", SafeRetryAction: "inspect_existing_result", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 409, Code: "login_challenge_used", SafeRetryAction: "restart_login", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 409, Code: "device_authorization_pending", SafeRetryAction: "poll_after_interval", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 409, Code: "credential_revision_stale", SafeRetryAction: "refresh_control_revision", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 410, Code: "login_challenge_expired", SafeRetryAction: "restart_login", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 429, Code: "device_poll_too_soon", SafeRetryAction: "poll_after_interval", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 429, Code: "authentication_rate_limited", SafeRetryAction: "retry_after_backoff", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 500, Code: "internal_error", SafeRetryAction: "contact_support_with_incident_id", Disposition: ProblemDisposition("unclassified_failure")},
	{Status: 503, Code: "credential_storage_unavailable", SafeRetryAction: "after_capability_change", Disposition: ProblemDisposition("confirmed_refusal")},
	{Status: 503, Code: "mutation_outcome_unknown", SafeRetryAction: "same_key_reconcile_only", Disposition: ProblemDisposition("outcome_unknown")},
}

func ProblemRuleFor(status int, code string) (ProblemRule, bool) {
	for _, rule := range ProblemRules {
		if rule.Status == status && rule.Code == code {
			return rule, true
		}
	}
	return ProblemRule{}, false
}

var SupportedFeatures = [...]string{"catalog-v1", "exact-values", "metadata-only-replay", "typed-result-controls", "wire-problem-v1"}

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
	HasBody                bool
	HasProtocol            bool
	QueryFields            []string
}

var operationControls = map[string]OperationControl{
	"approveDeviceLogin":    {OperationID: "approveDeviceLogin", Method: "POST", Path: "/v1/sessions/device/approve", RequiredBodyFields: []string{"challenge_id", "user_code"}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: true, HasProtocol: false, QueryFields: []string{}},
	"cancelRun":             {OperationID: "cancelRun", Method: "POST", Path: "/v1/workspaces/{workspace_id}/runs/{run_id}/cancel", RequiredBodyFields: []string{"protocol"}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: true, RequiresIfMatch: true, HasBody: true, HasProtocol: true, QueryFields: []string{}},
	"completeBrowserLogin":  {OperationID: "completeBrowserLogin", Method: "POST", Path: "/v1/sessions/browser", RequiredBodyFields: []string{"code", "state"}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: true, HasProtocol: false, QueryFields: []string{}},
	"createConnection":      {OperationID: "createConnection", Method: "POST", Path: "/v1/workspaces/{workspace_id}/connections", RequiredBodyFields: []string{"connection", "protocol"}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: true, RequiresIfMatch: true, HasBody: true, HasProtocol: true, QueryFields: []string{}},
	"createDelegation":      {OperationID: "createDelegation", Method: "POST", Path: "/v1/workspaces/{workspace_id}/delegations", RequiredBodyFields: []string{"delegation", "protocol"}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: true, RequiresIfMatch: true, HasBody: true, HasProtocol: true, QueryFields: []string{}},
	"createServiceAccount":  {OperationID: "createServiceAccount", Method: "POST", Path: "/v1/workspaces/{workspace_id}/service-accounts", RequiredBodyFields: []string{"protocol", "service_account"}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: true, RequiresIfMatch: true, HasBody: true, HasProtocol: true, QueryFields: []string{}},
	"deleteConnection":      {OperationID: "deleteConnection", Method: "DELETE", Path: "/v1/workspaces/{workspace_id}/connections/{connection_id}", RequiredBodyFields: []string{"protocol"}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: true, RequiresIfMatch: true, HasBody: true, HasProtocol: true, QueryFields: []string{}},
	"denyDeviceLogin":       {OperationID: "denyDeviceLogin", Method: "POST", Path: "/v1/sessions/device/deny", RequiredBodyFields: []string{"challenge_id", "user_code"}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: true, HasProtocol: false, QueryFields: []string{}},
	"downloadArtifact":      {OperationID: "downloadArtifact", Method: "GET", Path: "/v1/workspaces/{workspace_id}/artifacts/{artifact_id}/content", RequiredBodyFields: []string{}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: false, HasProtocol: false, QueryFields: []string{}},
	"exchangeDeviceLogin":   {OperationID: "exchangeDeviceLogin", Method: "POST", Path: "/v1/sessions/device/exchange", RequiredBodyFields: []string{"challenge_id", "proof"}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: true, HasProtocol: false, QueryFields: []string{}},
	"getCatalogCommand":     {OperationID: "getCatalogCommand", Method: "GET", Path: "/v1/workspaces/{workspace_id}/commands/{operation_id}", RequiredBodyFields: []string{}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: false, HasProtocol: false, QueryFields: []string{"connection_id"}},
	"getCatalogConnector":   {OperationID: "getCatalogConnector", Method: "GET", Path: "/v1/workspaces/{workspace_id}/connectors/{connector_id}", RequiredBodyFields: []string{}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: false, HasProtocol: false, QueryFields: []string{}},
	"getConnection":         {OperationID: "getConnection", Method: "GET", Path: "/v1/workspaces/{workspace_id}/connections/{connection_id}", RequiredBodyFields: []string{}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: false, HasProtocol: false, QueryFields: []string{}},
	"getCurrentSession":     {OperationID: "getCurrentSession", Method: "GET", Path: "/v1/sessions/me", RequiredBodyFields: []string{}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: false, HasProtocol: false, QueryFields: []string{"cursor", "page_size"}},
	"getRun":                {OperationID: "getRun", Method: "GET", Path: "/v1/workspaces/{workspace_id}/runs/{run_id}", RequiredBodyFields: []string{}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: false, HasProtocol: false, QueryFields: []string{}},
	"getRunResult":          {OperationID: "getRunResult", Method: "GET", Path: "/v1/workspaces/{workspace_id}/runs/{run_id}/result", RequiredBodyFields: []string{}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: false, HasProtocol: false, QueryFields: []string{}},
	"getServerInfo":         {OperationID: "getServerInfo", Method: "GET", Path: "/v1/info", RequiredBodyFields: []string{}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: false, HasProtocol: false, QueryFields: []string{}},
	"getTask":               {OperationID: "getTask", Method: "GET", Path: "/v1/workspaces/{workspace_id}/tasks/{task_id}", RequiredBodyFields: []string{}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: false, HasProtocol: false, QueryFields: []string{}},
	"getWorkspace":          {OperationID: "getWorkspace", Method: "GET", Path: "/v1/workspaces/{workspace_id}", RequiredBodyFields: []string{}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: false, HasProtocol: false, QueryFields: []string{}},
	"grantMembership":       {OperationID: "grantMembership", Method: "POST", Path: "/v1/workspaces/{workspace_id}/memberships", RequiredBodyFields: []string{"membership", "protocol"}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: true, RequiresIfMatch: true, HasBody: true, HasProtocol: true, QueryFields: []string{}},
	"inspectArtifact":       {OperationID: "inspectArtifact", Method: "GET", Path: "/v1/workspaces/{workspace_id}/artifacts/{artifact_id}", RequiredBodyFields: []string{}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: false, HasProtocol: false, QueryFields: []string{}},
	"listCatalogCommands":   {OperationID: "listCatalogCommands", Method: "GET", Path: "/v1/workspaces/{workspace_id}/commands", RequiredBodyFields: []string{}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: false, HasProtocol: false, QueryFields: []string{"connection_id", "cursor", "filter", "page_size"}},
	"listCatalogConnectors": {OperationID: "listCatalogConnectors", Method: "GET", Path: "/v1/workspaces/{workspace_id}/connectors", RequiredBodyFields: []string{}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: false, HasProtocol: false, QueryFields: []string{"cursor", "page_size"}},
	"listConnections":       {OperationID: "listConnections", Method: "GET", Path: "/v1/workspaces/{workspace_id}/connections", RequiredBodyFields: []string{}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: false, HasProtocol: false, QueryFields: []string{"cursor", "page_size"}},
	"listDelegations":       {OperationID: "listDelegations", Method: "GET", Path: "/v1/workspaces/{workspace_id}/delegations", RequiredBodyFields: []string{}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: false, HasProtocol: false, QueryFields: []string{"cursor", "page_size"}},
	"listMemberships":       {OperationID: "listMemberships", Method: "GET", Path: "/v1/workspaces/{workspace_id}/memberships", RequiredBodyFields: []string{}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: false, HasProtocol: false, QueryFields: []string{"cursor", "page_size"}},
	"listRunTasks":          {OperationID: "listRunTasks", Method: "GET", Path: "/v1/workspaces/{workspace_id}/runs/{run_id}/tasks", RequiredBodyFields: []string{}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: false, HasProtocol: false, QueryFields: []string{"cursor", "page_size"}},
	"listRuns":              {OperationID: "listRuns", Method: "GET", Path: "/v1/workspaces/{workspace_id}/runs", RequiredBodyFields: []string{}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: false, HasProtocol: false, QueryFields: []string{"cursor", "page_size", "state"}},
	"listServiceAccounts":   {OperationID: "listServiceAccounts", Method: "GET", Path: "/v1/workspaces/{workspace_id}/service-accounts", RequiredBodyFields: []string{}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: false, HasProtocol: false, QueryFields: []string{"cursor", "page_size"}},
	"listWorkspaces":        {OperationID: "listWorkspaces", Method: "GET", Path: "/v1/workspaces", RequiredBodyFields: []string{}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: false, HasProtocol: false, QueryFields: []string{"cursor", "page_size"}},
	"resumeRun":             {OperationID: "resumeRun", Method: "POST", Path: "/v1/workspaces/{workspace_id}/runs/{run_id}/resume", RequiredBodyFields: []string{"approval_ref", "protocol", "recovery_generation"}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: true, RequiresIfMatch: true, HasBody: true, HasProtocol: true, QueryFields: []string{}},
	"revokeCurrentSession":  {OperationID: "revokeCurrentSession", Method: "DELETE", Path: "/v1/sessions/current", RequiredBodyFields: []string{}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: false, HasProtocol: false, QueryFields: []string{}},
	"revokeDelegation":      {OperationID: "revokeDelegation", Method: "POST", Path: "/v1/workspaces/{workspace_id}/delegations/{id}/revoke", RequiredBodyFields: []string{"delegation", "protocol"}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: true, RequiresIfMatch: true, HasBody: true, HasProtocol: true, QueryFields: []string{}},
	"revokeMembership":      {OperationID: "revokeMembership", Method: "POST", Path: "/v1/workspaces/{workspace_id}/memberships/{id}/revoke", RequiredBodyFields: []string{"membership", "protocol"}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: true, RequiresIfMatch: true, HasBody: true, HasProtocol: true, QueryFields: []string{}},
	"revokeServiceAccount":  {OperationID: "revokeServiceAccount", Method: "POST", Path: "/v1/workspaces/{workspace_id}/service-accounts/{id}/revoke", RequiredBodyFields: []string{"protocol", "service_account"}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: true, RequiresIfMatch: true, HasBody: true, HasProtocol: true, QueryFields: []string{}},
	"startBrowserLogin":     {OperationID: "startBrowserLogin", Method: "POST", Path: "/v1/sessions/browser/challenges", RequiredBodyFields: []string{"reauthenticate"}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: true, HasProtocol: false, QueryFields: []string{}},
	"startDeviceLogin":      {OperationID: "startDeviceLogin", Method: "POST", Path: "/v1/sessions/device", RequiredBodyFields: []string{"device_id"}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: true, HasProtocol: false, QueryFields: []string{}},
	"submitInvocation":      {OperationID: "submitInvocation", Method: "POST", Path: "/v1/workspaces/{workspace_id}/invocations", RequiredBodyFields: []string{"connection_ref", "input", "lane", "operation_ref", "protocol", "result_controls"}, OptionalBodyFields: []string{"approval_ref"}, RequiresIdempotencyKey: true, RequiresIfMatch: false, HasBody: true, HasProtocol: true, QueryFields: []string{}},
	"updateConnection":      {OperationID: "updateConnection", Method: "PATCH", Path: "/v1/workspaces/{workspace_id}/connections/{connection_id}", RequiredBodyFields: []string{"connection", "protocol"}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: true, RequiresIfMatch: true, HasBody: true, HasProtocol: true, QueryFields: []string{}},
	"uploadArtifact":        {OperationID: "uploadArtifact", Method: "POST", Path: "/v1/workspaces/{workspace_id}/artifacts", RequiredBodyFields: []string{}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: false, HasProtocol: false, QueryFields: []string{}},
	"watchRunEvents":        {OperationID: "watchRunEvents", Method: "GET", Path: "/v1/workspaces/{workspace_id}/runs/{run_id}/events", RequiredBodyFields: []string{}, OptionalBodyFields: []string{}, RequiresIdempotencyKey: false, RequiresIfMatch: false, HasBody: false, HasProtocol: false, QueryFields: []string{}},
}

var operationIDsByRoute = map[string]string{
	"DELETE /v1/sessions/current":                                      "revokeCurrentSession",
	"DELETE /v1/workspaces/{workspace_id}/connections/{connection_id}": "deleteConnection",
	"DELETE /v1/workspaces/:workspace_id/connections/:connection_id":   "deleteConnection",
	"GET /v1/info":                                                      "getServerInfo",
	"GET /v1/sessions/me":                                               "getCurrentSession",
	"GET /v1/workspaces":                                                "listWorkspaces",
	"GET /v1/workspaces/{workspace_id}":                                 "getWorkspace",
	"GET /v1/workspaces/:workspace_id":                                  "getWorkspace",
	"GET /v1/workspaces/{workspace_id}/artifacts/{artifact_id}":         "inspectArtifact",
	"GET /v1/workspaces/:workspace_id/artifacts/:artifact_id":           "inspectArtifact",
	"GET /v1/workspaces/{workspace_id}/artifacts/{artifact_id}/content": "downloadArtifact",
	"GET /v1/workspaces/:workspace_id/artifacts/:artifact_id/content":   "downloadArtifact",
	"GET /v1/workspaces/{workspace_id}/commands":                        "listCatalogCommands",
	"GET /v1/workspaces/:workspace_id/commands":                         "listCatalogCommands",
	"GET /v1/workspaces/{workspace_id}/commands/{operation_id}":         "getCatalogCommand",
	"GET /v1/workspaces/:workspace_id/commands/:operation_id":           "getCatalogCommand",
	"GET /v1/workspaces/{workspace_id}/connections":                     "listConnections",
	"GET /v1/workspaces/:workspace_id/connections":                      "listConnections",
	"GET /v1/workspaces/{workspace_id}/connections/{connection_id}":     "getConnection",
	"GET /v1/workspaces/:workspace_id/connections/:connection_id":       "getConnection",
	"GET /v1/workspaces/{workspace_id}/connectors":                      "listCatalogConnectors",
	"GET /v1/workspaces/:workspace_id/connectors":                       "listCatalogConnectors",
	"GET /v1/workspaces/{workspace_id}/connectors/{connector_id}":       "getCatalogConnector",
	"GET /v1/workspaces/:workspace_id/connectors/:connector_id":         "getCatalogConnector",
	"GET /v1/workspaces/{workspace_id}/delegations":                     "listDelegations",
	"GET /v1/workspaces/:workspace_id/delegations":                      "listDelegations",
	"GET /v1/workspaces/{workspace_id}/memberships":                     "listMemberships",
	"GET /v1/workspaces/:workspace_id/memberships":                      "listMemberships",
	"GET /v1/workspaces/{workspace_id}/runs":                            "listRuns",
	"GET /v1/workspaces/:workspace_id/runs":                             "listRuns",
	"GET /v1/workspaces/{workspace_id}/runs/{run_id}":                   "getRun",
	"GET /v1/workspaces/:workspace_id/runs/:run_id":                     "getRun",
	"GET /v1/workspaces/{workspace_id}/runs/{run_id}/events":            "watchRunEvents",
	"GET /v1/workspaces/:workspace_id/runs/:run_id/events":              "watchRunEvents",
	"GET /v1/workspaces/{workspace_id}/runs/{run_id}/result":            "getRunResult",
	"GET /v1/workspaces/:workspace_id/runs/:run_id/result":              "getRunResult",
	"GET /v1/workspaces/{workspace_id}/runs/{run_id}/tasks":             "listRunTasks",
	"GET /v1/workspaces/:workspace_id/runs/:run_id/tasks":               "listRunTasks",
	"GET /v1/workspaces/{workspace_id}/service-accounts":                "listServiceAccounts",
	"GET /v1/workspaces/:workspace_id/service-accounts":                 "listServiceAccounts",
	"GET /v1/workspaces/{workspace_id}/tasks/{task_id}":                 "getTask",
	"GET /v1/workspaces/:workspace_id/tasks/:task_id":                   "getTask",
	"PATCH /v1/workspaces/{workspace_id}/connections/{connection_id}":   "updateConnection",
	"PATCH /v1/workspaces/:workspace_id/connections/:connection_id":     "updateConnection",
	"POST /v1/sessions/browser":                                         "completeBrowserLogin",
	"POST /v1/sessions/browser/challenges":                              "startBrowserLogin",
	"POST /v1/sessions/device":                                          "startDeviceLogin",
	"POST /v1/sessions/device/approve":                                  "approveDeviceLogin",
	"POST /v1/sessions/device/deny":                                     "denyDeviceLogin",
	"POST /v1/sessions/device/exchange":                                 "exchangeDeviceLogin",
	"POST /v1/workspaces/{workspace_id}/artifacts":                      "uploadArtifact",
	"POST /v1/workspaces/:workspace_id/artifacts":                       "uploadArtifact",
	"POST /v1/workspaces/{workspace_id}/connections":                    "createConnection",
	"POST /v1/workspaces/:workspace_id/connections":                     "createConnection",
	"POST /v1/workspaces/{workspace_id}/delegations":                    "createDelegation",
	"POST /v1/workspaces/:workspace_id/delegations":                     "createDelegation",
	"POST /v1/workspaces/{workspace_id}/delegations/{id}/revoke":        "revokeDelegation",
	"POST /v1/workspaces/:workspace_id/delegations/:id/revoke":          "revokeDelegation",
	"POST /v1/workspaces/{workspace_id}/invocations":                    "submitInvocation",
	"POST /v1/workspaces/:workspace_id/invocations":                     "submitInvocation",
	"POST /v1/workspaces/{workspace_id}/memberships":                    "grantMembership",
	"POST /v1/workspaces/:workspace_id/memberships":                     "grantMembership",
	"POST /v1/workspaces/{workspace_id}/memberships/{id}/revoke":        "revokeMembership",
	"POST /v1/workspaces/:workspace_id/memberships/:id/revoke":          "revokeMembership",
	"POST /v1/workspaces/{workspace_id}/runs/{run_id}/cancel":           "cancelRun",
	"POST /v1/workspaces/:workspace_id/runs/:run_id/cancel":             "cancelRun",
	"POST /v1/workspaces/{workspace_id}/runs/{run_id}/resume":           "resumeRun",
	"POST /v1/workspaces/:workspace_id/runs/:run_id/resume":             "resumeRun",
	"POST /v1/workspaces/{workspace_id}/service-accounts":               "createServiceAccount",
	"POST /v1/workspaces/:workspace_id/service-accounts":                "createServiceAccount",
	"POST /v1/workspaces/{workspace_id}/service-accounts/{id}/revoke":   "revokeServiceAccount",
	"POST /v1/workspaces/:workspace_id/service-accounts/:id/revoke":     "revokeServiceAccount",
}

type JSONFieldControl struct {
	AllowedFields  []string
	RequiredFields []string
	Nullable       bool
}

var operationObjectControls = map[string]map[string]JSONFieldControl{
	"approveDeviceLogin": {
		"": {AllowedFields: []string{"challenge_id", "user_code"}, RequiredFields: []string{"challenge_id", "user_code"}, Nullable: false},
	},
	"cancelRun": {
		"":          {AllowedFields: []string{"protocol"}, RequiredFields: []string{"protocol"}, Nullable: false},
		"/protocol": {AllowedFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, RequiredFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, Nullable: false},
	},
	"completeBrowserLogin": {
		"": {AllowedFields: []string{"code", "state"}, RequiredFields: []string{"code", "state"}, Nullable: false},
	},
	"createConnection": {
		"":            {AllowedFields: []string{"connection", "protocol"}, RequiredFields: []string{"connection", "protocol"}, Nullable: false},
		"/connection": {AllowedFields: []string{"connector_id", "display_name", "provider_account_id"}, RequiredFields: []string{"connector_id", "display_name", "provider_account_id"}, Nullable: false},
		"/protocol":   {AllowedFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, RequiredFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, Nullable: false},
	},
	"createDelegation": {
		"":            {AllowedFields: []string{"delegation", "protocol"}, RequiredFields: []string{"delegation", "protocol"}, Nullable: false},
		"/delegation": {AllowedFields: []string{"connection_scopes", "effective_actor_ref", "expires_at", "operation_scopes", "requested_actor_ref", "workspace_scopes"}, RequiredFields: []string{"connection_scopes", "effective_actor_ref", "expires_at", "operation_scopes", "requested_actor_ref", "workspace_scopes"}, Nullable: false},
		"/protocol":   {AllowedFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, RequiredFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, Nullable: false},
	},
	"createServiceAccount": {
		"":                 {AllowedFields: []string{"protocol", "service_account"}, RequiredFields: []string{"protocol", "service_account"}, Nullable: false},
		"/protocol":        {AllowedFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, RequiredFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, Nullable: false},
		"/service_account": {AllowedFields: []string{"connection_scopes", "display_name", "expires_at", "operation_scopes", "workspace_scopes"}, RequiredFields: []string{"connection_scopes", "display_name", "expires_at", "operation_scopes", "workspace_scopes"}, Nullable: false},
	},
	"deleteConnection": {
		"":          {AllowedFields: []string{"protocol"}, RequiredFields: []string{"protocol"}, Nullable: false},
		"/protocol": {AllowedFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, RequiredFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, Nullable: false},
	},
	"denyDeviceLogin": {
		"": {AllowedFields: []string{"challenge_id", "user_code"}, RequiredFields: []string{"challenge_id", "user_code"}, Nullable: false},
	},
	"downloadArtifact": {},
	"exchangeDeviceLogin": {
		"": {AllowedFields: []string{"challenge_id", "proof"}, RequiredFields: []string{"challenge_id", "proof"}, Nullable: false},
	},
	"getCatalogCommand":   {},
	"getCatalogConnector": {},
	"getConnection":       {},
	"getCurrentSession":   {},
	"getRun":              {},
	"getRunResult":        {},
	"getServerInfo":       {},
	"getTask":             {},
	"getWorkspace":        {},
	"grantMembership": {
		"":            {AllowedFields: []string{"membership", "protocol"}, RequiredFields: []string{"membership", "protocol"}, Nullable: false},
		"/membership": {AllowedFields: []string{"expires_at", "principal_ref", "resource_grants", "role"}, RequiredFields: []string{"expires_at", "principal_ref", "resource_grants", "role"}, Nullable: false},
		"/protocol":   {AllowedFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, RequiredFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, Nullable: false},
	},
	"inspectArtifact":       {},
	"listCatalogCommands":   {},
	"listCatalogConnectors": {},
	"listConnections":       {},
	"listDelegations":       {},
	"listMemberships":       {},
	"listRunTasks":          {},
	"listRuns":              {},
	"listServiceAccounts":   {},
	"listWorkspaces":        {},
	"resumeRun": {
		"":          {AllowedFields: []string{"approval_ref", "protocol", "recovery_generation"}, RequiredFields: []string{"approval_ref", "protocol", "recovery_generation"}, Nullable: false},
		"/protocol": {AllowedFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, RequiredFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, Nullable: false},
	},
	"revokeCurrentSession": {},
	"revokeDelegation": {
		"":            {AllowedFields: []string{"delegation", "protocol"}, RequiredFields: []string{"delegation", "protocol"}, Nullable: false},
		"/delegation": {AllowedFields: []string{"delegation_ref", "effective_actor_ref", "expires_at", "requested_actor_ref", "scope_digest"}, RequiredFields: []string{"delegation_ref", "effective_actor_ref", "expires_at", "requested_actor_ref", "scope_digest"}, Nullable: false},
		"/protocol":   {AllowedFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, RequiredFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, Nullable: false},
	},
	"revokeMembership": {
		"":            {AllowedFields: []string{"membership", "protocol"}, RequiredFields: []string{"membership", "protocol"}, Nullable: false},
		"/membership": {AllowedFields: []string{"bound_scope_digest", "expires_at", "membership_ref"}, RequiredFields: []string{"bound_scope_digest", "expires_at", "membership_ref"}, Nullable: false},
		"/protocol":   {AllowedFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, RequiredFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, Nullable: false},
	},
	"revokeServiceAccount": {
		"":                 {AllowedFields: []string{"protocol", "service_account"}, RequiredFields: []string{"protocol", "service_account"}, Nullable: false},
		"/protocol":        {AllowedFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, RequiredFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, Nullable: false},
		"/service_account": {AllowedFields: []string{"expires_at", "grant_revision", "service_account_ref"}, RequiredFields: []string{"expires_at", "grant_revision", "service_account_ref"}, Nullable: false},
	},
	"startBrowserLogin": {
		"": {AllowedFields: []string{"reauthenticate"}, RequiredFields: []string{"reauthenticate"}, Nullable: false},
	},
	"startDeviceLogin": {
		"": {AllowedFields: []string{"device_id"}, RequiredFields: []string{"device_id"}, Nullable: false},
	},
	"submitInvocation": {
		"":                           {AllowedFields: []string{"approval_ref", "connection_ref", "input", "lane", "operation_ref", "protocol", "result_controls"}, RequiredFields: []string{"connection_ref", "input", "lane", "operation_ref", "protocol", "result_controls"}, Nullable: false},
		"/protocol":                  {AllowedFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, RequiredFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, Nullable: false},
		"/result_controls":           {AllowedFields: []string{"continuation", "filter", "selection", "total_record_cap"}, RequiredFields: []string{}, Nullable: false},
		"/result_controls/filter":    {AllowedFields: []string{"operator", "path", "value"}, RequiredFields: []string{"operator", "path"}, Nullable: false},
		"/result_controls/selection": {AllowedFields: []string{"paths"}, RequiredFields: []string{"paths"}, Nullable: false},
	},
	"updateConnection": {
		"":            {AllowedFields: []string{"connection", "protocol"}, RequiredFields: []string{"connection", "protocol"}, Nullable: false},
		"/connection": {AllowedFields: []string{"display_name"}, RequiredFields: []string{"display_name"}, Nullable: false},
		"/protocol":   {AllowedFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, RequiredFields: []string{"expected_revision", "intent_digest", "request_key", "required_features", "wire_version"}, Nullable: false},
	},
	"uploadArtifact": {},
	"watchRunEvents": {},
}

func OperationControlFor(operationID string) (OperationControl, bool) {
	control, ok := operationControls[operationID]
	if !ok {
		return OperationControl{}, false
	}
	control.RequiredBodyFields = append([]string(nil), control.RequiredBodyFields...)
	control.OptionalBodyFields = append([]string(nil), control.OptionalBodyFields...)
	control.QueryFields = append([]string(nil), control.QueryFields...)
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
	case "catalog-v1", "exact-values", "metadata-only-replay", "typed-result-controls", "wire-problem-v1":
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
