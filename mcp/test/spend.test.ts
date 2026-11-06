// The spend tier — the two tools that start or stop a paid supplier resource,
// and the one place in this package where a bug costs real money rather than
// causing an inconvenience.
//
// The handshake's own behaviour is pinned by handshake.test.ts. This file
// asserts the thing that matrix cannot see: that the spend TOOLS are actually
// wired through the guardrail, and that their schemas are shaped so an agent
// cannot reach a charge by omission.
//
// The schema claim is structural, not advisory: `gpu_type`, `gpu_count` and
// `tier` are required and carry no default, so it is tested as a parse
// failure, not as a description someone might read.
//
// Assertions are grouped by the failure they exist to catch, and each group's
// header says what rots if it goes red.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { API_KEY_ENV } from "../src/auth.js";
import { _resetTokenStoreForTests } from "../src/guardrail/handshake.js";
import type { ElicitationHost } from "../src/guardrail/elicitation.js";
import {
  SPEND_TOOL_NAMES,
  findSpendTool,
  spendTools,
  type SpendToolDefinition,
} from "../src/tools/spend/index.js";

/** Seeded credential. If this string surfaces anywhere, the test fails. */
const SENTINEL_KEY = "gpuai_test_key_sentinel";
const ENV = { [API_KEY_ENV]: SENTINEL_KEY };

/** The locked names, in registration order. */
const EXPECTED_NAMES = ["gpu_instances_create", "gpu_instances_terminate"];

/** The one that can create a charge. Terminate is destructive, not spend. */
const SPEND_CAPABLE = ["gpu_instances_create"];

// ---------------------------------------------------------------------------
// Fake HTTP — routed by "METHOD /path", recording every call.
// ---------------------------------------------------------------------------

interface Call {
  method: string;
  pathname: string;
  body?: string;
}

let calls: Call[] = [];

const PRICING = {
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
    {
      gpu_type: "h100_sxm",
      gpu_count: 1,
      region: "us-east",
      tier: "on_demand",
      price_per_hour: 2.99,
      available: 5,
      instant_boot: true,
      community: false,
    },
    {
      gpu_type: "h100_sxm",
      gpu_count: 8,
      region: "us-west",
      tier: "on_demand",
      price_per_hour: 19.92,
      available: 1,
      instant_boot: true,
      community: false,
    },
    {
      gpu_type: "a100_80gb",
      gpu_count: 1,
      region: "us-west",
      tier: "on_demand",
      price_per_hour: 1.29,
      available: 4,
      instant_boot: true,
      community: false,
    },
  ],
  next_cursor: null,
};

const SSH_KEYS = {
  data: [
    {
      id: "key-11111111-2222-3333-4444-555555555555",
      name: "laptop",
      fingerprint: "SHA256:testfingerprint",
      created_at: "2026-08-01T00:00:00Z",
    },
  ],
  next_cursor: null,
};

// NOTE the field name: the wire shape is `operation_id`, not `id`
// (openapi Operation -> SDK `operationId`). A fixture that said `id` would make
// every assertion below pass against an implementation reading the wrong field.
const CREATE_OPERATION = {
  operation_id: "op-11111111-2222-3333-4444-555555555555",
  kind: "instance.create",
  state: "pending",
  resource_id: "gpu-abc12345",
  created_at: "2026-08-14T10:00:00Z",
};

const DELETE_OPERATION = {
  operation_id: "op-99999999-8888-7777-6666-555555555555",
  kind: "instance.delete",
  state: "pending",
  resource_id: "gpu-abc12345",
  created_at: "2026-08-14T10:00:00Z",
};

const INSTANCE = {
  id: "gpu-abc12345",
  name: "trainer",
  status: "running",
  gpu_type: "h100_sxm",
  gpu_count: 2,
  region: "us-west",
  tier: "on_demand",
  price_per_hour: 4.98,
  created_at: "2026-08-13T10:00:00Z",
};

function routes(): Record<string, { status: number; body: unknown }> {
  return {
    "GET /v1/pricing": { status: 200, body: PRICING },
    "GET /v1/ssh-keys": { status: 200, body: SSH_KEYS },
    "POST /v1/instances": { status: 202, body: CREATE_OPERATION },
    "GET /v1/instances/gpu-abc12345": { status: 200, body: INSTANCE },
    "DELETE /v1/instances/gpu-abc12345": { status: 202, body: DELETE_OPERATION },
  };
}

