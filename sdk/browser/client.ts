import createClient, { type Client as OpenAPIClient } from "openapi-fetch";
import controls from "../../api/controls.json" with { type: "json" };
import type { components, paths } from "./schema.js";

type Invocation = components["schemas"]["InvocationRequest"];
type Problem = components["schemas"]["Problem"];
type Principal = components["schemas"]["Principal"];
export type InvocationInput = Omit<Invocation, "protocol">;
export type BrowserAdmission = components["schemas"]["AdmissionResponse"];

export interface BrowserClientOptions {
  expectedInstanceID: string;
  csrfToken?: string;
  requestTimeoutMS?: number;
}

export interface InvocationOptions {
  expectedRevision: string;
  requestKey?: string;
  signal?: AbortSignal;
}

export interface BrowserClient {
  api: OpenAPIClient<paths>;
  information: Readonly<components["schemas"]["ServerInfo"]>;
  session: Readonly<components["schemas"]["CurrentSession"]>;
  submitInvocation(workspace: string, input: InvocationInput, options: InvocationOptions): Promise<BrowserAdmission>;
}

export class PMBrowserError extends Error {
  constructor(
    readonly code: string,
    readonly status = 0,
    readonly incidentID = "",
    readonly safeRetryAction = "",
    readonly requestKey = "",
  ) {
    super(code);
    this.name = "PMBrowserError";
  }
}

const maximumEnvelopeBytes = 1 << 20;
const encoder = new TextEncoder();

