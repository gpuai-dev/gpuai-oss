// The error funnel — every SDK failure that reaches an agent goes through here.
//
// Two properties are load-bearing and both are regression-tested below:
//
//  1. The credential never reaches the model's context. An API
//     error body is attacker-influenceable and header-adjacent; if a raw one is
//     forwarded, a leaked key ends up in a transcript that outlives the process.
//  2. A 401 says "your key is bad, check GPUAI_API_KEY", not
//     "Response returned an error code". An agent that cannot tell auth failure
//     from a server fault will retry forever or give up wrongly.
import { ResponseError } from "@gpuai/sdk";
import { describe, expect, it } from "vitest";

import { API_BASE_ENV, API_KEY_ENV } from "../src/auth.js";
import {
  mapSdkError,
  mapSdkErrorAsync,
  redactSecrets,
  summarizeErrorBody,
} from "../src/errors.js";

/** A credential-shaped value that must never survive into any output. */
const LIVE_KEY = "gpuai_test_key_primary";
/** A second token shape, NOT the live key — the shape rule must catch it too. */
const OTHER_TOKEN = "gpuai_test_key_someone_else";
/** A key that does not look like a gpuai_ token at all. */
const ODD_KEY = "not-token-shaped-at-all-42";

const ENV = { [API_KEY_ENV]: LIVE_KEY };

/** Build a real SDK ResponseError over a synthetic HTTP response. */
function responseError(status: number, body: unknown = { detail: "nope" }): ResponseError {
  const payload = typeof body === "string" ? body : JSON.stringify(body);
  return new ResponseError(
    new Response(payload, {
      status,
      headers: { "content-type": "application/json" },
    }),
    "Response returned an error code",
  );
}

describe("redactSecrets — the funnel every message passes through", () => {
  it("replaces the live key value wherever it appears", () => {
    const out = redactSecrets(`request failed with key ${LIVE_KEY} attached`, ENV);
    expect(out).not.toContain(LIVE_KEY);
    expect(out).toContain("[redacted]");
  });

  it("replaces a token-shaped substring even when it is not the live key", () => {
    const out = redactSecrets(`upstream echoed ${OTHER_TOKEN}`, ENV);
    expect(out).not.toContain(OTHER_TOKEN);
    expect(out).toContain("[redacted]");
  });

  it("replaces a live key that is not token-shaped", () => {
    const out = redactSecrets(`oops ${ODD_KEY}`, { [API_KEY_ENV]: ODD_KEY });
    expect(out).not.toContain(ODD_KEY);
  });

  it("leaves the env var NAME intact — the setup instructions depend on it", () => {
    // The name is upper-case; the token shape is lower-case. Redacting the name
    // would make every auth message useless ("check the [redacted] variable").
    const out = redactSecrets(`check the ${API_KEY_ENV} variable`, ENV);
    expect(out).toContain(API_KEY_ENV);
  });

  it("is a no-op when no key is configured", () => {
    expect(redactSecrets("plain text", {})).toBe("plain text");
  });

  it("redacts every occurrence, not just the first", () => {
    const out = redactSecrets(`${LIVE_KEY} and again ${LIVE_KEY}`, ENV);
    expect(out).not.toContain(LIVE_KEY);
  });
});

describe("mapSdkError — 401 is the auth gate", () => {
  const err = mapSdkError(responseError(401), "listing instances", { env: ENV });

  it("names the env var and says the key is invalid or revoked", () => {
    expect(err.message).toContain(API_KEY_ENV);
    expect(err.message.toLowerCase()).toContain("invalid or revoked");
  });

  it("never contains the credential value", () => {
    expect(err.message).not.toContain(LIVE_KEY);
  });

  it("never leaks Authorization header content", () => {
    expect(err.message.toLowerCase()).not.toContain("authorization");
    expect(err.message.toLowerCase()).not.toContain("bearer");
  });

  it("does not forward the raw SDK message", () => {
    expect(err.message).not.toContain("Response returned an error code");
  });
});

describe("mapSdkError — the rest of the status taxonomy", () => {
  it("an unmapped 4xx reads as a generic rejection with its status, not as a key problem", () => {
    const err = mapSdkError(responseError(402), "creating an instance", { env: ENV });
    expect(err.message).toContain("HTTP 402");
    expect(err.message).toContain("creating an instance");
    expect(err.message).not.toContain(API_KEY_ENV);
  });

  it("403 points at key permissions, distinct from 401's revoked-key message", () => {
    const err = mapSdkError(responseError(403), "creating an instance", { env: ENV });
    expect(err.message.toLowerCase()).not.toContain("invalid or revoked");
    expect(err.message.toLowerCase()).toMatch(/scope|permission/);
  });

  it("404 reads as not-found, with the context", () => {
    const err = mapSdkError(responseError(404), "getting instance gpu-1234", { env: ENV });
    expect(err.message.toLowerCase()).toContain("not found");
    expect(err.message).toContain("gpu-1234");
  });

  it("422 includes the supplied validation summary and nothing more", () => {
    const err = mapSdkError(responseError(422), "creating an instance", {
      env: ENV,
      details: "gpu_count must be between 1 and 8",
    });
    expect(err.message).toContain("gpu_count must be between 1 and 8");
    expect(err.message).not.toContain("{");
  });

  it("429 tells the agent to retry rather than to re-auth", () => {
    const err = mapSdkError(responseError(429), "listing pricing", { env: ENV });
    expect(err.message.toLowerCase()).toContain("rate limited");
    expect(err.message.toLowerCase()).toContain("retry");
  });
});

