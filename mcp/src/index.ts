#!/usr/bin/env node
// Entry point for the GPU.ai Open Source MCP server.
//
// Speaks MCP over stdio: the AI coding tool spawns this file as a subprocess
// and talks JSON-RPC to it on stdin/stdout. That makes stdout a protocol
// channel — all human-facing output must go to stderr, never stdout, or it
// corrupts the stream.
//
// This file stays thin on purpose: check the credential, build the server,
// register the tools, connect stdio. Everything with a decision in it lives
// somewhere testable — the tiers, the registry, the guardrail.
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";

import { buildConfiguration, describeStartupError } from "./auth.js";
import { registerAllTools } from "./tools/index.js";
import { readVersion } from "./version.js";

/** Server name as it appears to MCP clients. */
export const SERVER_NAME = "gpuai";

// readVersion lives in version.ts so auth.ts can stamp the outbound
// User-Agent with it without importing this module, which
// imports auth.ts. Re-exported here because this is where callers have always
// found it.
export { readVersion };

/**
 * Construct a bare MCP server — no tools attached.
 *
 * Kept separate from {@link buildServer} so a test can register a deliberately
 * partial surface, and so "constructing a server" never implies "and also
 * registering every tool".
 */
export function createServer(): McpServer {
  return new McpServer({ name: SERVER_NAME, version: readVersion() });
}

/**
 * Construct the server the client actually talks to: every tool registered.
 *
 * Registration performs no I/O — handlers read the environment and build an API
 * client lazily, per call — so this stays offline and instant, which is what
 * lets boot fail on configuration alone.
 */
export function buildServer(): McpServer {
  const server = createServer();
  registerAllTools(server);
  return server;
}

/**
 * Boot: validate configuration first, then connect the transport.
 *
 * Order matters. Validation is offline and instant, so a misconfigured server
 * fails with a readable setup message rather than completing an MCP handshake
 * and then failing confusingly on the client's first tool call.
 */
export async function main(): Promise<void> {
  // Presence check + base URL resolution. Throws StartupConfigError on failure.
  buildConfiguration();

  const server = buildServer();
  const transport = new StdioServerTransport();
  await server.connect(transport);
}

if (require.main === module) {
  main().catch((err: unknown) => {
    process.stderr.write(`${SERVER_NAME} MCP server failed to start.\n\n`);
    process.stderr.write(`${describeStartupError(err)}\n`);
    process.exitCode = 1;
  });
}
