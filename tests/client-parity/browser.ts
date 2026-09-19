import { test, expect, type Page } from "@playwright/test";
import { lstatSync, readFileSync } from "node:fs";
import { isAbsolute } from "node:path";
import type { BrowserClient as Client, BrowserAdmission as Admission, InvocationInput } from "../../sdk/browser/client.js";
import type { paths as PublicPaths } from "../../sdk/browser/schema.js";

// @ts-expect-error A private credential route must never become a generated public path.
const privateAdminPath: keyof PublicPaths = "/admin/v1/workspaces/{workspace_id}/connections/{connection_id}/credentials";
void privateAdminPath;

type Submission = { status: number; admission?: Admission; code?: string; request_key?: string };
interface Configuration {
  instance_id: string;
  workspace_id: string;
  operation_revision: string;
  request_key: string;
  input_field: string;
  input: InvocationInput;
  omit_csrf?: boolean;
}
interface FixtureInput extends Configuration {
  base_url: string;
  bootstrap_nonce: string;
  foreign_workspace_id: string;
  first_text: string;
  changed_text: string;
  expected_run_ref: string;
  expected_intent_digest: string;
  canonical_cases: Array<{ value: unknown; digest: string }>;
}
interface Observation {
  admission_calls: number;
  cancel_calls: number;
  effect_calls: number;
}

declare global {
  interface Window {
    parity: {
      connect(configuration: Configuration): Promise<{ instance_id: string; principal: Client["session"]["principal"] }>;
      submit(text: string, key?: string): Promise<Submission>;
      canonicalDigest(value: unknown): Promise<string>;
      api(): Client["api"];
    };
  }
}

const inputPath = process.env.PM_PARITY_INPUT;
if (!inputPath || !isAbsolute(inputPath)) throw new Error("Go-owned parity input is required");
const inputStat = lstatSync(inputPath);
if (!inputStat.isFile() || inputStat.isSymbolicLink() || inputStat.size > 128 << 10 || (inputStat.mode & 0o077) !== 0) {
  throw new Error("invalid owned parity input");
}
const fixture = JSON.parse(readFileSync(inputPath, "utf8")) as FixtureInput;
const origin = new URL(fixture.base_url);
if (origin.protocol !== "https:" || !["127.0.0.1", "[::1]"].includes(origin.hostname) ||
    origin.username || origin.password || origin.search || origin.hash || origin.pathname !== "/") {
  throw new Error("parity requires the owned loopback TLS origin");
}

async function connect(page: Page, actor = "alpha", overrides: Partial<Configuration> = {}) {
  await page.goto(`${fixture.base_url}/__pm_fixture__/`);
  const status = await page.evaluate(async ({ nonce, actor }) => {
    const response = await fetch("/__pm_fixture__/bootstrap", {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ nonce, actor }),
    });
    return response.status;
  }, { nonce: fixture.bootstrap_nonce, actor });
  expect(status).toBe(204);
  return page.evaluate(configuration => window.parity.connect(configuration), {
    instance_id: fixture.instance_id,
    workspace_id: fixture.workspace_id,
    operation_revision: fixture.operation_revision,
    request_key: fixture.request_key,
    input_field: fixture.input_field,
    input: fixture.input,
    ...overrides,
  });
}

async function observation(page: Page): Promise<Observation> {
  return page.evaluate(async () => {
    const response = await fetch("/__pm_fixture__/observations", { credentials: "same-origin" });
    if (!response.ok) throw new Error("owned observation unavailable");
    return response.json();
  });
}

async function extendNextAdmissionResponse(page: Page, extension: Record<string, unknown>) {
  await page.evaluate(fields => {
    const originalFetch: typeof window.fetch = window.fetch.bind(window);
    window.fetch = async (input, init) => {
      const response = await originalFetch(input, init);
      const requestURL = new URL(input instanceof Request ? input.url : String(input), location.href);
      if (response.status !== 202 || !requestURL.pathname.endsWith("/invocations")) return response;
      window.fetch = originalFetch;
      const body = await response.json() as Record<string, unknown>;
      const headers = new Headers(response.headers);
      headers.delete("Content-Length");
      return new Response(JSON.stringify({ ...body, ...fields }), {
        status: response.status,
        statusText: response.statusText,
        headers,
      });
    };
  }, extension);
}

test("pm-browser-client-parity-E01-C01 different workspace remains isolated", async ({ page }) => {
  await connect(page, "alpha", { workspace_id: fixture.foreign_workspace_id });
  const before = await observation(page);
  const result = await page.evaluate(text => window.parity.submit(text), fixture.first_text);
  expect(result.status).toBe(404);
  expect(result.admission).toBeUndefined();
  expect(await observation(page)).toEqual(before);
});

