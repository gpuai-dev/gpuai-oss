// The package's own version, resolved at runtime.
//
// Lives in its own module rather than in index.ts because BOTH the server
// identity (index.ts, the version MCP clients see in the handshake) and the
// outbound User-Agent (auth.ts) need it — and auth.ts cannot
// import index.ts, which imports auth.ts. A one-function module is the
// cycle-free shape; index.ts re-exports it so every existing caller of
// `readVersion` from the package entry point keeps working.
import { readFileSync } from "node:fs";
import { join } from "node:path";

/** Fallback when the version cannot be read — matches the committed placeholder. */
export const PLACEHOLDER_VERSION = "0.0.0-dev";

/**
 * Read the package version at runtime.
 *
 * Read rather than compiled in, so the version injected into package.json at
 * release time is the one clients actually see, without a rebuild step
 * silently reporting a stale number.
 */
export function readVersion(): string {
  try {
    const raw = readFileSync(join(__dirname, "..", "package.json"), "utf8");
    const parsed: unknown = JSON.parse(raw);
    if (
      typeof parsed === "object" &&
      parsed !== null &&
      typeof (parsed as { version?: unknown }).version === "string"
    ) {
      return (parsed as { version: string }).version;
    }
  } catch {
    // Fall through — an unreadable package.json must not stop the server.
  }
  return PLACEHOLDER_VERSION;
}