describe("mapSdkError — 500 from the operation-get path (known filed bug)", () => {
  // A malformed operation id 500s instead of 4xx-ing on the server, so the
  // mapping must at least point the agent somewhere useful.
  const err = mapSdkError(responseError(500), "getting operation not-a-uuid", { env: ENV });

  it("suggests checking the operation id format", () => {
    expect(err.message.toLowerCase()).toContain("operation id");
    expect(err.message.toLowerCase()).toMatch(/format|malformed/);
  });

  it("does not dump a stack or a response body", () => {
    expect(err.message).not.toContain("at Object.");
    expect(err.message).not.toContain("{");
    expect(err.message.split("\n").length).toBeLessThanOrEqual(2);
  });

  it("keeps a plain 5xx elsewhere free of operation-id advice", () => {
    const other = mapSdkError(responseError(503), "listing pricing", { env: ENV });
    expect(other.message.toLowerCase()).not.toContain("operation id");
    expect(other.message.toLowerCase()).toMatch(/server error|unavailable/);
  });
});

describe("mapSdkError — non-HTTP failures (network refused)", () => {
  const cause = new Error("connect ECONNREFUSED 127.0.0.1:443");
  const err = mapSdkError(cause, "listing gpu types", {
    env: { ...ENV, [API_BASE_ENV]: "https://api.internal.example.com/v1" },
  });

  it("names the base URL host so the operator knows what was unreachable", () => {
    expect(err.message).toContain("api.internal.example.com");
  });

  it("tells the operator to check the gpuctl server and the base setting", () => {
    expect(err.message).toContain("check that the gpuctl server is running");
    expect(err.message).toContain(API_BASE_ENV);
  });

  it("does not dump the full error chain", () => {
    expect(err.message).not.toContain("ECONNREFUSED 127.0.0.1:443");
    expect(err.message.split("\n").length).toBeLessThanOrEqual(2);
  });

  it("falls back gracefully when the base URL is unusable", () => {
    const fallback = mapSdkError(cause, "listing gpu types", {
      env: { ...ENV, [API_BASE_ENV]: "))not a url((" },
    });
    expect(fallback.message.length).toBeGreaterThan(0);
    expect(fallback.message).not.toContain(LIVE_KEY);
  });
});

describe("every mapped message passes through redaction", () => {
  const statuses = [400, 401, 402, 403, 404, 409, 422, 429, 500, 502, 503];

  it.each(statuses)("status %i never echoes the credential", (status) => {
    const err = mapSdkError(responseError(status), `calling with ${LIVE_KEY}`, {
      env: ENV,
      details: `upstream said ${LIVE_KEY}`,
    });
    expect(err.message).not.toContain(LIVE_KEY);
  });

  it("redacts a key seeded into a thrown non-SDK error's message", () => {
    const err = mapSdkError(new Error(`boom ${LIVE_KEY}`), "doing a thing", { env: ENV });
    expect(err.message).not.toContain(LIVE_KEY);
  });

  it("redacts a key seeded into a non-Error throw", () => {
    const err = mapSdkError(`raw string ${LIVE_KEY}`, "doing a thing", { env: ENV });
    expect(err.message).not.toContain(LIVE_KEY);
  });

  it("returns a real Error every time", () => {
    expect(mapSdkError(undefined, "doing a thing", { env: ENV })).toBeInstanceOf(Error);
  });
});

describe("summarizeErrorBody — a summary, never a dump", () => {
  it("extracts the RFC 7807 detail field", async () => {
    const summary = await summarizeErrorBody(
      responseError(422, { type: "about:blank", title: "Unprocessable", detail: "gpu_count too high", status: 422 }),
      ENV,
    );
    expect(summary).toBe("gpu_count too high");
  });

  it("extracts the OpenAI envelope message", async () => {
    const summary = await summarizeErrorBody(
      responseError(409, { error: { message: "instance is already stopping", type: "conflict" } }),
      ENV,
    );
    expect(summary).toBe("instance is already stopping");
  });

  it("redacts a credential seeded into the body", async () => {
    const summary = await summarizeErrorBody(
      responseError(422, { detail: `bad key ${LIVE_KEY}` }),
      ENV,
    );
    expect(summary).toBeDefined();
    expect(summary).not.toContain(LIVE_KEY);
  });

  it("returns undefined for an unparseable body rather than forwarding it", async () => {
    const summary = await summarizeErrorBody(responseError(500, "<html>oh no</html>"), ENV);
    expect(summary).toBeUndefined();
  });

  it("returns undefined for a non-HTTP error", async () => {
    expect(await summarizeErrorBody(new Error("nope"), ENV)).toBeUndefined();
  });

  it("truncates a long detail instead of forwarding the whole thing", async () => {
    const summary = await summarizeErrorBody(
      responseError(422, { detail: "x".repeat(5000) }),
      ENV,
    );
    expect(summary).toBeDefined();
    expect(summary!.length).toBeLessThanOrEqual(320);
  });
});

describe("mapSdkErrorAsync — the funnel tool handlers actually call", () => {
  it("folds the body summary into the message", async () => {
    const err = await mapSdkErrorAsync(
      responseError(422, { detail: "gpu_type is required" }),
      "creating an instance",
      ENV,
    );
    expect(err.message).toContain("gpu_type is required");
  });

  it("still maps 401 to the auth message and ignores the body", async () => {
    const err = await mapSdkErrorAsync(
      responseError(401, { detail: `token ${LIVE_KEY} rejected` }),
      "listing instances",
      ENV,
    );
    expect(err.message).toContain(API_KEY_ENV);
    expect(err.message).not.toContain(LIVE_KEY);
  });
});