test("pm-browser-client-parity-E01-C02 public SDK has no admin secret route", async ({ page }) => {
  await connect(page);
  const before = await observation(page);
  const result = await page.evaluate(async () => {
    try {
      const api = window.parity.api() as unknown as {
        POST(path: string, options: Record<string, never>): Promise<unknown>;
      };
      await api.POST("/admin/v1/workspaces/{workspace_id}/connections/{connection_id}/credentials", {});
      return "request_was_sent";
    } catch (error) {
      return error instanceof Error && "code" in error ? String(error.code) : "unexpected_failure";
    }
  });
  expect(result).toBe("unsafe_client_request");
  expect(await observation(page)).toEqual(before);
});

test("pm-browser-client-parity-E01-C03 safe additive admission retains one committed RunID", async ({ page }, testInfo) => {
  await connect(page);
  await extendNextAdmissionResponse(page, { future_context: { safe_label: "retained" } });
  await page.getByLabel("Text", { exact: true }).fill(fixture.first_text);
  await page.getByRole("button", { name: "Submit typed invocation" }).click();
  await expect(page.locator("#status")).toHaveText("Durably accepted");
  const admission = JSON.parse(await page.locator("#result").innerText()) as Admission & {
    future_context: { safe_label: string };
  };
  expect(admission.run_ref).toBe(fixture.expected_run_ref);
  expect(admission.intent_digest).toBe(fixture.expected_intent_digest);
  expect(admission.request_key).toBe(fixture.request_key);
  expect(admission.state).toBe("accepted");
  expect(admission.replay).toBe(true);
  expect(admission.future_context).toEqual({ safe_label: "retained" });
  await extendNextAdmissionResponse(page, {
    future_context: { provider_secret: "synthetic-must-be-refused" },
  });
  const unsafe = await page.evaluate(
    ({ text, key }) => window.parity.submit(text, key),
    { text: fixture.first_text, key: fixture.request_key },
  );
  expect(unsafe).toMatchObject({
    status: 0,
    code: "malformed_admission_response",
    request_key: fixture.request_key,
  });
  const observed = await page.evaluate(async ({ workspace, run }) => {
    const response = await window.parity.api().GET("/v1/workspaces/{workspace_id}/runs/{run_id}", {
      params: { path: { workspace_id: workspace, run_id: run } },
    });
    return { status: response.response.status, data: response.data };
  }, { workspace: fixture.workspace_id, run: fixture.expected_run_ref });
  expect(observed.status).toBe(200);
  expect(observed.data?.run_ref).toBe(fixture.expected_run_ref);
  await page.screenshot({ path: testInfo.outputPath("typed-admission.png"), fullPage: true });
});

test("pm-browser-client-parity-E01-C04 changed input keeps original intent and returns 409", async ({ page }) => {
  // Playwright gives each case a fresh client journal; the server must detect the
  // conflict even when this browser has not seen the original client's key.
  await connect(page);
  const before = await observation(page);
  const result = await page.evaluate(text => window.parity.submit(text), fixture.changed_text);
  expect(result.status).toBe(409);
  expect(result.admission).toBeUndefined();
  const after = await observation(page);
  expect(after.effect_calls).toBe(before.effect_calls);
});

test("pm-browser-client-parity-E01-C05 real browser cookie is HttpOnly Secure SameSite", async ({ page, context }) => {
  await connect(page);
  const cookie = (await context.cookies(fixture.base_url)).find(value => value.name === "__Host-pm_session");
  expect(cookie && { httpOnly: cookie.httpOnly, secure: cookie.secure, sameSite: cookie.sameSite, path: cookie.path })
    .toEqual({ httpOnly: true, secure: true, sameSite: "Lax", path: "/" });
  const readable = await page.evaluate(() => ({
    cookies: document.cookie.split(";").map(value => value.trim().split("=", 1)[0]),
    storageKeys: [...Object.keys(localStorage), ...Object.keys(sessionStorage)],
  }));
  expect(readable.cookies).not.toContain("__Host-pm_session");
  expect(readable.storageKeys).toEqual([]);
  const response = await page.evaluate(async () => {
    const session = await window.parity.api().GET("/v1/sessions/me");
    return { status: session.response.status, transport: session.data?.transport };
  });
  expect(response).toEqual({ status: 200, transport: "browser_session" });
});

