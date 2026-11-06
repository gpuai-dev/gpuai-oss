// The schema regression suite — what a CLIENT actually sees.
//
// Every other test in this package reaches into the tool definitions directly.
// This one does not: it stands the real server up over an in-memory transport,
// connects a real MCP client, and asserts against the `tools/list` response.
// The distinction is the whole point of the file. A tool definition can be
// perfect and still be advertised wrong — a registry that drops a field, a zod
// shape that converts to a JSON Schema with `gpu_count` absent from `required`,
// a spend tool registered with the read-only annotations. None of that is
// visible from the definition side, and all of it is a guardrail regression:
// a schema that makes count optional is a schema that lets an agent omit it.
//
// So the assertions here are deliberately about the WIRE representation —
// `required` arrays, `properties` keys, `annotations` — not about the zod
// objects that produced them.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";

import { API_KEY_ENV } from "../src/auth.js";
import { _resetTokenStoreForTests } from "../src/guardrail/handshake.js";
import { ALL_TOOL_NAMES, registerAllTools } from "../src/tools/index.js";
import { READONLY_TOOL_NAMES } from "../src/tools/readonly/index.js";
import { SPEND_TOOL_NAMES } from "../src/tools/spend/index.js";
import { WRITE_TOOL_NAMES } from "../src/tools/write/index.js";
import { buildServer, createServer } from "../src/index.js";

const SENTINEL_KEY = "gpuai_test_key_sentinel";

/** The tool that provisions hardware. It must name type AND count. */
const CAPACITY_TOOLS = ["gpu_instances_create"];

/** Everything that RESERVES CAPACITY, and so must name a GPU explicitly. */
const SPEND_CAPABLE = ["gpu_instances_create"];

interface AdvertisedTool {
  name: string;
  title?: string;
  description?: string;
  inputSchema: {
    type: string;
    properties?: Record<string, Record<string, unknown>>;
    required?: string[];
  };
  annotations?: Record<string, unknown>;
}

let client: Client;
let advertised: AdvertisedTool[];

async function connect(): Promise<void> {
  const server = buildServer();
  const [clientTransport, serverTransport] = InMemoryTransport.createLinkedPair();
  client = new Client({ name: "schema-regression", version: "0" });
  await Promise.all([server.connect(serverTransport), client.connect(clientTransport)]);
  const response = await client.listTools();
  advertised = response.tools as unknown as AdvertisedTool[];
}

function find(name: string): AdvertisedTool {
  const found = advertised.find((t) => t.name === name);
  if (!found) {
    throw new Error(`${name} is not advertised by the server`);
  }
  return found;
}

function requiredOf(name: string): string[] {
  return find(name).inputSchema.required ?? [];
}

function propertiesOf(name: string): Record<string, Record<string, unknown>> {
  return find(name).inputSchema.properties ?? {};
}

beforeEach(async () => {
  _resetTokenStoreForTests();
  process.env[API_KEY_ENV] = SENTINEL_KEY;
  await connect();
});