/** No bearer/provider credential or alternate API origin enters this client. */
export async function createBrowserClient(options: BrowserClientOptions): Promise<BrowserClient> {
  const origin = location.origin;
  const timeout = options.requestTimeoutMS ?? 30_000;
  const csrfToken = options.csrfToken;
  if (location.protocol !== "https:" || !identity(options.expectedInstanceID, 512) ||
      !Number.isInteger(timeout) || timeout < 1_000 || timeout > 300_000 ||
      (csrfToken !== undefined && !identity(csrfToken, 512))) {
    throw new PMBrowserError("invalid_client_configuration");
  }
  const expectedInstanceID = options.expectedInstanceID;
  const api = createClient<paths>({
    baseUrl: origin,
    credentials: "same-origin",
    redirect: "error",
    fetch: async (input: Request) => {
      const url = new URL(input.url);
      if (url.origin !== origin || url.username || url.password || url.hash ||
          input.headers.has("Authorization") || !publicRoute(input.method, url.pathname)) {
        throw new PMBrowserError("unsafe_client_request");
      }
      const headers = new Headers(input.headers);
      if (input.method !== "GET" && input.method !== "HEAD" && csrfToken !== undefined) {
        headers.set("X-CSRF-Token", csrfToken);
      }
      const response = await fetch(new Request(input, {
        headers,
        credentials: "same-origin",
        mode: "same-origin",
        redirect: "error",
        signal: AbortSignal.any([input.signal, AbortSignal.timeout(timeout)]),
      }));
      if (!response.body || response.headers.get("Content-Type")?.split(";", 1)[0] === "text/event-stream") {
        return response;
      }
      let size = 0;
      const body = response.body.pipeThrough(new TransformStream<Uint8Array, Uint8Array>({
        transform(chunk, controller) {
          size += chunk.byteLength;
          if (size > maximumEnvelopeBytes) {
            controller.error(new PMBrowserError("response_too_large"));
            return;
          }
          controller.enqueue(chunk);
        },
      }));
      return new Response(body, { status: response.status, statusText: response.statusText, headers: response.headers });
    },
  });
  const discovery = await api.GET("/v1/info");
  if (discovery.error) throw problemError(discovery.response, discovery.error);
  const information = discovery.data;
  if (!information || !safePublicFields(information) || information.instance_id !== expectedInstanceID ||
      information.protocol_major !== controls.protocol_major ||
      information.protocol_version !== controls.wire_version ||
      !featureList(information.features) || !featureList(information.required_features) ||
      information.required_features.some(feature => !controls.supported_features.includes(feature))) {
    throw new PMBrowserError("incompatible_server");
  }
  const current = await api.GET("/v1/sessions/me");
  if (current.error) throw problemError(current.response, current.error);
  if (!current.data || !safePublicFields(current.data) || current.data.transport !== "browser_session" ||
      !current.data.principal || !identity(current.data.principal.principal_id, 512)) {
    throw new PMBrowserError("browser_session_required");
  }
  const principal = structuredClone(current.data.principal);

  async function submitInvocation(workspace: string, input: InvocationInput, invocation: InvocationOptions) {
    if (!identity(workspace, 512) || !identity(invocation.expectedRevision, 512)) {
      throw new PMBrowserError("invalid_invocation_scope");
    }
    const payload = structuredClone(input);
    const target = `${payload.connection_ref}/${payload.operation_ref}`;
    const requestKey = invocation.requestKey ?? crypto.randomUUID();
    if (!identity(requestKey, 200)) throw new PMBrowserError("invalid_request_key");
    const intentDigest = await canonicalDigest({
      expected_revision: invocation.expectedRevision,
      operation: "invocation.submit",
      payload,
      target,
      workspace,
    });
    const body: Invocation = {
      ...payload,
      protocol: {
        expected_revision: invocation.expectedRevision,
        intent_digest: intentDigest,
        request_key: requestKey,
        required_features: [],
        wire_version: controls.wire_version as Invocation["protocol"]["wire_version"],
      },
    };
    await persistRecovery(expectedInstanceID, workspace, principal, target, body);
    const deadline = Date.now() + 30_000;
    const signal = AbortSignal.any([
      ...(invocation.signal ? [invocation.signal] : []),
      AbortSignal.timeout(30_000),
    ]);
    let failure = new PMBrowserError("mutation_outcome_unknown", 0, "", "same_key_reconcile_only", requestKey);
    for (let send = 0; send < 3; send++) {
      let retryAfter: string | null = null;
      try {
        const result = await api.POST("/v1/workspaces/{workspace_id}/invocations", {
          params: { path: { workspace_id: workspace }, header: { "Idempotency-Key": requestKey } },
          body,
          signal,
        });
        if (result.error) {
          failure = problemError(result.response, result.error, requestKey);
          if (!retryable(failure)) throw failure;
          retryAfter = result.response.headers.get("Retry-After");
        } else {
          if (!result.data || result.response.status !== 202 || result.data.state !== "accepted" ||
              result.data.request_key !== requestKey || !/^sha256:[0-9a-f]{64}$/.test(result.data.intent_digest) ||
              !identity(result.data.run_ref, 512) || typeof result.data.replay !== "boolean" ||
              !isOutcomeReference(result.data.outcome_reference) || !safePublicFields(result.data)) {
            throw new PMBrowserError("malformed_admission_response", 0, "", "same_key_reconcile_only", requestKey);
          }
          return result.data;
        }
      } catch (error) {
        if (error instanceof PMBrowserError && !retryable(error)) throw error;
        failure = new PMBrowserError("mutation_outcome_unknown", 0, "", "same_key_reconcile_only", requestKey);
        if (signal.aborted) throw failure;
      }
      if (send === 2) break;
      const delay = retryDelay(retryAfter, send === 0 ? 250 : 1_000);
      if (delay === null || Date.now() + delay >= deadline) break;
      await pause(delay, signal, failure);
    }
    throw failure;
  }

  return {
    api,
    information: Object.freeze(structuredClone(information)),
    session: Object.freeze(structuredClone(current.data)),
    submitInvocation,
  };
}

function publicRoute(method: string, pathname: string): boolean {
  const actual = pathname.split("/");
  return controls.operations.some(operation => {
    if (operation.method !== method) return false;
    const expected = operation.path.split("/");
    return actual.length === expected.length && expected.every((part, index) =>
      /^\{[^{}]+\}$/.test(part) ? Boolean(actual[index]) : actual[index] === part);
  });
}

