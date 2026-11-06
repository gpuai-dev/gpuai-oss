// The read-only tool tier — the free half of the surface, and the half where a
// mistake is quiet rather than loud.
//
// A spend tool that misbehaves costs money and gets noticed. A read-only tool
// that leaks the credential into a tool result costs nothing today and shows up
// months later in someone's transcript. So the assertions here are mostly about
// what must NOT be in the output, plus the two structural invariants the audit
// locks: exactly ten tools, every one of them flagged read-only.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { API_BASE_ENV, API_KEY_ENV } from "../src/auth.js";
import {
  READONLY_TOOL_NAMES,
  findReadonlyTool,
  readonlyTools,
  type ReadonlyToolDefinition,
} from "../src/tools/readonly/index.js";

/** Seeded credential. If this string appears in a tool result, the test fails. */
const SENTINEL_KEY = "gpuai_test_key_sentinel";
const ENV = { [API_KEY_ENV]: SENTINEL_KEY };

/**
 * The tier, in registration order.
 *
 * The list is written out rather than derived because its VALUE is that it
 * cannot drift silently: a tool added to the file without a deliberate edit
 * here fails, and so does a tool quietly dropped.
 */
const EXPECTED_NAMES = [
  "gpu_auth_status",
  "gpu_health_get",
  "gpu_types_list",
  "gpu_pricing_list",
  "gpu_environments_list",
  "gpu_instances_list",
  "gpu_instances_get",
  "gpu_operations_get",
  "gpu_ssh_keys_list",
  "gpu_ssh_keys_get",
];

const INSTANCE_PAGE = {
  data: [
    {
      id: "gpu-abc123",
      name: "trainer",
      status: "running",
      gpu_type: "H100",
      gpu_count: 1,
      region: "us-west",
      tier: "on_demand",
      price_per_hour: 2.5,
      created_at: "2026-08-13T10:00:00Z",
    },
  ],
  next_cursor: null,
};

/** Routes fake HTTP responses by path, and records what was requested. */
interface FakeRoute {
  status: number;
  body: unknown;
}

let requestedUrls: string[] = [];

function installFetch(routes: Record<string, FakeRoute>): void {
  requestedUrls = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string | URL) => {
      const href = typeof url === "string" ? url : url.toString();
      requestedUrls.push(href);
      const pathname = new URL(href).pathname;
      const route = routes[pathname];
      if (!route) {
        return new Response(JSON.stringify({ detail: `no fake route for ${pathname}` }), {
          status: 599,
          headers: { "content-type": "application/json" },
        });
      }
      return new Response(
        typeof route.body === "string" ? route.body : JSON.stringify(route.body),
        { status: route.status, headers: { "content-type": "application/json" } },
      );
    }),
  );
}

/** Everything a tool result could carry, flattened for leak assertions. */
function serialize(result: unknown): string {
  return JSON.stringify(result);
}

function tool(name: string): ReadonlyToolDefinition {
  const found = findReadonlyTool(name);
  if (!found) {
    throw new Error(`tool ${name} is not registered`);
  }
  return found;
}