function installFetch(overrides: Record<string, { status: number; body: unknown }> = {}): void {
  calls = [];
  const table = { ...routes(), ...overrides };
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string | URL, init?: RequestInit) => {
      const href = typeof url === "string" ? url : url.toString();
      const pathname = new URL(href).pathname;
      const method = (init?.method ?? "GET").toUpperCase();
      calls.push({
        method,
        pathname,
        body: typeof init?.body === "string" ? init.body : undefined,
      });
      const route = table[`${method} ${pathname}`];
      if (!route) {
        return new Response(JSON.stringify({ detail: `no fake route for ${method} ${pathname}` }), {
          status: 599,
          headers: { "content-type": "application/json" },
        });
      }
      return new Response(JSON.stringify(route.body), {
        status: route.status,
        headers: { "content-type": "application/json" },
      });
    }),
  );
}

function tool(name: string): SpendToolDefinition {
  const found = findSpendTool(name);
  if (!found) {
    throw new Error(`spend tool ${name} is not registered`);
  }
  return found;
}

/** The mutating calls. If one of these appears on a first call, the guardrail failed. */
function mutations(): Call[] {
  return calls.filter((c) => c.method === "POST" || c.method === "DELETE");
}

/** Parse the structured half of a tool result (content[1], or content[0] alone). */
function payload(result: { content: Array<{ text: string }> }): Record<string, unknown> {
  const raw = result.content.length > 1 ? result.content[1]!.text : result.content[0]!.text;
  return JSON.parse(raw) as Record<string, unknown>;
}

/** The quote summary — the sentence a human is asked to approve on. */
function summaryOf(result: { content: Array<{ text: string }> }): string {
  return String(payload(result).summary);
}

function firstLine(result: { content: Array<{ text: string }> }): string {
  return result.content[0]!.text.split("\n")[0]!;
}

/** Every content block joined — what an agent actually reads back. */
function text(result: { content: Array<{ text: string }> }): string {
  return result.content.map((c) => c.text).join("\n");
}

/** A client that supports elicitation and answers with the given outcome. */
function elicitor(outcome: { action: string; content?: Record<string, unknown> }): {
  host: ElicitationHost;
  messages: string[];
} {
  const messages: string[] = [];
  return {
    messages,
    host: {
      getClientCapabilities: () => ({ elicitation: {} }),
      elicitInput: async (params) => {
        messages.push(params.message);
        return outcome;
      },
    },
  };
}