function identity(value: unknown, maximum: number): value is string {
  return typeof value === "string" && value.length > 0 && encoder.encode(value).length <= maximum &&
    value.trim() === value && !/[\u0000-\u001f\u007f]/u.test(value);
}

function featureList(value: unknown): value is string[] {
  return Array.isArray(value) && value.every(feature => identity(feature, 128)) &&
    new Set(value).size === value.length;
}

function problemError(response: Response, value: unknown, requestKey = ""): PMBrowserError {
  const problem = value as Partial<Problem> | null;
  const known = ["type", "title", "status", "detail", "code", "incident_id", "safe_retry_action", "outcome_reference"];
  if (response.headers.get("Content-Type")?.split(";", 1)[0]?.trim().toLowerCase() !== "application/problem+json" ||
      !problem || typeof problem !== "object" || Array.isArray(problem) || !safePublicFields(problem) ||
      problem.status !== response.status || response.status < 400 || response.status > 599 ||
      typeof problem.type !== "string" || typeof problem.title !== "string" || !problem.title.trim() ||
      encoder.encode(problem.title).length > 256 || typeof problem.detail !== "string" || !problem.detail.trim() ||
      encoder.encode(problem.detail).length > 2_048 || !identity(problem.code, 128) ||
      !identity(problem.incident_id, 512) || !identity(problem.safe_retry_action, 128) ||
      Object.keys(problem).some(name => !known.includes(name) && known.includes(name.toLowerCase()))) {
    return new PMBrowserError("malformed_problem", 0, "", "", requestKey);
  }
  try {
    new URL(problem.type);
  } catch {
    return new PMBrowserError("malformed_problem", 0, "", "", requestKey);
  }
  const rule = controls.problems.find(candidate => candidate.status === problem.status &&
    candidate.code === problem.code && candidate.safe_retry_action === problem.safe_retry_action);
  const outcome = problem.outcome_reference;
  if (!rule || ((rule.disposition === "outcome_unknown" || rule.disposition === "resolved_receipt_expired") && !outcome) ||
      (outcome != null && !isOutcomeReference(outcome))) {
    return new PMBrowserError("malformed_problem", 0, "", "", requestKey);
  }
  return new PMBrowserError(problem.code, problem.status, problem.incident_id, problem.safe_retry_action, requestKey);
}

function safePublicFields(value: unknown, depth = 0): boolean {
  if (depth > 128) return false;
  if (value === null || typeof value !== "object") return true;
  if (Array.isArray(value)) return value.every(child => safePublicFields(child, depth + 1));
  return Object.entries(value).every(([name, child]) =>
    !controls.forbidden_public_fields.includes(name.toLowerCase()) && safePublicFields(child, depth + 1));
}

function isOutcomeReference(value: unknown): value is components["schemas"]["SafeOutcomeReference"] {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const reference = value as Record<string, unknown>;
  return identity(reference.ref, 512) && typeof reference.kind === "string" &&
    ["request", "run", "connection", "membership", "service_account", "delegation"].includes(reference.kind);
}

function retryable(error: PMBrowserError): boolean {
  return (error.status === 429 || error.status === 503) &&
    ["after_backlog_progress", "same_key_after_dependency_recovery", "same_key_reconcile_only"].includes(error.safeRetryAction);
}

function retryDelay(value: string | null, minimum: number): number | null {
  if (!value?.trim()) return minimum;
  const raw = value.trim();
  const delay = /^\+?\d+$/.test(raw) ? Number(raw) * 1_000 : Date.parse(raw) - Date.now();
  return Number.isFinite(delay) && delay >= 0 ? Math.max(minimum, delay) : null;
}

