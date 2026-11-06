// The write tier — the three tools that change account state for free.
//
// "Free" is why this file exists. The spend tier's tests can lean on an obvious
// stake: a bug there costs money. Nothing here costs anything, and the failure
// mode is quieter and no less real — an agent acting on injected instructions
// creating an SSH key that grants persistent access, or deleting the key the
// user relies on. None of those cost a cent at the moment they happen.
//
// So the assertions are deliberately STRUCTURAL and mostly roster-iterated:
// rather than testing each tool's confirmation individually and hoping the
// next tool gets the same treatment, the tests walk `writeTools` and assert
// the property of every member. A tool added without the guardrail fails a
// test nobody had to remember to write for it — which is the only version of
// this discipline that survives the tier growing.
//
// Assertions are grouped by the failure they exist to catch, and each group's
// header says what rots if it goes red.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { API_KEY_ENV } from "../src/auth.js";
import { _resetTokenStoreForTests } from "../src/guardrail/handshake.js";
import {
  WRITE_TOOL_NAMES,
  findWriteTool,
  writeTools,
  type WriteToolDefinition,
} from "../src/tools/write/index.js";

/** Seeded credential. If this string surfaces anywhere, the test fails. */
const SENTINEL_KEY = "gpuai_test_key_sentinel";
const ENV = { [API_KEY_ENV]: SENTINEL_KEY };

/** The locked names, in registration order. */
const EXPECTED_NAMES = ["gpu_ssh_keys_create", "gpu_ssh_keys_delete", "gpu_instances_update"];

/** The one that takes something away. The other two add or rename. */
const DESTRUCTIVE = ["gpu_ssh_keys_delete"];

/**
 * Minimal valid arguments per tool, for the roster-iterated calls.
 *
 * Deliberately a table rather than a generated shape: the point of the
 * roster-iterated tests is that a NEW tool fails until someone adds it here,
 * which forces the author to state what a valid call to it looks like.
 */
const VALID_ARGS: Record<string, Record<string, unknown>> = {
  gpu_ssh_keys_create: {
    name: "laptop",
    public_key: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITESTKEY laptop",
  },
  gpu_ssh_keys_delete: { ssh_key_id: "key-11111111-2222-3333-4444-555555555555" },
  gpu_instances_update: { instance_id: "gpu-abc12345", name: "renamed" },
};

// ---------------------------------------------------------------------------
// Fake HTTP — routed by "METHOD /path", recording every call.
// ---------------------------------------------------------------------------

interface Call {
  method: string;
  pathname: string;
  body?: string;
}

let calls: Call[] = [];

const SSH_KEY = {
  id: "key-11111111-2222-3333-4444-555555555555",
  name: "laptop",
  fingerprint: "SHA256:testfingerprint",
  created_at: "2026-08-01T00:00:00Z",
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
    "GET /v1/ssh-keys/key-11111111-2222-3333-4444-555555555555": { status: 200, body: SSH_KEY },
    "POST /v1/ssh-keys": { status: 201, body: SSH_KEY },
    "DELETE /v1/ssh-keys/key-11111111-2222-3333-4444-555555555555": { status: 204, body: {} },
    "GET /v1/instances/gpu-abc12345": { status: 200, body: INSTANCE },
    "PATCH /v1/instances/gpu-abc12345": { status: 200, body: { ...INSTANCE, name: "renamed" } },
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
      // 204 is a null-body status: constructing a Response with a body throws,
      // and the deletes in this tier genuinely answer 204. Modelling that
      // faithfully is the point — a stub that returned 200-with-{} would hide a
      // deserialiser that cannot cope with an empty body.
      if (route.status === 204 || route.status === 304) {
        return new Response(null, { status: route.status });
      }
      return new Response(JSON.stringify(route.body), {
        status: route.status,
        headers: { "content-type": "application/json" },
      });
    }),
  );
}

function tool(name: string): WriteToolDefinition {
  const found = findWriteTool(name);
  if (!found) {
    throw new Error(`write tool ${name} is not registered`);
  }
  return found;
}

/** Everything that is not a read. If one appears on a first call, we failed. */
function mutations(): Call[] {
  return calls.filter((c) => c.method !== "GET");
}

/** Parse the structured half of a tool result (content[1], or content[0] alone). */
function payload(result: { content: Array<{ text: string }> }): Record<string, unknown> {
  const raw = result.content.length > 1 ? result.content[1]!.text : result.content[0]!.text;
  return JSON.parse(raw) as Record<string, unknown>;
}