beforeEach(() => {
  _resetTokenStoreForTests();
  installFetch();
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

// ---------------------------------------------------------------------------
// 1. The tier is exactly two, and advertises itself as spend-capable.
//    Rots: a spend tool added without a guardrail, or a spend tool that a
//    client renders with the same affordance as a catalog read.
// ---------------------------------------------------------------------------

describe("the spend tier is exactly the audited two", () => {
  it("registers 2 tools with the locked names, in order", () => {
    expect(spendTools).toHaveLength(2);
    expect(SPEND_TOOL_NAMES).toEqual(EXPECTED_NAMES);
  });

  it("marks every spend tool NOT read-only", () => {
    for (const definition of spendTools) {
      expect(definition.annotations.readOnlyHint, definition.name).toBe(false);
    }
  });

  it("marks terminate destructive and the spend-capable tool not", () => {
    expect(tool("gpu_instances_terminate").annotations.destructiveHint).toBe(true);
    for (const name of SPEND_CAPABLE) {
      expect(tool(name).annotations.destructiveHint, name).toBe(false);
    }
  });

  it("gives every tool a confirmation_token parameter", () => {
    for (const definition of spendTools) {
      expect(Object.keys(definition.inputSchema), definition.name).toContain(
        "confirmation_token",
      );
    }
  });
});

// ---------------------------------------------------------------------------
// 2. Structural schemas.
//    Rots: the API's "gpu_type may be omitted" affordance leaks into the MCP
//    surface, and an agent provisions hardware nobody named.
// ---------------------------------------------------------------------------

describe("schemas make capacity explicit — never inferred", () => {
  it("rejects gpu_instances_create with gpu_type omitted", async () => {
    const result = await tool("gpu_instances_create").handler(
      { gpu_count: 1, tier: "on_demand" },
      { env: ENV },
    );
    expect(result.isError).toBe(true);
    expect(mutations()).toHaveLength(0);
  });

  it("rejects gpu_instances_create with gpu_count omitted", async () => {
    const result = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", tier: "on_demand" },
      { env: ENV },
    );
    expect(result.isError).toBe(true);
    expect(mutations()).toHaveLength(0);
  });

  it("rejects gpu_instances_create with tier omitted", async () => {
    const result = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1 },
      { env: ENV },
    );
    expect(result.isError).toBe(true);
    expect(mutations()).toHaveLength(0);
  });

  it("bounds gpu_count to 1-8 — the API's own bounds, mirrored at the MCP layer", async () => {
    for (const bad of [0, 9, 100, 1.5]) {
      const result = await tool("gpu_instances_create").handler(
        { gpu_type: "h100_sxm", gpu_count: bad, tier: "on_demand" },
        { env: ENV },
      );
      expect(result.isError, `gpu_count=${bad}`).toBe(true);
    }
    expect(mutations()).toHaveLength(0);
  });

  it("advertises no default for gpu_type or gpu_count on any spend tool", () => {
    for (const name of ["gpu_instances_create"]) {
      for (const field of ["gpu_type", "gpu_count"]) {
        const entry = tool(name).inputSchema[field];
        expect(entry, `${name}.${field}`).toBeDefined();
        // A zod default would parse an absent value into a present one, which
        // is exactly the silent capacity guess this schema exists to prevent.
        //
        // The double cast is deliberate: zod 4 types the raw entry as
        // `$ZodType<unknown, unknown, …>`, whose public surface does not
        // structurally include `safeParse` even though every concrete schema
        // carries it at runtime. Going through `unknown` is what the compiler
        // asks for; the narrow shape below is still asserted, so a schema that
        // genuinely stopped being parseable would fail here rather than be cast
        // into silence.
        const parsed = (entry as unknown as { safeParse: (v: unknown) => { success: boolean } })
          .safeParse(undefined);
        expect(parsed.success, `${name}.${field} accepts undefined`).toBe(false);
      }
    }
  });

  it("requires instance_id on terminate", async () => {
    const result = await tool("gpu_instances_terminate").handler({}, { env: ENV });
    expect(result.isError).toBe(true);
    expect(mutations()).toHaveLength(0);
  });
});

// ---------------------------------------------------------------------------
// 3. No action on a first call.
//    Rots: a single tool call spends real money. The reason the handshake exists.
// ---------------------------------------------------------------------------

describe("a first call prices and returns a token — it never spends", () => {
  it("gpu_instances_create quotes without creating anything", async () => {
    const result = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand" },
      { env: ENV },
    );

    expect(result.isError).toBeFalsy();
    const quote = payload(result);
    expect(quote.status).toBe("confirmation_required");
    expect(typeof quote.confirmationToken).toBe("string");
    expect(String(quote.summary)).toContain("2.49");
    expect(String(quote.summary)).toContain("h100_sxm");
    expect(mutations()).toHaveLength(0);
  });

  it("resolves the region at quote time so the executed launch is the quoted one", async () => {
    const result = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand" },
      { env: ENV },
    );
    const resolved = payload(result).resolvedParams as Record<string, unknown>;
    // us-west is the cheapest of the two available regions in the fixture.
    expect(resolved.region).toBe("us-west");
    expect(resolved.gpu_count).toBe(1);
    expect(resolved.tier).toBe("on_demand");
  });

  it("gpu_instances_terminate describes what will be destroyed without destroying it", async () => {
    const result = await tool("gpu_instances_terminate").handler(
      { instance_id: "gpu-abc12345" },
      { env: ENV },
    );

    expect(result.isError).toBeFalsy();
    const quote = payload(result);
    expect(quote.status).toBe("confirmation_required");
    const summary = String(quote.summary);
    expect(summary).toContain("gpu-abc12345");
    expect(summary).toContain("h100_sxm");
    expect(mutations()).toHaveLength(0);
  });

  it("quotes only the price — no running session total is appended", async () => {
    const create = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand" },
      { env: ENV },
    );
    const terminate = await tool("gpu_instances_terminate").handler(
      { instance_id: "gpu-abc12345" },
      { env: ENV },
    );
    for (const result of [create, terminate]) {
      expect(summaryOf(result).toLowerCase()).not.toContain("session");
    }
    // The quote is built from the pricing, SSH-key and instance reads alone.
    for (const call of calls) {
      expect(call.pathname, call.pathname).toMatch(/^\/v1\/(pricing|ssh-keys|instances)/);
    }
  });
});

