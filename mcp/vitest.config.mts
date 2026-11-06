// Vitest runner config for the MCP server package — offline unit tests only
// (handshake mechanics, schema shape, error mapping, auth wiring all run
// with no network and no credentials). The live/adversarial gates are manual.
import { defineConfig } from "vitest/config";

export default defineConfig({
  test: {
    environment: "node",
    include: ["test/**/*.test.ts"],
    // Tests exist. An empty match means the include glob drifted — that MUST
    // fail rather than pass silently, or the guardrail regression suite could
    // be deleted by accident.
  },
});