afterEach(async () => {
  delete process.env[API_KEY_ENV];
  await client.close();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

// ---------------------------------------------------------------------------
// 1. The surface is exactly fifteen.
//    Rots: a tool silently fails to register, or one is added without passing
//    through the tier files the audit governs.
// ---------------------------------------------------------------------------

describe("the assembled server advertises exactly the three tiers", () => {
  it("lists 15 tools", () => {
    expect(advertised).toHaveLength(15);
  });

  it("lists the 10 read-only, 3 write and 2 spend tools in registration order, and nothing else", () => {
    expect(advertised.map((t) => t.name)).toEqual([
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
      "gpu_ssh_keys_create",
      "gpu_ssh_keys_delete",
      "gpu_instances_update",
      "gpu_instances_create",
      "gpu_instances_terminate",
    ]);
    const expected = [...READONLY_TOOL_NAMES, ...WRITE_TOOL_NAMES, ...SPEND_TOOL_NAMES];
    expect([...ALL_TOOL_NAMES]).toEqual(expected);
    expect(ALL_TOOL_NAMES).toHaveLength(15);
    expect(READONLY_TOOL_NAMES).toHaveLength(10);
    expect(WRITE_TOOL_NAMES).toHaveLength(3);
    expect(SPEND_TOOL_NAMES).toHaveLength(2);
  });

  it("has no duplicate names", () => {
    const names = advertised.map((t) => t.name);
    expect(new Set(names).size).toBe(names.length);
  });

  it("gives every tool a title and a substantive description", () => {
    for (const tool of advertised) {
      expect(tool.title, tool.name).toBeTruthy();
      expect((tool.description ?? "").length, tool.name).toBeGreaterThan(40);
    }
  });
});

// ---------------------------------------------------------------------------
// 2. Annotations split the tiers where a client can see it.
//    Rots: a host renders a provisioning call with the same affordance as a
//    catalog read, or prompts on every free lookup and trains click-through.
// ---------------------------------------------------------------------------

describe("annotations reach the client", () => {
  it("marks every read-only tool readOnlyHint: true", () => {
    for (const name of READONLY_TOOL_NAMES) {
      expect(find(name).annotations?.readOnlyHint, name).toBe(true);
    }
  });

  it("marks every spend and write tool readOnlyHint: false", () => {
    for (const name of [...WRITE_TOOL_NAMES, ...SPEND_TOOL_NAMES]) {
      expect(find(name).annotations?.readOnlyHint, name).toBe(false);
    }
  });

  it("marks exactly the two take-away tools destructive", () => {
    // Terminate destroys a machine; the SSH key delete takes away a thing the
    // user has. Everything else destroys nothing, and marking those
    // destructive would train click-through on the ones that are.
    const destructive = ["gpu_instances_terminate", "gpu_ssh_keys_delete"];
    for (const tool of advertised) {
      expect(tool.annotations?.destructiveHint, tool.name).toBe(destructive.includes(tool.name));
    }
  });
});

// ---------------------------------------------------------------------------
// 3. The guardrail's schema shape.
//    Rots: THIS is the regression that matters. A required field that becomes
//    optional, or a default that fills in a GPU count nobody named, is a
//    guardrail hole that no other test in this package would notice.
// ---------------------------------------------------------------------------

describe("spend schemas advertise capacity as required", () => {
  it("lists gpu_type under required on every spend-capable tool", () => {
    for (const name of SPEND_CAPABLE) {
      expect(requiredOf(name), name).toContain("gpu_type");
    }
  });

  it("lists gpu_count and tier under required on the provisioning tool", () => {
    for (const name of CAPACITY_TOOLS) {
      expect(requiredOf(name), name).toContain("gpu_count");
      expect(requiredOf(name), name).toContain("tier");
    }
  });

  it("lists instance_id under required on terminate", () => {
    expect(requiredOf("gpu_instances_terminate")).toContain("instance_id");
  });

  it("advertises confirmation_token as an optional property on every guarded tool", () => {
    // Both guarded tiers, on the WIRE. A tool whose definition carries the
    // param but whose advertised schema drops it is a tool an agent cannot
    // confirm — the handshake would re-quote forever.
    for (const name of [...WRITE_TOOL_NAMES, ...SPEND_TOOL_NAMES]) {
      expect(Object.keys(propertiesOf(name)), name).toContain("confirmation_token");
      expect(requiredOf(name), name).not.toContain("confirmation_token");
    }
  });

  it("advertises no default for gpu_type, gpu_count or tier", () => {
    for (const name of SPEND_TOOL_NAMES) {
      const properties = propertiesOf(name);
      for (const field of ["gpu_type", "gpu_count", "tier"]) {
        const property = properties[field];
        if (property === undefined) continue;
        expect(property, `${name}.${field} advertises a default`).not.toHaveProperty("default");
      }
    }
  });

  it("bounds gpu_count between 1 and 8 in the advertised schema", () => {
    for (const name of CAPACITY_TOOLS) {
      const property = propertiesOf(name).gpu_count!;
      expect(property.minimum, name).toBe(1);
      expect(property.maximum, name).toBe(8);
    }
  });

  it("puts no confirmation_token on any read-only tool", () => {
    for (const name of READONLY_TOOL_NAMES) {
      expect(Object.keys(propertiesOf(name)), name).not.toContain("confirmation_token");
    }
  });
});

// ---------------------------------------------------------------------------
// 4. The registered handler is the guarded one.
//    Rots: the registry wires a bare execute path and every schema assertion
//    above becomes decoration.
// ---------------------------------------------------------------------------

describe("calling a spend tool through the server quotes rather than spends", () => {
  it("returns a confirmation_required quote and issues no mutating request", async () => {
    const requests: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string | URL, init?: RequestInit) => {
        const href = typeof url === "string" ? url : url.toString();
        const pathname = new URL(href).pathname;
        requests.push(`${(init?.method ?? "GET").toUpperCase()} ${pathname}`);
        // One real offering on the pricing read: an unpriceable request is now
        // refused outright (no quote -> no token), so proving "quote, don't
        // spend" requires a request that can actually be priced. Same for the
        // SSH pre-flight: a launch with no attachable key is refused at
        // quote time, so the account must hold one key for a quote to exist.
        const body =
          pathname === "/v1/pricing"
            ? {
                data: [
                  {
                    gpu_type: "h100_sxm",
                    gpu_count: 1,
                    region: "us-west",
                    tier: "on_demand",
                    price_per_hour: 2.49,
                    available: 3,
                    instant_boot: true,
                    community: false,
                  },
                ],
                next_cursor: null,
              }
            : pathname === "/v1/ssh-keys"
              ? {
                  data: [
                    {
                      id: "key-11111111-2222-3333-4444-555555555555",
                      name: "laptop",
                      fingerprint: "SHA256:testfingerprint",
                      created_at: "2026-08-01T00:00:00Z",
                    },
                  ],
                  next_cursor: null,
                }
              : { data: [], next_cursor: null };
        return new Response(JSON.stringify(body), {
          status: 200,
          headers: { "content-type": "application/json" },
        });
      }),
    );

    const result = (await client.callTool({
      name: "gpu_instances_create",
      arguments: { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand" },
    })) as { content: Array<{ text: string }>; isError?: boolean };

    expect(result.isError).toBeFalsy();
    expect(result.content[0]!.text).toContain("confirmation_required");
    expect(requests.some((r) => r.startsWith("POST"))).toBe(false);
  });

  it("rejects a call that omits gpu_count without reaching the network", async () => {
    const requests: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string | URL) => {
        requests.push(String(url));
        return new Response("{}", { status: 200 });
      }),
    );

    // The client-side schema check and the server-side one both refuse this;
    // either way nothing may reach the API.
    await client
      .callTool({
        name: "gpu_instances_create",
        arguments: { gpu_type: "h100_sxm", tier: "on_demand" },
      })
      .catch(() => undefined);

    expect(requests).toHaveLength(0);
  });
});

// ---------------------------------------------------------------------------
// 5. The registry is the only wiring path.
//    Rots: index.ts grows a second, divergent registration block.
// ---------------------------------------------------------------------------

describe("registerAllTools is what index.ts's server gets", () => {
  it("registers the same 15 tools onto a server built by createServer()", async () => {
    const server = createServer();
    registerAllTools(server);

    const [clientTransport, serverTransport] = InMemoryTransport.createLinkedPair();
    const probe = new Client({ name: "probe", version: "0" });
    await Promise.all([server.connect(serverTransport), probe.connect(clientTransport)]);

    const { tools } = await probe.listTools();
    expect(tools).toHaveLength(15);
    await probe.close();
  });
});