// ---------------------------------------------------------------------------
// 4. The quote refuses launches that cannot start. A refusal here is the
// whole point — the human must never be asked to approve a request the
// engine is guaranteed to reject after the yes. If these go red, the quote is
// minting tokens for impossible launches again.
// ---------------------------------------------------------------------------

describe("the quote refuses launches that cannot start", () => {
  const EMPTY_KEYS = { data: [], next_cursor: null };

  it("refuses an instance create when the account has no SSH keys", async () => {
    installFetch({ "GET /v1/ssh-keys": { status: 200, body: EMPTY_KEYS } });
    const result = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand" },
      { env: ENV },
    );
    expect(result.isError).toBe(true);
    const text = result.content.map((c) => c.text).join("\n");
    expect(text).toContain("no SSH keys");
    expect(text).not.toContain("confirmationToken");
    expect(mutations()).toHaveLength(0);
  });

  it("refuses explicit ssh_key_ids that do not exist on the account", async () => {
    installFetch();
    const result = await tool("gpu_instances_create").handler(
      {
        gpu_type: "h100_sxm",
        gpu_count: 1,
        tier: "on_demand",
        ssh_key_ids: ["key-does-not-exist"],
      },
      { env: ENV },
    );
    expect(result.isError).toBe(true);
    expect(result.content.map((c) => c.text).join("\n")).toContain("key-does-not-exist");
    expect(mutations()).toHaveLength(0);
  });

  it("quotes normally when the named ssh_key_ids exist", async () => {
    installFetch();
    const result = await tool("gpu_instances_create").handler(
      {
        gpu_type: "h100_sxm",
        gpu_count: 1,
        tier: "on_demand",
        ssh_key_ids: ["key-11111111-2222-3333-4444-555555555555"],
      },
      { env: ENV },
    );
    expect(result.isError).toBeFalsy();
    expect(typeof payload(result).confirmationToken).toBe("string");
    expect(mutations()).toHaveLength(0);
  });

  it("still quotes a create when the ssh-keys read itself fails (e.g. granular key without ssh_keys:read)", async () => {
    installFetch({
      "GET /v1/ssh-keys": { status: 403, body: { detail: "missing scope ssh_keys:read" } },
    });
    const result = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand" },
      { env: ENV },
    );
    expect(result.isError).toBeFalsy();
    expect(typeof payload(result).confirmationToken).toBe("string");
    expect(mutations()).toHaveLength(0);
  });

});

// ---------------------------------------------------------------------------
// 5. The confirming call executes — and executes the QUOTE's params.
//    Rots: confirmation cannot land (user routes around the server), or a
//    confirmed call runs something other than what was priced.
// ---------------------------------------------------------------------------

