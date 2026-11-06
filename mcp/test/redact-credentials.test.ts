// Instance web-console credentials must not reach model context.
//
// The finding: `gpu_instances_get` and `gpu_operations_get` put
// the per-instance HTTP Basic Auth login (`app_user` / `app_password`, and the
// operation-result spelling `app_basic_auth_user` / `app_basic_auth_pass`) into
// tool results in cleartext. The API returning them is by design —
// `gpu instances open` re-reads the credential — but a tool result becomes
// part of an agent transcript that outlives the process and is outside the
// operator's control. So the MCP layer is where the credential stops.
//
// Two levels of assertion, and both are needed:
//
//   - UNIT, on sanitizeCredentialFields itself: the key set is exact-match, so
//     `app_url` (which the user needs) cannot be caught by a pattern meant for
//     `app_password`, and an instance with no console must not GAIN a
//     redaction placeholder.
//   - TIER, through the real handlers over a faked fetch: a sentinel credential
//     is served by the API and must appear in no tool result — paired with a
//     presence assertion on the URL and the redaction notice, so an SDK model
//     that silently dropped the field could not make the absence pass for the
//     wrong reason.
import { afterEach, describe, expect, it, vi } from "vitest";

import { API_KEY_ENV } from "../src/auth.js";
import { CREDENTIAL_REDACTION_NOTICE, sanitizeCredentialFields } from "../src/errors.js";
import { findReadonlyTool } from "../src/tools/readonly/index.js";
import { findSpendTool } from "../src/tools/spend/index.js";

/** Values that must never appear in any tool result. */
const SENTINEL_PASSWORD = "SENTINEL-app-password-x7f3a9c";
const SENTINEL_USER = "SENTINEL-app-user-gpuweb";

const APP_URL = "https://gpu-abc123.apps.example.com";
const TERMINAL_URL = "https://gpu-abc123-term.apps.example.com";

const ENV = { [API_KEY_ENV]: "gpuai_test_key_sentinel" };
const OPERATION_ID = "11111111-2222-3333-4444-555555555555";

/**
 * Wire-format instance, as the API sends it: a running instance with a web
 * console. snake_case, because that is what the SDK parses into camelCase
 * model fields — the fixture must exercise that mapping rather than assume it.
 */
const INSTANCE = {
  id: "gpu-abc123",
  name: "comfy",
  status: "running",
  gpu_type: "h100_sxm",
  gpu_count: 1,
  region: "us-west-2",
  tier: "on_demand",
  price_per_hour: 2.5,
  created_at: "2026-08-17T10:00:00Z",
  connection: {
    hostname: "203.0.113.7",
    port: 22,
    ssh_command: "ssh ubuntu@203.0.113.7 -p 22",
    app_url: APP_URL,
    terminal_url: TERMINAL_URL,
    app_user: SENTINEL_USER,
    app_password: SENTINEL_PASSWORD,
  },
};

/** A succeeded create: the operation result carries the basic-auth spelling. */
const OPERATION = {
  operation_id: OPERATION_ID,
  kind: "instance.create",
  state: "succeeded",
  resource_id: "gpu-abc123",
  created_at: "2026-08-17T10:00:00Z",
  completed_at: "2026-08-17T10:04:00Z",
  result: {
    app_url: APP_URL,
    terminal_url: TERMINAL_URL,
    app_basic_auth_user: SENTINEL_USER,
    app_basic_auth_pass: SENTINEL_PASSWORD,
  },
};

const ROUTES: Record<string, unknown> = {
  "/v1/instances/gpu-abc123": INSTANCE,
  "/v1/instances": { data: [INSTANCE], next_cursor: null },
  [`/v1/operations/${OPERATION_ID}`]: OPERATION,
};

let requestedUrls: string[] = [];

/** Same fake-fetch shape as auth-status.test.ts: routed by pathname, recorded. */
function installFetch(): void {
  requestedUrls = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string | URL) => {
      const href = typeof url === "string" ? url : url.toString();
      requestedUrls.push(href);
      const body = ROUTES[new URL(href).pathname];
      if (body === undefined) {
        return new Response(JSON.stringify({ detail: `no fake route for ${href}` }), {
          status: 599,
          headers: { "content-type": "application/json" },
        });
      }
      return new Response(JSON.stringify(body), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    }),
  );
}

