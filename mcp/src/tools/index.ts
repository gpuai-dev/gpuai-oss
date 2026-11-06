// The tool registry — the one place tool definitions become MCP tools.
//
// Three tiers, one registration path. The read-only tier (free, non-mutating),
// the write tier (mutations that cost nothing and still need a yes), and the
// guarded spend tier (create, which starts a paid instance, and terminate,
// which destroys one) are defined in their
// own files against the same definition shape, and this module walks all three
// arrays through `server.registerTool`.
//
// Having exactly one registration path is the point. A tool that registered
// itself — or a second block in index.ts that wired "just one more" — would be
// a tool outside the tier files, and the tier files are where each tool's
// guardrail is decided. That only holds while this is the only door.
//
// The tier a tool lands in is the tier whose guardrail it gets, and that is a
// definition-level fact, not a registry one — which is why adding the write tier
// here is three lines and no new decision.
//
// What this module deliberately does NOT do:
//
//   * decide anything about spend. The handshake lives in the spend tier's own
//     wrapper, so a tool arrives here already guarded. If the registry could
//     grant or skip the guardrail, the guardrail would be a registry decision;
//     it is a definition-level one.
//   * read the environment or build an API client. Handlers do that per call,
//     lazily, which is what keeps boot offline — registering every tool
//     makes zero network calls.
import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";

import type { ElicitFormRequest, ElicitationHost } from "../guardrail/elicitation.js";
import { READONLY_TOOL_NAMES, readonlyTools } from "./readonly/index.js";
import { SPEND_TOOL_NAMES, spendTools } from "./spend/index.js";
import { WRITE_TOOL_NAMES, writeTools } from "./write/index.js";

/** Every advertised tool name, in tier order: read-only, write, spend. */
export const ALL_TOOL_NAMES: readonly string[] = [
  ...READONLY_TOOL_NAMES,
  ...WRITE_TOOL_NAMES,
  ...SPEND_TOOL_NAMES,
];

/**
 * Adapt the MCP server to the guardrail's elicitation interface.
 *
 * Three notes, each a thing that cost time to find:
 *
 *  1. `elicitInput` lives on the LOW-LEVEL server (`server.server`), not on the
 *     per-request `extra` object a tool callback receives. In
 *     @modelcontextprotocol/sdk 1.30.0 `RequestHandlerExtra` carries
 *     signal / sessionId / sendRequest / sendNotification and no elicitation
 *     helper at all.
 *  2. The cast is the single boundary between the SDK's generated zod-inferred
 *     request type and the small structural interface the guardrail is written
 *     against. Keeping the guardrail free of SDK types is what lets it be
 *     tested with a plain object, and one cast here is the price.
 *  3. Capabilities are only known after `initialize` completes, so this is read
 *     per call rather than captured once at registration.
 */
function elicitationHost(server: McpServer): ElicitationHost {
  return {
    getClientCapabilities: () => server.server.getClientCapabilities(),
    elicitInput: (params: ElicitFormRequest) =>
      server.server.elicitInput(params as Parameters<typeof server.server.elicitInput>[0]),
  };
}

/**
 * Register every tool onto a server instance.
 *
 * Idempotent it is NOT — the MCP SDK throws on a duplicate name, which is the
 * correct behaviour and the reason this is called exactly once at boot.
 */
export function registerAllTools(server: McpServer): void {
  for (const definition of readonlyTools) {
    server.registerTool(
      definition.name,
      {
        title: definition.title,
        description: definition.description,
        inputSchema: definition.inputSchema,
        annotations: definition.annotations,
      },
      // The SDK hands the callback parsed args; the definition re-parses with
      // its own schema, which is what keeps the definition usable (and tested)
      // without a server in front of it.
      (async (args: Record<string, unknown>) =>
        definition.handler(args)) as Parameters<typeof server.registerTool>[2],
    );
  }

  // The write tier takes the same context as the spend tier: its confirmation
  // uses the same elicitation adapter, because it is the same handshake with a
  // consequence sentence where the price line goes.
  for (const definition of writeTools) {
    server.registerTool(
      definition.name,
      {
        title: definition.title,
        description: definition.description,
        inputSchema: definition.inputSchema,
        annotations: definition.annotations,
      },
      (async (args: Record<string, unknown>) =>
        definition.handler(args, { elicitation: elicitationHost(server) })) as Parameters<
        typeof server.registerTool
      >[2],
    );
  }

  for (const definition of spendTools) {
    server.registerTool(
      definition.name,
      {
        title: definition.title,
        description: definition.description,
        inputSchema: definition.inputSchema,
        annotations: definition.annotations,
      },
      (async (args: Record<string, unknown>) =>
        definition.handler(args, { elicitation: elicitationHost(server) })) as Parameters<
        typeof server.registerTool
      >[2],
    );
  }
}