describe("a confirming call executes the quoted request", () => {
  it("creates the instance and leads with the operation id", async () => {
    const first = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand" },
      { env: ENV },
    );
    const token = String(payload(first).confirmationToken);

    const second = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand", confirmation_token: token },
      { env: ENV },
    );

    expect(second.isError).toBeFalsy();
    expect(firstLine(second)).toContain(CREATE_OPERATION.operation_id);
    expect(mutations().filter((c) => c.pathname === "/v1/instances")).toHaveLength(1);
  });

  it("sends the region the quote resolved, not one the confirming call omitted", async () => {
    const first = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand" },
      { env: ENV },
    );
    const token = String(payload(first).confirmationToken);

    await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand", confirmation_token: token },
      { env: ENV },
    );

    const post = mutations().find((c) => c.pathname === "/v1/instances");
    const body = JSON.parse(post?.body ?? "{}") as Record<string, unknown>;
    expect(body.region).toBe("us-west");
    expect(body.gpu_type).toBe("h100_sxm");
    expect(body.gpu_count).toBe(1);
  });

  it("names gpu_operations_get and the 2s-to-10s cadence in the create response", async () => {
    const first = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand" },
      { env: ENV },
    );
    const token = String(payload(first).confirmationToken);
    const second = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand", confirmation_token: token },
      { env: ENV },
    );

    const text = JSON.stringify(second);
    expect(text).toContain("gpu_operations_get");
    expect(text).toContain("2s");
    expect(text).toContain("10s");
  });

  it("reports the quoted price and the charges note, and sends only launch fields", async () => {
    const first = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand" },
      { env: ENV },
    );
    const token = String(payload(first).confirmationToken);
    const second = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand", confirmation_token: token },
      { env: ENV },
    );

    const body = payload(second);
    expect(body.quoted_price_per_hour).toBe(2.49);
    expect(String(body.charges_note)).toContain("gpu_instances_terminate");
    expect(Object.keys(body).sort()).toEqual(
      [
        "charges_note",
        "instance_id",
        "next_steps",
        "on_success",
        "operation_id",
        "operation_state",
        "quoted_price_per_hour",
        "requested",
        "warnings",
      ].sort(),
    );

    const post = mutations().find((c) => c.pathname === "/v1/instances");
    const sent = JSON.parse(post?.body ?? "{}") as Record<string, unknown>;
    const allowed = [
      "gpu_type",
      "gpu_count",
      "tier",
      "region",
      "ssh_key_ids",
      "name",
      "environment",
      "disk_gb",
      "max_price_per_hour",
    ];
    for (const key of Object.keys(sent)) {
      expect(allowed, key).toContain(key);
    }
  });

  it("advertises exactly the launch parameters on create", () => {
    expect(Object.keys(tool("gpu_instances_create").inputSchema).sort()).toEqual(
      [
        "confirmation_token",
        "disk_gb",
        "environment",
        "gpu_count",
        "gpu_type",
        "max_price_per_hour",
        "name",
        "region",
        "ssh_key_ids",
        "tier",
      ].sort(),
    );
  });

  it("terminates and tells the agent to verify", async () => {
    const first = await tool("gpu_instances_terminate").handler(
      { instance_id: "gpu-abc12345" },
      { env: ENV },
    );
    const token = String(payload(first).confirmationToken);

    const second = await tool("gpu_instances_terminate").handler(
      { instance_id: "gpu-abc12345", confirmation_token: token },
      { env: ENV },
    );

    expect(second.isError).toBeFalsy();
    expect(firstLine(second)).toContain(DELETE_OPERATION.operation_id);
    expect(JSON.stringify(second)).toContain("gpu_instances_list");
    expect(mutations().some((c) => c.method === "DELETE")).toBe(true);
  });

  it("re-quotes rather than executing when the confirming call drifts the count", async () => {
    const first = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand" },
      { env: ENV },
    );
    const token = String(payload(first).confirmationToken);

    const second = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 8, tier: "on_demand", confirmation_token: token },
      { env: ENV },
    );

    expect(payload(second).status).toBe("confirmation_required");
    expect(mutations()).toHaveLength(0);
  });
});

// ---------------------------------------------------------------------------
// 6. Descriptions carry the money-guardrail contract verbatim.
//    Rots: a plausible rewrite drops the clause that makes blanket
//    pre-authorization not count, and the guardrail loses its prose half.
// ---------------------------------------------------------------------------