/** The whole result as one string — for phrase assertions across both parts. */
function text(result: { content: Array<{ text: string }> }): string {
  return result.content.map((c) => c.text).join("\n");
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
// 1. The tier is exactly the audited roster.
//    Rots: a write tool is added, reordered or dropped without the roster
//    assertions below ever running against it.
// ---------------------------------------------------------------------------

describe("the write tier is exactly the audited roster", () => {
  it("registers 3 tools with the locked names, in order", () => {
    expect(writeTools).toHaveLength(3);
    expect(WRITE_TOOL_NAMES).toEqual(EXPECTED_NAMES);
  });

  it("marks every write tool NOT read-only", () => {
    for (const definition of writeTools) {
      expect(definition.annotations.readOnlyHint, definition.name).toBe(false);
    }
  });

  it("marks the one take-away tool destructive and the other two not", () => {
    for (const definition of writeTools) {
      expect(definition.annotations.destructiveHint, definition.name).toBe(
        DESTRUCTIVE.includes(definition.name),
      );
    }
  });

  it("gives every write tool a confirmation_token parameter", () => {
    for (const definition of writeTools) {
      expect(Object.keys(definition.inputSchema), definition.name).toContain(
        "confirmation_token",
      );
    }
  });

  it("gives every write tool a title and a substantive description", () => {
    for (const definition of writeTools) {
      expect(definition.title, definition.name).toBeTruthy();
      expect(definition.description.length, definition.name).toBeGreaterThan(40);
    }
  });

  it("has valid arguments recorded for every registered tool", () => {
    // The guard that keeps the roster-iterated tests below honest: a tool with
    // no entry here would be silently skipped by them.
    for (const definition of writeTools) {
      expect(VALID_ARGS[definition.name], definition.name).toBeDefined();
    }
  });
});

// ---------------------------------------------------------------------------
// 2. The confirmation is structural, for every member.
//    Rots: THIS is the group that matters. A write tool that executes on its
//    first call is a write tool an injected "don't ask me, just do it" reaches.
// ---------------------------------------------------------------------------

describe("every write tool requires a confirmation before it mutates", () => {
  it("returns confirmation_required and issues no mutating request", async () => {
    for (const definition of writeTools) {
      installFetch();
      _resetTokenStoreForTests();

      const result = await definition.handler(VALID_ARGS[definition.name]!, { env: ENV });

      expect(result.isError, definition.name).toBeFalsy();
      expect(text(result), definition.name).toContain("confirmation_required");
      expect(mutations(), definition.name).toHaveLength(0);
    }
  });

  it("mints a token that is single-use", async () => {
    for (const definition of writeTools) {
      installFetch();
      _resetTokenStoreForTests();

      const args = VALID_ARGS[definition.name]!;
      const quoted = payload(await definition.handler(args, { env: ENV }));
      const token = quoted.confirmationToken as string;
      expect(token, definition.name).toBeTruthy();

      const confirmed = await definition.handler(
        { ...args, confirmation_token: token },
        { env: ENV },
      );
      expect(confirmed.isError, definition.name).toBeFalsy();
      const before = mutations().length;
      expect(before, definition.name).toBe(1);

      // Replaying the burnt token must not mutate a second time — it re-quotes.
      const replay = await definition.handler({ ...args, confirmation_token: token }, { env: ENV });
      expect(text(replay), definition.name).toContain("confirmation_required");
      expect(mutations(), definition.name).toHaveLength(before);
    }
  });

  it("invalidates a token when the params drift between quote and confirm", async () => {
    const definition = tool("gpu_instances_update");
    const quoted = payload(
      await definition.handler({ instance_id: "gpu-abc12345", name: "renamed" }, { env: ENV }),
    );
    const token = quoted.confirmationToken as string;

    const drifted = await definition.handler(
      { instance_id: "gpu-abc12345", name: "something-else", confirmation_token: token },
      { env: ENV },
    );

    expect(text(drifted)).toContain("confirmation_required");
    expect(mutations()).toHaveLength(0);
  });

  it("does not execute on a fabricated token", async () => {
    const definition = tool("gpu_ssh_keys_create");
    const result = await definition.handler(
      { ...VALID_ARGS.gpu_ssh_keys_create, confirmation_token: "not-a-real-token" },
      { env: ENV },
    );

    expect(text(result)).toContain("confirmation_required");
    expect(mutations()).toHaveLength(0);
  });
});

// ---------------------------------------------------------------------------
// 3. The consequence sentences.
//    Rots: a confirmation that says "create an SSH key" and not what an SSH key
//    on this platform actually grants — a yes to a sentence nobody understood.
// ---------------------------------------------------------------------------

describe("each write tool's confirmation says what it actually does", () => {
  it("says an SSH key grants persistent access to provisioned instances", async () => {
    const result = await tool("gpu_ssh_keys_create").handler(VALID_ARGS.gpu_ssh_keys_create, {
      env: ENV,
    });
    expect(text(result)).toContain("grants persistent access to provisioned instances");
  });

  it("names the key and admits a delete does not revoke access already granted", async () => {
    const result = await tool("gpu_ssh_keys_delete").handler(VALID_ARGS.gpu_ssh_keys_delete!, {
      env: ENV,
    });
    expect(text(result)).toContain("laptop");
    expect(text(result)).toContain("SHA256:testfingerprint");
    expect(text(result)).toContain("does not by itself revoke access");
  });

  it("states old and new name when quoting a rename", async () => {
    const result = await tool("gpu_instances_update").handler(VALID_ARGS.gpu_instances_update, {
      env: ENV,
    });
    expect(text(result)).toContain("trainer");
    expect(text(result)).toContain("renamed");
  });

});

// ---------------------------------------------------------------------------
// 4. What a confirmed write hands back.
//    Rots: the executed result loses the id or the caveat the agent must relay.
// ---------------------------------------------------------------------------

describe("confirmed writes report what changed", () => {
  async function confirm(name: string): Promise<{ content: Array<{ text: string }> }> {
    const definition = tool(name);
    const args = VALID_ARGS[name]!;
    const quoted = payload(await definition.handler(args, { env: ENV }));
    return definition.handler(
      { ...args, confirmation_token: quoted.confirmationToken as string },
      { env: ENV },
    );
  }

  it("gpu_ssh_keys_create POSTs the key and returns its id and fingerprint", async () => {
    const result = await confirm("gpu_ssh_keys_create");
    const post = mutations().find((c) => c.method === "POST");
    expect(post?.pathname).toBe("/v1/ssh-keys");
    expect(JSON.parse(post!.body!)).toMatchObject({
      name: "laptop",
      public_key: VALID_ARGS.gpu_ssh_keys_create!.public_key,
    });
    const body = payload(result);
    expect(body.ssh_key_id).toBe(SSH_KEY.id);
    expect(body.fingerprint).toBe(SSH_KEY.fingerprint);
  });

  it("gpu_ssh_keys_delete DELETEs the key and carries the access caveat", async () => {
    const result = await confirm("gpu_ssh_keys_delete");
    expect(mutations().map((c) => `${c.method} ${c.pathname}`)).toEqual([
      `DELETE /v1/ssh-keys/${SSH_KEY.id}`,
    ]);
    const body = payload(result);
    expect(body.deleted).toBe(true);
    expect(String(body.access_note)).toContain("Running instances");
  });

  it("gpu_instances_update PATCHes only the name and says the cost is unchanged", async () => {
    const result = await confirm("gpu_instances_update");
    const patch = mutations().find((c) => c.method === "PATCH");
    expect(patch?.pathname).toBe("/v1/instances/gpu-abc12345");
    expect(JSON.parse(patch!.body!)).toEqual({ name: "renamed" });
    const body = payload(result);
    expect(body.name).toBe("renamed");
    expect(String(body.cost_note)).toContain("changes nothing about what this instance costs");
    expect(Object.keys(body).sort()).toEqual(["cost_note", "instance_id", "name", "status"]);
  });
});

// ---------------------------------------------------------------------------
// 5. gpu_instances_update is rename-only.
//    Rots: the tool grows a second mutable field and stops being the narrow
//    thing its confirmation describes.
// ---------------------------------------------------------------------------

describe("gpu_instances_update can only rename", () => {
  it("advertises exactly instance_id, name and the confirmation token", () => {
    expect(Object.keys(tool("gpu_instances_update").inputSchema).sort()).toEqual(
      ["confirmation_token", "instance_id", "name"].sort(),
    );
  });

  it("rejects an unknown field without reaching the network", async () => {
    const result = await tool("gpu_instances_update").handler(
      { instance_id: "gpu-abc12345", name: "renamed", gpu_count: 8 },
      { env: ENV },
    );
    expect(result.isError).toBe(true);
    expect(calls).toHaveLength(0);
  });
});

// ---------------------------------------------------------------------------
// 6. The credential never surfaces.
//    Rots: a write tool's error path forwards a body carrying the API key.
// ---------------------------------------------------------------------------

describe("the API key never appears in a write tool's output", () => {
  it("keeps the key out of both success and failure paths", async () => {
    for (const definition of writeTools) {
      installFetch();
      _resetTokenStoreForTests();
      const quoted = await definition.handler(VALID_ARGS[definition.name]!, { env: ENV });
      expect(text(quoted), definition.name).not.toContain(SENTINEL_KEY);

      installFetch({
        "GET /v1/ssh-keys/key-11111111-2222-3333-4444-555555555555": {
          status: 500,
          body: { detail: `boom with ${SENTINEL_KEY}` },
        },
        "GET /v1/instances/gpu-abc12345": {
          status: 500,
          body: { detail: `boom with ${SENTINEL_KEY}` },
        },
      });
      _resetTokenStoreForTests();
      const failed = await definition.handler(VALID_ARGS[definition.name]!, { env: ENV });
      expect(text(failed), definition.name).not.toContain(SENTINEL_KEY);
    }
  });

  it("maps a failed quote read through the error funnel", async () => {
    installFetch({
      "GET /v1/instances/gpu-abc12345": {
        status: 404,
        body: { detail: "instance not found" },
      },
    });
    const result = await tool("gpu_instances_update").handler(VALID_ARGS.gpu_instances_update!, {
      env: ENV,
    });
    expect(result.isError).toBe(true);
    expect(text(result).toLowerCase()).toContain("not found");
    expect(mutations()).toHaveLength(0);
  });
});