function serialize(result: unknown): string {
  return JSON.stringify(result);
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

/**
 * The four assertions every tier test makes, as one helper.
 *
 * The absence pair is the finding; the presence pair is what stops the absence
 * from passing for the wrong reason. If the SDK dropped the field, or the fake
 * route stopped matching, the URLs and the notice would go missing too.
 */
function expectRedactedButUsable(text: string): void {
  expect(text).not.toContain(SENTINEL_PASSWORD);
  expect(text).not.toContain(SENTINEL_USER);
  expect(text).toContain(APP_URL);
  expect(text).toContain(TERMINAL_URL);
  expect(text).toContain(CREDENTIAL_REDACTION_NOTICE);
}

describe("no tool result carries an instance web-console credential", () => {
  it("gpu_instances_get redacts the login and keeps the links", async () => {
    installFetch();
    const result = await findReadonlyTool("gpu_instances_get")!.handler(
      { id: "gpu-abc123" },
      { env: ENV },
    );

    expect(result.isError).toBeFalsy();
    expectRedactedButUsable(serialize(result));
    // Non-vacuity: the credential really was served and really was read.
    expect(requestedUrls.some((u) => u.includes("/v1/instances/gpu-abc123"))).toBe(true);
  });

  it("gpu_instances_list redacts the login on every row", async () => {
    installFetch();
    const result = await findReadonlyTool("gpu_instances_list")!.handler({}, { env: ENV });

    expect(result.isError).toBeFalsy();
    expectRedactedButUsable(serialize(result));
    expect(requestedUrls.some((u) => u.includes("/v1/instances"))).toBe(true);
  });

  it("gpu_operations_get redacts the basic-auth spelling on a succeeded create", async () => {
    installFetch();
    const result = await findReadonlyTool("gpu_operations_get")!.handler(
      { id: OPERATION_ID },
      { env: ENV },
    );

    expect(result.isError).toBeFalsy();
    expectRedactedButUsable(serialize(result));
    expect(requestedUrls.some((u) => u.includes(OPERATION_ID))).toBe(true);
  });

  it("the spend tier's quote path is sanitised too", async () => {
    installFetch();
    // No elicitation host, so this is the first call: a quote, built from the
    // same instance fixture. Today's resolvedParams hand-pick their fields and
    // carry no credential — this test is what keeps that true if someone later
    // spreads the whole instance in.
    const result = await findSpendTool("gpu_instances_terminate")!.handler(
      { instance_id: "gpu-abc123" },
      { env: ENV },
    );

    const text = serialize(result);
    expect(result.isError).toBeFalsy();
    expect(text).not.toContain(SENTINEL_PASSWORD);
    expect(text).not.toContain(SENTINEL_USER);
    // Non-vacuity: the fixture instance was fetched and priced.
    expect(text).toContain("gpu-abc123");
    expect(text).toContain("2.50");
    expect(requestedUrls.some((u) => u.includes("/v1/instances/gpu-abc123"))).toBe(true);
  });
});

describe("sanitizeCredentialFields — the key set is exact-match", () => {
  it("redacts appPassword and leaves appUrl alone", () => {
    const out = sanitizeCredentialFields({
      connection: { appPassword: SENTINEL_PASSWORD, appUrl: APP_URL },
    }) as { connection: Record<string, unknown> };

    expect(out.connection.appPassword).toBe(CREDENTIAL_REDACTION_NOTICE);
    expect(out.connection.appUrl).toBe(APP_URL);
  });

  it("redacts every spelling of the credential keys, nested two levels deep", () => {
    const out = sanitizeCredentialFields({
      outer: {
        connection: {
          appPassword: SENTINEL_PASSWORD,
          app_password: SENTINEL_PASSWORD,
          appBasicAuthPass: SENTINEL_PASSWORD,
          app_basic_auth_pass: SENTINEL_PASSWORD,
          appUser: SENTINEL_USER,
          app_user: SENTINEL_USER,
          appBasicAuthUser: SENTINEL_USER,
          app_basic_auth_user: SENTINEL_USER,
        },
      },
    });

    const text = JSON.stringify(out);
    expect(text).not.toContain(SENTINEL_PASSWORD);
    expect(text).not.toContain(SENTINEL_USER);
    expect(text).toContain(CREDENTIAL_REDACTION_NOTICE);
  });

  it("redacts inside arrays of objects", () => {
    const out = sanitizeCredentialFields({
      data: [
        { id: "a", connection: { app_password: SENTINEL_PASSWORD, app_url: APP_URL } },
        { id: "b", connection: { app_password: SENTINEL_PASSWORD } },
      ],
    });

    const text = JSON.stringify(out);
    expect(text).not.toContain(SENTINEL_PASSWORD);
    expect(text).toContain(APP_URL);
    expect(text).toContain("\"id\":\"b\"");
  });

  it("leaves the fields the user needs untouched", () => {
    const input = {
      app_url: APP_URL,
      appUrl: APP_URL,
      terminal_url: TERMINAL_URL,
      terminalUrl: TERMINAL_URL,
      ssh_user: "ubuntu",
      sshUser: "ubuntu",
      ssh_command: "ssh ubuntu@1.2.3.4 -p 22",
    };

    expect(sanitizeCredentialFields(input)).toEqual(input);
  });

  it("injects no placeholder into a connection that has no console credential", () => {
    const input = {
      connection: { hostname: "1.2.3.4", port: 22, ssh_command: "ssh ubuntu@1.2.3.4" },
    };

    // Deep equality, not a substring check: gaining a redaction notice would
    // tell the agent a credential exists where none does.
    expect(sanitizeCredentialFields(input)).toEqual(input);
    expect(JSON.stringify(sanitizeCredentialFields(input))).not.toContain("redacted");
  });

  it("replaces only string values — null and undefined stay as they are", () => {
    const out = sanitizeCredentialFields({
      app_password: null,
      appPassword: undefined,
      app_user: 0,
    }) as Record<string, unknown>;

    expect(out.app_password).toBeNull();
    expect(out.appPassword).toBeUndefined();
    expect(out.app_user).toBe(0);
  });

  it("does not mutate its input", () => {
    const input = { connection: { app_password: SENTINEL_PASSWORD } };
    sanitizeCredentialFields(input);
    expect(input.connection.app_password).toBe(SENTINEL_PASSWORD);
  });

  it("passes primitives and null through unchanged", () => {
    expect(sanitizeCredentialFields("plain")).toBe("plain");
    expect(sanitizeCredentialFields(42)).toBe(42);
    expect(sanitizeCredentialFields(null)).toBeNull();
    expect(sanitizeCredentialFields(undefined)).toBeUndefined();
  });

  it("uses a notice that tells the user where the credential actually lives", () => {
    expect(CREDENTIAL_REDACTION_NOTICE).toContain("redacted");
    expect(CREDENTIAL_REDACTION_NOTICE).toContain("gpu instances open");
  });
});