describe("descriptions lift the money-guardrail wording", () => {
  it("states that confirmation must come AFTER the user has seen the price", () => {
    for (const name of SPEND_CAPABLE) {
      expect(tool(name).description, name).toContain("AFTER the user has seen the price");
    }
  });

  it("states that capacity is never inferred", () => {
    for (const name of SPEND_CAPABLE) {
      const description = tool(name).description;
      expect(description, name).toContain("never");
      expect(/infer|guess/i.test(description), name).toBe(true);
    }
  });

  it("states that one confirmation covers one call", () => {
    for (const name of SPEND_CAPABLE) {
      expect(tool(name).description.toLowerCase(), name).toContain("one confirmation");
    }
  });

  it("carries the terminate-then-verify wording on the destructive tool", () => {
    const description = tool("gpu_instances_terminate").description;
    expect(description).toContain("terminate, then verify");
    expect(description).toContain("may still be accruing charges");
  });

  it("never names a specific supplier or a hard-coded endpoint", () => {
    for (const definition of spendTools) {
      const haystack = `${definition.title} ${definition.description}`.toLowerCase();
      for (const word of ["vultr", "mock", "localhost", "http://", "https://"]) {
        expect(haystack, `${definition.name} mentions ${word}`).not.toContain(word);
      }
    }
  });
});

// ---------------------------------------------------------------------------
// 7. Elicitation is the same contract with nicer UX — never weaker.
//    Rots: elicitation becomes a second, softer path to a charge.
// ---------------------------------------------------------------------------

describe("the elicitation path enforces the handshake, not a weaker version", () => {
  it("shows the price in the elicitation message and executes on an explicit yes", async () => {
    const { host, messages } = elicitor({ action: "accept", content: { confirm: true } });

    const result = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand" },
      { env: ENV, elicitation: host },
    );

    expect(messages).toHaveLength(1);
    expect(messages[0]).toContain("2.49");
    expect(messages[0]).toContain("h100_sxm");
    expect(result.isError).toBeFalsy();
    expect(firstLine(result)).toContain(CREATE_OPERATION.operation_id);
    expect(mutations().filter((c) => c.pathname === "/v1/instances")).toHaveLength(1);
  });

  it("creates nothing when the user declines, and hands back no usable token", async () => {
    const { host } = elicitor({ action: "decline" });

    const result = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand" },
      { env: ENV, elicitation: host },
    );

    expect(mutations()).toHaveLength(0);
    const body = payload(result);
    expect(body.status).toBe("declined");
    expect(body.confirmationToken).toBeUndefined();
  });

  it("creates nothing when the client answers accept-but-not-confirmed", async () => {
    const { host } = elicitor({ action: "accept", content: { confirm: false } });

    const result = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand" },
      { env: ENV, elicitation: host },
    );

    expect(mutations()).toHaveLength(0);
    expect(payload(result).status).toBe("declined");
  });

  it("falls back to the handshake when the client does not support elicitation", async () => {
    const host: ElicitationHost = {
      getClientCapabilities: () => ({}),
      elicitInput: async () => {
        throw new Error("elicitInput must not be called on a client that lacks the capability");
      },
    };

    const result = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand" },
      { env: ENV, elicitation: host },
    );

    expect(result.isError).toBeFalsy();
    expect(payload(result).status).toBe("confirmation_required");
    expect(mutations()).toHaveLength(0);
  });

  it("falls back to the handshake when elicitation throws mid-flight", async () => {
    const host: ElicitationHost = {
      getClientCapabilities: () => ({ elicitation: {} }),
      elicitInput: async () => {
        throw new Error("client closed the prompt");
      },
    };

    const result = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand" },
      { env: ENV, elicitation: host },
    );

    expect(payload(result).status).toBe("confirmation_required");
    expect(mutations()).toHaveLength(0);
  });

  it("never puts the API key in the elicitation message", async () => {
    const { host, messages } = elicitor({ action: "accept", content: { confirm: true } });

    await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand" },
      { env: ENV, elicitation: host },
    );

    for (const message of messages) {
      expect(message).not.toContain(SENTINEL_KEY);
      expect(message).not.toMatch(/gpuai_[A-Za-z0-9_]+/);
    }
  });

  it("does not re-elicit on a call that already carries a valid token", async () => {
    const first = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand" },
      { env: ENV },
    );
    const token = String(payload(first).confirmationToken);

    const { host, messages } = elicitor({ action: "decline" });
    const second = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand", confirmation_token: token },
      { env: ENV, elicitation: host },
    );

    // The token IS the approval. A decline prompt here would let a client
    // veto a confirmation the handshake already validated — and, worse, a
    // second prompt on every confirm trains users to click through.
    expect(messages).toHaveLength(0);
    expect(firstLine(second)).toContain(CREATE_OPERATION.operation_id);
  });
});