function pause(milliseconds: number, signal: AbortSignal, failure: PMBrowserError): Promise<void> {
  const { promise, resolve, reject } = Promise.withResolvers<void>();
  if (signal.aborted) {
    reject(failure);
    return promise;
  }
  const abort = () => { clearTimeout(timer); reject(failure); };
  const timer = setTimeout(() => { signal.removeEventListener("abort", abort); resolve(); }, milliseconds);
  signal.addEventListener("abort", abort, { once: true });
  return promise;
}

async function persistRecovery(instance: string, workspace: string, principal: Principal, target: string, body: Invocation) {
  const scope = await byteDigest([instance, workspace, principal.kind, principal.principal_id,
    "invocation.submit", target].join("\u0000"));
  const record = {
    schema_version: "pm.client-recovery/v1",
    scope_fingerprint: scope,
    request_key: body.protocol.request_key,
    intent_fingerprint: body.protocol.intent_digest,
    typed_request_fingerprint: await canonicalDigest(body),
  };
  const key = `pm.client-recovery/v1/${(await byteDigest(`${scope}\u0000${body.protocol.request_key}`)).slice(7)}`;
  const document = JSON.stringify(record);
  try {
    await navigator.locks.request(key, { mode: "exclusive", signal: AbortSignal.timeout(30_000) }, () => {
      const previous = localStorage.getItem(key);
      if (previous !== null && previous !== document) throw new PMBrowserError("recovery_conflict");
      if (previous === null) localStorage.setItem(key, document);
    });
  } catch (error) {
    if (error instanceof PMBrowserError) throw error;
    throw new PMBrowserError("recovery_unavailable");
  }
}

/** Matches contract.CanonicalDigest, including Go string escaping and UTF-8 key order. */
export async function canonicalDigest(value: unknown): Promise<string> {
  return byteDigest(canonical(value, 0));
}

async function byteDigest(text: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", encoder.encode(text));
  return `sha256:${Array.from(new Uint8Array(digest), byte => byte.toString(16).padStart(2, "0")).join("")}`;
}

function canonical(value: unknown, depth: number): string {
  if (depth > 128) throw new PMBrowserError("invalid_typed_intent");
  if (value === null) return "null";
  if (typeof value === "boolean") return value ? "true" : "false";
  if (typeof value === "string") return quote(value);
  if (typeof value === "number") {
    // Exact provider numbers are typed string lexemes, never JavaScript numbers.
    if (!Number.isSafeInteger(value)) throw new PMBrowserError("invalid_typed_intent");
    if (value === 0) return "0";
    const text = String(value);
    const digits = text.replace(/0+$/, "");
    const exponent = text.length - digits.length;
    return exponent ? `${digits}e${exponent}` : digits;
  }
  if (Array.isArray(value)) return `[${value.map(item => canonical(item, depth + 1)).join(",")}]`;
  if (typeof value !== "object" || (Object.getPrototypeOf(value) !== Object.prototype && Object.getPrototypeOf(value) !== null)) {
    throw new PMBrowserError("invalid_typed_intent");
  }
  const object = value as Record<string, unknown>;
  return `{${Object.keys(object).sort(compareCodePoints).map(key => `${quote(key)}:${canonical(object[key], depth + 1)}`).join(",")}}`;
}

function quote(value: string): string {
  for (const character of value) {
    const code = character.codePointAt(0)!;
    if (code >= 0xd800 && code <= 0xdfff) throw new PMBrowserError("invalid_typed_intent");
  }
  return JSON.stringify(value).replace(/[<>&\u2028\u2029]/gu,
    character => `\\u${character.charCodeAt(0).toString(16).padStart(4, "0")}`);
}

function compareCodePoints(left: string, right: string): number {
  let a = 0;
  let b = 0;
  while (a < left.length && b < right.length) {
    const first = left.codePointAt(a)!;
    const second = right.codePointAt(b)!;
    if (first !== second) return first - second;
    a += first > 0xffff ? 2 : 1;
    b += second > 0xffff ? 2 : 1;
  }
  return (left.length - a) - (right.length - b);
}
