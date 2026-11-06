// Standing regression matrix for the two-call spend handshake.
//
// This suite pins `withSpendGuardrail`'s ONE safety property in both directions
// by machine-checked cases instead of by comments and a matrix someone runs by
// hand.
//
// The property, stated once: a spend tool can NEVER execute on a single call.
// Execution requires a token that this module minted, for this tool, for these
// exact params, not yet used, not yet expired. Every group below is one way that
// property could rot:
//
//   under-enforcement (execute happens when it shouldn't) => a user gets
//     charged for GPUs they never approved. That is the whole reason the
//     handshake exists: an ignorable guardrail on a spend surface is no
//     guardrail.
//   over-enforcement (execute refused when it should proceed) => the second call
//     never lands, the agent re-quotes forever, and the user routes around the
//     MCP server entirely. Max friction, zero real safety.
//
// Everything here is offline and pure: no network, no credential, no MCP client
// (most guardrail regression signal lives here). The absence of any
// client/elicitation object in these calls is itself load-bearing: it proves the
// guardrail holds on the fallback path, in a client with no elicitation support
// at all.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  _resetTokenStoreForTests,
  normalizeParams,
  withSpendGuardrail,
  type Quote,
  type QuoteResult,
} from "../src/guardrail/handshake.js";

/** The tool under test in most cases — the canonical spend surface. */
const CREATE = "gpu_instances_create";

/** A second spend tool, used to prove tokens do not cross tool boundaries. */
const TERMINATE = "gpu_instances_terminate";

type CreateParams = {
  gpu_type: string;
  gpu_count: number;
  tier?: string;
  confirmation_token?: string;
};

/** The params a well-behaved agent sends on call 1. */
function baseParams(over: Partial<CreateParams> = {}): CreateParams {
  return { gpu_type: "h100_sxm", gpu_count: 1, tier: "on_demand", ...over };
}

/**
 * The resolved params a quote pins down — deliberately RICHER than what the
 * agent sent (a region and a price the quote resolved). Execution must run on
 * THESE, not on whatever arrives on call 2: that is what makes
 * quote-cheap-execute-expensive structurally impossible rather than merely
 * checked.
 */
const RESOLVED = Object.freeze({
  gpu_type: "h100_sxm",
  gpu_count: 1,
  tier: "on_demand",
  region: "us-west-2",
  price_per_hour: 2.49,
});

const SUMMARY = "1x h100_sxm (on_demand) at $2.49/hr — est. $2.49 for 1 hour";

/** A quoteFn that always succeeds, and records every call. */
function makeQuoteFn(quote: Partial<Quote> = {}) {
  return vi.fn(
    async (): Promise<Quote> => ({
      summary: SUMMARY,
      resolvedParams: { ...RESOLVED },
      ...quote,
    }),
  );
}

/**
 * An executeFn whose invocation IS the breach signal for the negative cases.
 *
 * It declares its params so the recorded call args stay typed — the "executed
 * with the QUOTED params" assertion reads `mock.calls[0][0]`, and an untyped
 * spy would make that assertion unwritable.
 */
function makeExecuteFn() {
  return vi.fn(async (_params: Record<string, unknown>) => ({
    instance_id: "gpu_abc123",
    status: "provisioning",
  }));
}

/** Narrow a guardrail return value to the first-call quote shape. */
function asQuote(result: unknown): QuoteResult {
  const q = result as QuoteResult;
  expect(typeof q.confirmationToken).toBe("string");
  expect(q.confirmationToken.length).toBeGreaterThan(0);
  return q;
}

