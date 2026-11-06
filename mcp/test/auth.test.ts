// Startup auth/base-URL wiring — the four behaviours this module locks.
//
// These are the guardrail's first line: a server that boots without a key, or
// that silently points a customer at the wrong environment, or that echoes the
// credential into an error message, is broken in a way no later test catches.
import { afterEach, describe, expect, it, vi } from "vitest";

import {
  API_BASE_ENV,
  API_KEY_ENV,
  DEFAULT_API_BASE,
  StartupConfigError,
  buildConfiguration,
  describeStartupError,
  requireApiKey,
  resolveBaseUrl,
} from "../src/auth.js";
import { readVersion } from "../src/version.js";

/** A key value that must never appear in any output the server produces. */
const LEAK_SENTINEL = "gpuai_test_LEAKCHECK";

afterEach(() => {
  vi.restoreAllMocks();
});

describe("requireApiKey — fail fast when the key is absent", () => {
  it("throws a StartupConfigError naming GPUAI_API_KEY when the env var is unset", () => {
    expect(() => requireApiKey({})).toThrow(StartupConfigError);
    expect(() => requireApiKey({})).toThrow(/GPUAI_API_KEY/);
  });

  it("throws when the key is set but blank — an empty string is not a credential", () => {
    expect(() => requireApiKey({ [API_KEY_ENV]: "   " })).toThrow(/GPUAI_API_KEY/);
  });

  it("returns the key verbatim when present", () => {
    expect(requireApiKey({ [API_KEY_ENV]: LEAK_SENTINEL })).toBe(LEAK_SENTINEL);
  });

  it("produces a setup message that names the env var and how to fix it", () => {
    let thrown: unknown;
    try {
      requireApiKey({});
    } catch (err) {
      thrown = err;
    }
    const rendered = describeStartupError(thrown, {});
    expect(rendered).toContain(API_KEY_ENV);
    // Points the operator at where the per-client env block goes, rather than
    // dumping a bare stack trace.
    expect(rendered.toLowerCase()).toMatch(/env|environment/);
  });
});

describe("never echo the credential", () => {
  it("keeps the key value out of the failure output when another config value is invalid", () => {
    const env = { [API_KEY_ENV]: LEAK_SENTINEL, [API_BASE_ENV]: "not-a-url" };

    let thrown: unknown;
    expect(() => {
      try {
        buildConfiguration(env);
      } catch (err) {
        thrown = err;
        throw err;
      }
    }).toThrow(StartupConfigError);

    const rendered = describeStartupError(thrown, env);

    // The failure is about the base URL...
    expect(rendered).toContain(API_BASE_ENV);
    // ...and it must not carry the credential along with it, anywhere.
    expect(rendered).not.toContain(LEAK_SENTINEL);
    expect((thrown as Error).message).not.toContain(LEAK_SENTINEL);
    expect(String((thrown as Error).stack ?? "")).not.toContain(LEAK_SENTINEL);
  });

  it("redacts the key even if a message somehow interpolates it", () => {
    const env = { [API_KEY_ENV]: LEAK_SENTINEL };
    const leaky = new Error(`upstream rejected token ${LEAK_SENTINEL}`);
    const rendered = describeStartupError(leaky, env);
    expect(rendered).not.toContain(LEAK_SENTINEL);
  });
});

describe("resolveBaseUrl — local server by default, override honoured verbatim", () => {
  it("resolves to the local gpuctl server when GPUAI_API_BASE is unset", () => {
    expect(resolveBaseUrl({})).toBe("http://localhost:8080/v1");
    expect(DEFAULT_API_BASE).toBe("http://localhost:8080/v1");
  });

  it("resolves to the default when GPUAI_API_BASE is set but empty", () => {
    expect(resolveBaseUrl({ [API_BASE_ENV]: "" })).toBe(DEFAULT_API_BASE);
  });

  it("returns the override verbatim when set", () => {
    const override = "https://api.example.invalid/v1";
    expect(resolveBaseUrl({ [API_BASE_ENV]: override })).toBe(override);
  });

  it("does not normalise, rewrite, or strip the override", () => {
    const override = "http://localhost:9090/v1/";
    expect(resolveBaseUrl({ [API_BASE_ENV]: override })).toBe(override);
  });

  it("wires the resolved key and base into the SDK Configuration", async () => {
    const config = buildConfiguration({
      [API_KEY_ENV]: LEAK_SENTINEL,
      [API_BASE_ENV]: "https://api.example.invalid/v1",
    });
    expect(config.basePath).toBe("https://api.example.invalid/v1");
    const accessToken = config.accessToken;
    expect(accessToken).toBeTypeOf("function");
    await expect(accessToken?.()).resolves.toBe(LEAK_SENTINEL);
  });

  it("defaults the Configuration to the local server when no override is given", () => {
    const config = buildConfiguration({ [API_KEY_ENV]: LEAK_SENTINEL });
    expect(config.basePath).toBe(DEFAULT_API_BASE);
  });
});

describe("startup validation performs no network I/O", () => {
  it("never calls fetch while validating configuration", () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");

    const env = { [API_KEY_ENV]: LEAK_SENTINEL, [API_BASE_ENV]: "https://api.example.invalid/v1" };
    requireApiKey(env);
    resolveBaseUrl(env);
    buildConfiguration(env);

    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it("never calls fetch on the failure path either", () => {
    const fetchSpy = vi.spyOn(globalThis, "fetch");

    expect(() => buildConfiguration({})).toThrow(StartupConfigError);

    expect(fetchSpy).not.toHaveBeenCalled();
  });
});

describe("User-Agent stamping", () => {
  // WHY THIS MATTERS: MCP wraps @gpuai/sdk, so without an explicit header a
  // call from an AI coding tool is byte-identical to a hand-written TS SDK
  // call. The whole point of per-surface attribution is telling them apart —
  // "how many people actually use the MCP server" has no other answer.
  it("stamps a versioned gpuai-mcp User-Agent on the SDK Configuration", () => {
    const config = buildConfiguration({ [API_KEY_ENV]: LEAK_SENTINEL });
    const ua = config.headers?.["User-Agent"];
    expect(ua).toMatch(/^gpuai-mcp\/(\d+\.\d+\.\d+|0\.0\.0-dev)$/);
  });

  it("reports the same version readVersion() resolves, not a compiled-in copy", () => {
    const config = buildConfiguration({ [API_KEY_ENV]: LEAK_SENTINEL });
    expect(config.headers?.["User-Agent"]).toBe(`gpuai-mcp/${readVersion()}`);
  });

  it("is distinct from the TS SDK it wraps — MCP's header wins over the SDK default", () => {
    const config = buildConfiguration({ [API_KEY_ENV]: LEAK_SENTINEL });
    expect(config.headers?.["User-Agent"]).not.toContain("gpuai-sdk-ts");
  });

  it("never carries the credential into the header block ", () => {
    const config = buildConfiguration({ [API_KEY_ENV]: LEAK_SENTINEL });
    // The whole header map, stringified — a leak anywhere in it fails here,
    // not just under the key we happen to assert on.
    expect(JSON.stringify(config.headers ?? {})).not.toContain(LEAK_SENTINEL);
  });

  it("still exports readVersion from the package entry point", async () => {
    // readVersion moved to version.ts to break the auth.ts -> index.ts cycle;
    // the re-export is what keeps every existing caller working.
    const entry = await import("../src/index.js");
    expect(entry.readVersion()).toBe(readVersion());
  });
});