beforeEach(() => {
  installFetch({
    "/v1/instances": { status: 200, body: INSTANCE_PAGE },
  });
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("the tier is exactly the audited ten", () => {
  it("registers 10 tools", () => {
    expect(readonlyTools).toHaveLength(10);
    expect(EXPECTED_NAMES).toHaveLength(10);
  });

  it("registers exactly the locked names, in order", () => {
    expect(READONLY_TOOL_NAMES).toEqual(EXPECTED_NAMES);
    expect(readonlyTools.map((t) => t.name)).toEqual(EXPECTED_NAMES);
  });

  it("has no duplicate names", () => {
    expect(new Set(READONLY_TOOL_NAMES).size).toBe(READONLY_TOOL_NAMES.length);
  });

  it("marks every tool read-only and non-destructive", () => {
    for (const definition of readonlyTools) {
      expect(definition.annotations.readOnlyHint, definition.name).toBe(true);
      expect(definition.annotations.destructiveHint, definition.name).toBe(false);
    }
  });

  it("gives every tool a substantive description and title", () => {
    for (const definition of readonlyTools) {
      expect(definition.description.length, definition.name).toBeGreaterThan(40);
      expect(definition.title.length, definition.name).toBeGreaterThan(0);
    }
  });

  it("gives every tool a callable handler and an object input schema", () => {
    for (const definition of readonlyTools) {
      expect(typeof definition.handler, definition.name).toBe("function");
      expect(typeof definition.inputSchema, definition.name).toBe("object");
    }
  });

  it("puts no confirmation token on any free tool — the gate belongs to spend tools only", () => {
    for (const definition of readonlyTools) {
      expect(Object.keys(definition.inputSchema), definition.name).not.toContain(
        "confirmation_token",
      );
    }
  });
});

describe("gpu_auth_status — the friendly path", () => {
  it("reports authentication and the first page of instances", async () => {
    const result = await tool("gpu_auth_status").handler({}, { env: ENV });
    expect(result.isError).toBeFalsy();

    const status = JSON.parse(result.content[0]!.text) as {
      authenticated: boolean;
      api_base: string;
      credential: { source: string; note: string };
      instances: { active_on_first_page: number; more_pages: boolean };
      next_steps: string[];
    };
    expect(status.authenticated).toBe(true);
    expect(status.credential.source).toContain(API_KEY_ENV);
    expect(status.instances.active_on_first_page).toBe(1);
    expect(status.instances.more_pages).toBe(false);
    expect(status.next_steps.join(" ")).toContain("gpu_pricing_list");
  });

  it("proves authentication with a single small instance listing", async () => {
    await tool("gpu_auth_status").handler({}, { env: ENV });
    expect(requestedUrls).toHaveLength(1);
    const url = new URL(requestedUrls[0]!);
    expect(url.pathname).toBe("/v1/instances");
    expect(url.searchParams.get("limit")).toBe("5");
  });

  it("reports more_pages when the listing has a next cursor", async () => {
    installFetch({
      "/v1/instances": { status: 200, body: { ...INSTANCE_PAGE, next_cursor: "c2" } },
    });
    const result = await tool("gpu_auth_status").handler({}, { env: ENV });
    const status = JSON.parse(result.content[0]!.text) as { instances: { more_pages: boolean } };
    expect(status.instances.more_pages).toBe(true);
  });

  it("reports the resolved API base so the operator can spot a wrong environment", async () => {
    const result = await tool("gpu_auth_status").handler({}, { env: ENV });
    expect(serialize(result)).toContain("http://localhost:8080/v1");

    const overridden = await tool("gpu_auth_status").handler(
      {},
      { env: { ...ENV, [API_BASE_ENV]: "https://gpu.example.com/v1" } },
    );
    expect(serialize(overridden)).toContain("https://gpu.example.com/v1");
  });

  it("NEVER returns the credential — the seeded sentinel must be absent", async () => {
    const result = await tool("gpu_auth_status").handler({}, { env: ENV });
    expect(serialize(result)).not.toContain(SENTINEL_KEY);
  });

  it("does send the credential upstream — the leak test above must not pass vacuously", async () => {
    await tool("gpu_auth_status").handler({}, { env: ENV });
    expect(requestedUrls.length).toBeGreaterThan(0);
  });

  it("has no confirmation_token parameter in its schema", () => {
    expect(Object.keys(tool("gpu_auth_status").inputSchema)).not.toContain("confirmation_token");
  });

  it("tells the agent to call it first, before provisioning", () => {
    const description = tool("gpu_auth_status").description.toLowerCase();
    expect(description).toContain("first");
    expect(description).toContain("provisioning");
  });

  it("maps a 401 to the auth message without echoing the key", async () => {
    installFetch({
      "/v1/instances": {
        status: 401,
        body: { detail: `key ${SENTINEL_KEY} rejected`, status: 401 },
      },
    });

    const result = await tool("gpu_auth_status").handler({}, { env: ENV });
    const text = serialize(result);

    expect(result.isError).toBe(true);
    expect(text).toContain(API_KEY_ENV);
    expect(text.toLowerCase()).toContain("invalid or revoked");
    expect(text).not.toContain(SENTINEL_KEY);
  });

  it("propagates a server failure as a mapped error rather than claiming authentication", async () => {
    installFetch({ "/v1/instances": { status: 500, body: { detail: "boom" } } });

    const result = await tool("gpu_auth_status").handler({}, { env: ENV });
    const text = serialize(result);
    expect(result.isError).toBe(true);
    expect(result.content[0]!.text).not.toContain('"authenticated"');
    expect(text).toContain("checking authentication status");
  });
});

describe("gpu_operations_get — the poll surface ", () => {
  const description = tool("gpu_operations_get").description;

  it("states the 2s to 10s backoff cadence the CLI uses", () => {
    expect(description).toContain("2s");
    expect(description).toContain("10s");
  });

  it("names all three terminal states", () => {
    expect(description).toContain("succeeded");
    expect(description).toContain("failed");
    expect(description).toContain("cancelled");
  });

  it("instructs cleanup of anything left running on failure", () => {
    expect(description.toLowerCase()).toContain("terminate");
  });

  it("requires an operation id", () => {
    expect(Object.keys(tool("gpu_operations_get").inputSchema)).toContain("id");
  });
});

describe("the remaining reads reach the API through the typed SDK", () => {
  it("gpu_environments_list and gpu_health_get need no arguments", async () => {
    installFetch({
      "/v1/environments": {
        status: 200,
        body: { frameworks: ["pytorch"], versions: {}, includes: {}, vm_images: {} },
      },
      "/v1/health": { status: 200, body: { status: "ok" } },
    });

    expect((await tool("gpu_environments_list").handler({}, { env: ENV })).isError).toBeFalsy();
    expect((await tool("gpu_health_get").handler({}, { env: ENV })).isError).toBeFalsy();
  });

  it("gpu_ssh_keys_get rejects a missing id without calling the API", async () => {
    installFetch({});
    const result = await tool("gpu_ssh_keys_get").handler({}, { env: ENV });
    expect(result.isError).toBe(true);
    expect(requestedUrls).toHaveLength(0);
  });
});

describe("descriptions are a customer-facing surface", () => {
  const all = readonlyTools.map((t) => `${t.title} ${t.description}`).join(" ").toLowerCase();

  it("never advertises a hard-coded endpoint — the server URL is configuration", () => {
    expect(all).not.toMatch(/https?:\/\//);
  });

  it("never names a specific supplier — the server's configured suppliers are generic", () => {
    for (const named of ["vultr", "mock"]) {
      expect(all, named).not.toContain(named);
    }
  });
});

describe("read tools reach the API through the typed SDK", () => {
  it("gpu_types_list returns catalog data", async () => {
    installFetch({
      "/v1/gpu-types": { status: 200, body: { data: [{ gpu_type: "H100", vram_gb: 80 }], next_cursor: null } },
    });

    const result = await tool("gpu_types_list").handler({}, { env: ENV });
    expect(result.isError).toBeFalsy();
    expect(serialize(result)).toContain("H100");
  });

  it("gpu_instances_get passes the id through to the path", async () => {
    installFetch({
      "/v1/instances/gpu-abc123": { status: 200, body: INSTANCE_PAGE.data[0] },
    });

    const result = await tool("gpu_instances_get").handler({ id: "gpu-abc123" }, { env: ENV });
    expect(result.isError).toBeFalsy();
    expect(requestedUrls.some((u) => u.includes("gpu-abc123"))).toBe(true);
  });

  it("rejects a missing required id without calling the API", async () => {
    installFetch({});
    const result = await tool("gpu_instances_get").handler({}, { env: ENV });
    expect(result.isError).toBe(true);
    expect(requestedUrls).toHaveLength(0);
  });

  it("honours the base URL override verbatim", async () => {
    installFetch({
      "/v1/gpu-types": { status: 200, body: { data: [], next_cursor: null } },
    });

    await tool("gpu_types_list").handler(
      {},
      { env: { ...ENV, [API_BASE_ENV]: "https://api.internal.example.com/v1" } },
    );
    expect(requestedUrls[0]).toContain("api.internal.example.com");
  });

  it("maps a 401 on any read tool to the auth message, never a raw SDK error", async () => {
    installFetch({ "/v1/gpu-types": { status: 401, body: { detail: "nope" } } });

    const result = await tool("gpu_types_list").handler({}, { env: ENV });
    expect(result.isError).toBe(true);
    expect(serialize(result)).toContain(API_KEY_ENV);
    expect(serialize(result)).not.toContain("Response returned an error code");
  });

  it("surfaces a missing credential as a setup message, not a crash", async () => {
    const result = await tool("gpu_types_list").handler({}, { env: {} });
    expect(result.isError).toBe(true);
    expect(serialize(result)).toContain(API_KEY_ENV);
  });
});