beforeEach(() => {
  _resetTokenStoreForTests();
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

// ---------------------------------------------------------------------------
// Group 1 — NO ACTION FIRST. The single most important case in the file: one
// call, on its own, spends nothing. A misbehaving agent that ignores every word
// of prose in the tool description still hits this wall.
// ---------------------------------------------------------------------------
describe("no-action-first: a first call executes nothing", () => {
  it("does not invoke executeFn when no confirmation_token is present", async () => {
    const quoteFn = makeQuoteFn();
    const executeFn = makeExecuteFn();

    const result = await withSpendGuardrail(CREATE, baseParams(), quoteFn, executeFn);

    expect(executeFn).not.toHaveBeenCalled();
    expect(quoteFn).toHaveBeenCalledTimes(1);
    const quote = asQuote(result);
    expect(quote.summary).toBe(SUMMARY);
    expect(quote.resolvedParams).toEqual(RESOLVED);
  });

  it("returns the price line and resolved params the user must be shown", async () => {
    const result = await withSpendGuardrail(
      CREATE,
      baseParams(),
      makeQuoteFn(),
      makeExecuteFn(),
    );

    const quote = asQuote(result);
    // The price has to be IN the response, not merely computed. "Confirmation is
    // only valid after the user has seen the price" (76's clause) is enforced by
    // construction here: no quote -> no token -> no execute.
    expect(quote.summary).toContain("$2.49");
    expect(quote.resolvedParams).toMatchObject({ gpu_type: "h100_sxm", gpu_count: 1 });
  });

  it("instructs the agent to show the price, get a yes, and call again with the token", async () => {
    const result = await withSpendGuardrail(
      CREATE,
      baseParams(),
      makeQuoteFn(),
      makeExecuteFn(),
    );

    const quote = asQuote(result);
    // A well-behaved agent produces good UX from this text; a misbehaving one
    // still cannot skip call 2. The text is for the former, not a substitute for
    // the latter.
    expect(quote.instructions).toMatch(/show the user this price/i);
    expect(quote.instructions).toMatch(/explicit yes/i);
    expect(quote.instructions).toContain("confirmation_token");
    expect(quote.instructions).toContain(quote.confirmationToken);
  });

  it("holds with no client capability object anywhere in the call (elicitation-free path)", async () => {
    const executeFn = makeExecuteFn();

    // Four arguments. No server, no transport, no client capabilities, no
    // elicitation. The guardrail is pure server-side logic — that is precisely
    // why it survives a client that supports none of it.
    const result = await withSpendGuardrail(CREATE, baseParams(), makeQuoteFn(), executeFn);

    expect(executeFn).not.toHaveBeenCalled();
    asQuote(result);
  });
});

// ---------------------------------------------------------------------------
// Group 2 — NO QUOTE, NO TOKEN, NO EXECUTE. A token this module never minted is
// worth nothing, no matter how plausible it looks. Note the required behaviour on
// rejection: re-quote, not error-and-die. An error would tempt the agent to
// retry-with-force; a fresh quote puts it back on the only path that works.
// ---------------------------------------------------------------------------
describe("no-quote-no-execute: fabricated tokens buy nothing", () => {
  it("refuses a token that was never minted and returns a fresh quote instead", async () => {
    const quoteFn = makeQuoteFn();
    const executeFn = makeExecuteFn();

    const result = await withSpendGuardrail(
      CREATE,
      baseParams({ confirmation_token: "11111111-2222-3333-4444-555555555555" }),
      quoteFn,
      executeFn,
    );

    expect(executeFn).not.toHaveBeenCalled();
    const quote = asQuote(result);
    expect(quote.confirmationToken).not.toBe("11111111-2222-3333-4444-555555555555");
    expect(quoteFn).toHaveBeenCalledTimes(1);
  });

  it("refuses an empty-string token the same way it refuses an absent one", async () => {
    const executeFn = makeExecuteFn();

    const result = await withSpendGuardrail(
      CREATE,
      baseParams({ confirmation_token: "" }),
      makeQuoteFn(),
      executeFn,
    );

    expect(executeFn).not.toHaveBeenCalled();
    asQuote(result);
  });
});

// ---------------------------------------------------------------------------
// Group 3 — THE HAPPY PATH. Over-enforcement is a real failure mode: if call 2
// cannot land, the user routes around the server and the guardrail protects
// nobody. Execution runs on the QUOTED params, which is what makes the price the
// user saw the price they get charged.
// ---------------------------------------------------------------------------
describe("happy path: quote then confirm executes exactly once", () => {
  it("executes on call 2 with a valid token and identical params", async () => {
    const quoteFn = makeQuoteFn();
    const executeFn = makeExecuteFn();

    const first = asQuote(
      await withSpendGuardrail(CREATE, baseParams(), quoteFn, executeFn),
    );
    const second = await withSpendGuardrail(
      CREATE,
      baseParams({ confirmation_token: first.confirmationToken }),
      quoteFn,
      executeFn,
    );

    expect(executeFn).toHaveBeenCalledTimes(1);
    expect(second).toEqual({ instance_id: "gpu_abc123", status: "provisioning" });
    // quoteFn ran once, for call 1 only — call 2 must not re-price, or the user
    // would be charged against a quote they never saw.
    expect(quoteFn).toHaveBeenCalledTimes(1);
  });

  it("executes with the params the quote resolved, not the params call 2 carried", async () => {
    const executeFn = makeExecuteFn();

    const first = asQuote(
      await withSpendGuardrail(CREATE, baseParams(), makeQuoteFn(), executeFn),
    );
    await withSpendGuardrail(
      CREATE,
      baseParams({ confirmation_token: first.confirmationToken }),
      makeQuoteFn(),
      executeFn,
    );

    // The region/price the quote pinned down are present; confirmation_token is
    // not leaked into the execute call.
    expect(executeFn).toHaveBeenCalledWith(RESOLVED);
    expect(executeFn.mock.calls[0]?.[0]).not.toHaveProperty("confirmation_token");
  });

  it("keeps two in-flight quotes independent — each token executes its own", async () => {
    const executeA = makeExecuteFn();
    const executeB = makeExecuteFn();

    const quoteA = asQuote(
      await withSpendGuardrail(CREATE, baseParams(), makeQuoteFn(), executeA),
    );
    const quoteB = asQuote(
      await withSpendGuardrail(
        CREATE,
        baseParams({ gpu_count: 4 }),
        makeQuoteFn({ resolvedParams: { ...RESOLVED, gpu_count: 4 } }),
        executeB,
      ),
    );

    expect(quoteA.confirmationToken).not.toBe(quoteB.confirmationToken);

    await withSpendGuardrail(
      CREATE,
      baseParams({ gpu_count: 4, confirmation_token: quoteB.confirmationToken }),
      makeQuoteFn(),
      executeB,
    );

    expect(executeB).toHaveBeenCalledTimes(1);
    expect(executeA).not.toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// Group 4 — SINGLE USE. Replay is the double-provision bug: one approval, two
// (or ten) paid instances. The token must burn BEFORE execute runs, so a
// throwing executeFn cannot leave a live token behind for a retry loop to
// re-spend against.
// ---------------------------------------------------------------------------
describe("single-use: a consumed token never executes twice", () => {
  it("refuses a replayed token and returns a fresh quote", async () => {
    const executeFn = makeExecuteFn();

    const first = asQuote(
      await withSpendGuardrail(CREATE, baseParams(), makeQuoteFn(), executeFn),
    );
    const replayParams = baseParams({ confirmation_token: first.confirmationToken });

    await withSpendGuardrail(CREATE, replayParams, makeQuoteFn(), executeFn);
    const replayed = await withSpendGuardrail(CREATE, replayParams, makeQuoteFn(), executeFn);

    expect(executeFn).toHaveBeenCalledTimes(1);
    const requote = asQuote(replayed);
    expect(requote.confirmationToken).not.toBe(first.confirmationToken);
  });

  it("burns the token even when executeFn throws — a failed spend is still a spent token", async () => {
    const boom = vi.fn(async () => {
      throw new Error("provider rejected the request");
    });

    const first = asQuote(
      await withSpendGuardrail(CREATE, baseParams(), makeQuoteFn(), boom),
    );
    const params = baseParams({ confirmation_token: first.confirmationToken });

    await expect(withSpendGuardrail(CREATE, params, makeQuoteFn(), boom)).rejects.toThrow(
      /provider rejected/,
    );

    // Retrying with the same token must NOT re-enter execute: the first attempt
    // may well have created a paid resource before failing.
    const retry = await withSpendGuardrail(CREATE, params, makeQuoteFn(), boom);
    expect(boom).toHaveBeenCalledTimes(1);
    asQuote(retry);
  });
});

// ---------------------------------------------------------------------------
// Group 5 — EXPIRY. A token is a snapshot of a price. Hours later that price may
// be stale and the user's "yes" certainly is. Both directions matter: expired
// must fail, and not-yet-expired must still work (an over-eager clock check
// breaks every legitimate confirmation).
// ---------------------------------------------------------------------------
describe("expiry: tokens are short-lived", () => {
  it("refuses a token used after ttlMs and returns a fresh quote", async () => {
    vi.useFakeTimers();
    const executeFn = makeExecuteFn();

    const first = asQuote(
      await withSpendGuardrail(CREATE, baseParams(), makeQuoteFn(), executeFn, {
        ttlMs: 60_000,
      }),
    );

    vi.advanceTimersByTime(60_001);

    const late = await withSpendGuardrail(
      CREATE,
      baseParams({ confirmation_token: first.confirmationToken }),
      makeQuoteFn(),
      executeFn,
      { ttlMs: 60_000 },
    );

    expect(executeFn).not.toHaveBeenCalled();
    expect(asQuote(late).confirmationToken).not.toBe(first.confirmationToken);
  });

  it("still accepts a token used just inside its ttl", async () => {
    vi.useFakeTimers();
    const executeFn = makeExecuteFn();

    const first = asQuote(
      await withSpendGuardrail(CREATE, baseParams(), makeQuoteFn(), executeFn, {
        ttlMs: 60_000,
      }),
    );

    vi.advanceTimersByTime(59_000);

    await withSpendGuardrail(
      CREATE,
      baseParams({ confirmation_token: first.confirmationToken }),
      makeQuoteFn(),
      executeFn,
      { ttlMs: 60_000 },
    );

    expect(executeFn).toHaveBeenCalledTimes(1);
  });

  it("defaults to a five-minute ttl when none is given", async () => {
    vi.useFakeTimers();
    const executeFn = makeExecuteFn();

    const first = asQuote(
      await withSpendGuardrail(CREATE, baseParams(), makeQuoteFn(), executeFn),
    );

    vi.advanceTimersByTime(300_001);

    await withSpendGuardrail(
      CREATE,
      baseParams({ confirmation_token: first.confirmationToken }),
      makeQuoteFn(),
      executeFn,
    );

    expect(executeFn).not.toHaveBeenCalled();
  });
});

// ---------------------------------------------------------------------------
// Group 6 — PARAMS BOUND. The quote-cheap-execute-expensive gap: quote 1 GPU,
// confirm 8. The token carries the params it was priced for; drift invalidates
// it AND burns it, so an agent cannot grind a cheap token against expensive
// params.
// ---------------------------------------------------------------------------
describe("params-bound: a token only spends what it quoted", () => {
  it("refuses a token when gpu_count drifted 1 -> 2", async () => {
    const executeFn = makeExecuteFn();

    const first = asQuote(
      await withSpendGuardrail(CREATE, baseParams(), makeQuoteFn(), executeFn),
    );
    const drifted = await withSpendGuardrail(
      CREATE,
      baseParams({ gpu_count: 2, confirmation_token: first.confirmationToken }),
      makeQuoteFn({ resolvedParams: { ...RESOLVED, gpu_count: 2 } }),
      executeFn,
    );

    expect(executeFn).not.toHaveBeenCalled();
    expect(asQuote(drifted).confirmationToken).not.toBe(first.confirmationToken);
  });

  it("refuses a token when gpu_type drifted", async () => {
    const executeFn = makeExecuteFn();

    const first = asQuote(
      await withSpendGuardrail(CREATE, baseParams(), makeQuoteFn(), executeFn),
    );
    await withSpendGuardrail(
      CREATE,
      baseParams({ gpu_type: "b200", confirmation_token: first.confirmationToken }),
      makeQuoteFn(),
      executeFn,
    );

    expect(executeFn).not.toHaveBeenCalled();
  });

  it("refuses a token when tier drifted spot -> on_demand", async () => {
    const executeFn = makeExecuteFn();

    const first = asQuote(
      await withSpendGuardrail(
        CREATE,
        baseParams({ tier: "spot" }),
        makeQuoteFn(),
        executeFn,
      ),
    );
    await withSpendGuardrail(
      CREATE,
      baseParams({ tier: "on_demand", confirmation_token: first.confirmationToken }),
      makeQuoteFn(),
      executeFn,
    );

    expect(executeFn).not.toHaveBeenCalled();
  });

  it("invalidates the drifted token, so replaying it with the ORIGINAL params still fails", async () => {
    const executeFn = makeExecuteFn();

    const first = asQuote(
      await withSpendGuardrail(CREATE, baseParams(), makeQuoteFn(), executeFn),
    );
    // Attempt the swap...
    await withSpendGuardrail(
      CREATE,
      baseParams({ gpu_count: 8, confirmation_token: first.confirmationToken }),
      makeQuoteFn(),
      executeFn,
    );
    // ...then fall back to the honest params with the same token. Still no.
    // Otherwise a token is a free oracle to probe params against.
    const fallback = await withSpendGuardrail(
      CREATE,
      baseParams({ confirmation_token: first.confirmationToken }),
      makeQuoteFn(),
      executeFn,
    );

    expect(executeFn).not.toHaveBeenCalled();
    asQuote(fallback);
  });
});

// ---------------------------------------------------------------------------
// Group 7 — NORMALIZATION. The mirror risk to group 6: if key order or an
// explicitly-undefined optional counted as drift, every honest second call would
// be refused. Normalize what arrives (schema-default filling belongs to the tool
// layer, not here).
// ---------------------------------------------------------------------------
describe("normalization: cosmetic differences are not drift", () => {
  it("accepts a second call whose keys are reordered and whose optionals are undefined", async () => {
    const executeFn = makeExecuteFn();

    const first = asQuote(
      await withSpendGuardrail(CREATE, baseParams(), makeQuoteFn(), executeFn),
    );

    const reordered = {
      tier: "on_demand",
      confirmation_token: first.confirmationToken,
      gpu_count: 1,
      ssh_key_id: undefined,
      gpu_type: "h100_sxm",
    };
    await withSpendGuardrail(CREATE, reordered, makeQuoteFn(), executeFn);

    expect(executeFn).toHaveBeenCalledTimes(1);
  });

  it("normalizeParams: reordered keys hash identically", () => {
    expect(normalizeParams({ a: 1, b: 2 })).toBe(normalizeParams({ b: 2, a: 1 }));
  });

  it("normalizeParams: nested object key order does not matter", () => {
    expect(normalizeParams({ env: { B: "2", A: "1" }, n: 1 })).toBe(
      normalizeParams({ n: 1, env: { A: "1", B: "2" } }),
    );
  });

  it("normalizeParams: an explicitly-undefined key equals an absent key", () => {
    expect(normalizeParams({ a: 1, b: undefined })).toBe(normalizeParams({ a: 1 }));
  });

  it("normalizeParams: a changed GPU count or type is DIFFERENT", () => {
    const one = normalizeParams({ gpu_type: "h100_sxm", gpu_count: 1 });
    expect(normalizeParams({ gpu_type: "h100_sxm", gpu_count: 2 })).not.toBe(one);
    expect(normalizeParams({ gpu_type: "b200", gpu_count: 1 })).not.toBe(one);
  });

  it("normalizeParams: array order is meaningful and preserved", () => {
    expect(normalizeParams({ ids: ["a", "b"] })).not.toBe(normalizeParams({ ids: ["b", "a"] }));
  });

  it("normalizeParams: a string 1 is not the number 1", () => {
    // JSON.stringify keeps the distinction; a hand-rolled key=value join would
    // not, and "1" vs 1 is exactly how a count could smuggle past the check.
    expect(normalizeParams({ gpu_count: 1 })).not.toBe(normalizeParams({ gpu_count: "1" }));
  });
});

// ---------------------------------------------------------------------------
// Group 8 — TOOL BOUND. A token minted to create a $2/hr instance must not
// confirm a different tool's action. Binding to params alone is not enough: two
// tools can take the same-shaped params and do wildly different things.
// ---------------------------------------------------------------------------
describe("tool-bound: tokens do not cross tool boundaries", () => {
  it("refuses a create token presented to the terminate tool", async () => {
    const createExecute = makeExecuteFn();
    const terminateExecute = makeExecuteFn();

    const first = asQuote(
      await withSpendGuardrail(CREATE, baseParams(), makeQuoteFn(), createExecute),
    );
    const crossed = await withSpendGuardrail(
      TERMINATE,
      baseParams({ confirmation_token: first.confirmationToken }),
      makeQuoteFn(),
      terminateExecute,
    );

    expect(terminateExecute).not.toHaveBeenCalled();
    expect(createExecute).not.toHaveBeenCalled();
    asQuote(crossed);
  });
});

// ---------------------------------------------------------------------------
// Group 9 — QUOTE FAILURE. If pricing fails, there is no price the user could
// have seen, so there must be no token. The error surfaces; the spend path stays
// shut.
// ---------------------------------------------------------------------------
describe("quote-failure: a failed quote mints nothing", () => {
  it("propagates the quote error without invoking executeFn", async () => {
    const executeFn = makeExecuteFn();
    const quoteFn = vi.fn(async (): Promise<Quote> => {
      throw new Error("pricing unavailable for h100_sxm");
    });

    await expect(
      withSpendGuardrail(CREATE, baseParams(), quoteFn, executeFn),
    ).rejects.toThrow(/pricing unavailable/);

    expect(executeFn).not.toHaveBeenCalled();
  });

  it("leaves no token behind — a later call with any token still only re-quotes", async () => {
    const executeFn = makeExecuteFn();
    const failing = vi.fn(async (): Promise<Quote> => {
      throw new Error("pricing unavailable");
    });

    await expect(
      withSpendGuardrail(CREATE, baseParams(), failing, executeFn),
    ).rejects.toThrow();

    for (const token of ["", "undefined", "00000000-0000-4000-8000-000000000000"]) {
      const result = await withSpendGuardrail(
        CREATE,
        baseParams({ confirmation_token: token }),
        makeQuoteFn(),
        executeFn,
      );
      asQuote(result);
    }

    expect(executeFn).not.toHaveBeenCalled();
  });
});