// ---------------------------------------------------------------------------
// 8. Failures stay legible and redacted.
//    Rots: a raw SDK error, or the credential, reaches the model's context.
// ---------------------------------------------------------------------------

describe("errors funnel through the mapper", () => {
  it("never echoes the credential when the API rejects the request", async () => {
    const result = await tool("gpu_instances_terminate").handler(
      { instance_id: "gpu-does-not-exist" },
      { env: ENV },
    );

    expect(result.isError).toBe(true);
    expect(JSON.stringify(result)).not.toContain(SENTINEL_KEY);
  });

  it("mints no token when pricing fails — a price nobody saw cannot be approved", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new Error("network is down");
      }),
    );

    const result = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand" },
      { env: ENV },
    );

    expect(result.isError).toBe(true);
    expect(JSON.stringify(result)).not.toContain("confirmationToken");
  });
});

// ---------------------------------------------------------------------------
// 9. A quote is a price, and a price is a ceiling.
//    Rots: a confirmable "quote" with no price, or an estimated launch that
//    places above the number the user was shown, in a region nobody named.
// ---------------------------------------------------------------------------

describe("unpriced requests are refused and estimated prices are enforced", () => {
  it("refuses to quote when no pricing is listed at all — no token, no ceiling-less launch", async () => {
    // "b200" has no rows in the fixture: selectOffer resolves nothing. The old
    // behaviour returned a confirmable quote reading "no price could be
    // resolved" whose confirm launched at whatever the market charged.
    const result = await tool("gpu_instances_create").handler(
      { gpu_type: "b200", gpu_count: 1, tier: "on_demand" },
      { env: ENV },
    );

    expect(result.isError).toBe(true);
    const text = JSON.stringify(result);
    expect(text).not.toContain("confirmationToken");
    expect(text).toContain("No pricing is currently listed");
    expect(text).not.toContain("Could not reach");
    expect(mutations()).toHaveLength(0);
  });

  it("pins a ceiling and a region on estimated quotes — the estimate is what was approved", async () => {
    // gpu_count 2 has no exact h100_sxm offering (fixture lists 1 and 8), so
    // the price is derived per-GPU from the cheapest reference: 2 x $2.49 =
    // $4.98 estimated, in the reference offering's region (us-west).
    const first = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 2, tier: "on_demand" },
      { env: ENV },
    );

    expect(first.isError).toBeFalsy();
    const quote = payload(first);
    expect(quote.status).toBe("confirmation_required");
    expect(String(quote.summary)).toContain("ceiling");
    const resolved = quote.resolvedParams as Record<string, unknown>;
    expect(resolved.quoted_price_is_estimate).toBe(true);
    expect(resolved.max_price_per_hour).toBe(4.98);
    expect(resolved.region).toBe("us-west");

    // The confirmed launch carries both to the API — the ceiling is enforced
    // by the platform, not by prose.
    const token = String(quote.confirmationToken);
    await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 2, tier: "on_demand", confirmation_token: token },
      { env: ENV },
    );
    const post = mutations().find((c) => c.pathname === "/v1/instances");
    const body = JSON.parse(post?.body ?? "{}") as Record<string, unknown>;
    expect(body.max_price_per_hour).toBe(4.98);
    expect(body.region).toBe("us-west");
  });

  it("never overrides a ceiling the user set themselves", async () => {
    const result = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand", max_price_per_hour: 3.5 },
      { env: ENV },
    );
    const resolved = payload(result).resolvedParams as Record<string, unknown>;
    expect(resolved.max_price_per_hour).toBe(3.5);
  });

  it("applies no automatic ceiling when disk_gb is customized — documented, not accidental", async () => {
    // Extra disk folds into the hourly rate, so the quoted GPU price would
    // trip the ceiling for a non-price reason. The docs tell the user to set
    // max_price_per_hour themselves in this case.
    const result = await tool("gpu_instances_create").handler(
      { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand", disk_gb: 200 },
      { env: ENV },
    );
    const resolved = payload(result).resolvedParams as Record<string, unknown>;
    expect(resolved.max_price_per_hour).toBeUndefined();
  });
});