for (const fault of ["origin", "host"] as const) {
  test(`pm-browser-client-parity-E01-C06 wrong ${fault} is independently denied`, async ({ page }) => {
    await connect(page);
    const result = await page.evaluate(async ({ fault, fixture }) => {
      // The positive control uses identical bytes and the same owned forwarding
      // path. Each negative changes only one browser-forbidden authority field.
      const payload: InvocationInput = {
        ...fixture.input,
        input: {
          presence: "value",
          type: "object",
          fields: {
            [fixture.input_field]: { presence: "value", type: "string", text: fixture.first_text },
          },
        },
      };
      const intentDigest = await window.parity.canonicalDigest({
        expected_revision: fixture.operation_revision,
        operation: "invocation.submit",
        payload,
        target: `${payload.connection_ref}/${payload.operation_ref}`,
        workspace: fixture.workspace_id,
      });
      const body = JSON.stringify({
        ...payload,
        protocol: {
          wire_version: "pm.api/v1",
          required_features: [],
          request_key: fixture.request_key,
          expected_revision: fixture.operation_revision,
          intent_digest: intentDigest,
        },
      });
      const csrf = document.cookie.split("; ").find(value => value.startsWith("__Host-pm_csrf="))?.split("=")[1] ?? "";
      const send = async (selected: string) => {
        const response = await fetch(`/__pm_fixture__/inbound-fault/${selected}`, {
          method: "POST",
          credentials: "same-origin",
          headers: {
            "Content-Type": "application/json",
            "Idempotency-Key": fixture.request_key,
            "X-CSRF-Token": csrf,
          },
          body,
        });
        return { status: response.status, body: await response.json() };
      };
      const observe = async () => (await fetch("/__pm_fixture__/observations", { credentials: "same-origin" })).json() as Promise<Observation>;
      const control = await send("none");
      const before = await observe();
      const denied = await send(fault);
      return { control, denied, before, after: await observe() };
    }, { fault, fixture: {
      input: fixture.input,
      input_field: fixture.input_field,
      first_text: fixture.first_text,
      request_key: fixture.request_key,
      operation_revision: fixture.operation_revision,
      workspace_id: fixture.workspace_id,
    } });
    expect(result.control).toMatchObject({ status: 202, body: { run_ref: fixture.expected_run_ref, replay: true } });
    expect(result.denied).toMatchObject({ status: 400, body: { code: "invalid_request" } });
    expect(result.after).toEqual(result.before);
  });
}

test("pm-browser-client-parity-E01-C07 missing CSRF cannot reach admission", async ({ page }) => {
  await connect(page, "alpha", { omit_csrf: true });
  const before = await observation(page);
  const result = await page.evaluate(text => window.parity.submit(text, "parity-missing-csrf"), fixture.first_text);
  expect(result.status).toBe(403);
  expect(await observation(page)).toEqual(before);
});

test("pm-browser-client-parity-E01-C08 AbortSignal closes only viewing", async ({ page }) => {
  await connect(page);
  const before = await observation(page);
  const observed = await page.evaluate(async ({ workspace, run }) => {
    const controller = new AbortController();
    const events = await window.parity.api().GET("/v1/workspaces/{workspace_id}/runs/{run_id}/events", {
      params: { path: { workspace_id: workspace, run_id: run } },
      parseAs: "stream",
      signal: controller.signal,
    });
    if (!events.data) throw new Error("event stream unavailable");
    const reader = events.data.getReader();
    let aborted = false;
    try {
      const first = await reader.read();
      if (first.done || !first.value?.byteLength) throw new Error("no committed stream snapshot");
      controller.abort();
      await reader.read();
    } catch (error) {
      aborted = error instanceof DOMException && error.name === "AbortError";
    } finally {
      reader.releaseLock();
    }
    const state = await window.parity.api().GET("/v1/workspaces/{workspace_id}/runs/{run_id}", {
      params: { path: { workspace_id: workspace, run_id: run } },
    });
    return { aborted, status: state.response.status, data: state.data };
  }, { workspace: fixture.workspace_id, run: fixture.expected_run_ref });
  expect(observed.aborted).toBe(true);
  expect(observed.status).toBe(200);
  expect(observed.data?.state).toBe("accepted");
  expect((await observation(page)).cancel_calls).toBe(before.cancel_calls);
});

test("pm-browser-client-parity-E01-C09 beta principal cannot disclose alpha", async ({ page }) => {
  await connect(page, "beta");
  const before = await observation(page);
  const result = await page.evaluate(text => window.parity.submit(text, "parity-beta"), fixture.first_text);
  expect(result.status).toBe(404);
  expect(result.admission).toBeUndefined();
  expect(await observation(page)).toEqual(before);
});

test("browser canonical intent preserves exact values and Go key ordering", async ({ page }) => {
  await connect(page);
  for (const item of fixture.canonical_cases) {
    const actual = await page.evaluate(value => window.parity.canonicalDigest(value), item.value);
    expect(actual).toBe(item.digest);
  }
});
