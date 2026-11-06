// The two-call spend handshake — the server's core safety mechanism.
//
// The spend-confirmation contract is "show the price, get an explicit yes,
// never infer the GPU count." An MCP server hands a spend-capable credential to
// an arbitrary model in an arbitrary client, and an ignorable guardrail on a
// spend surface is no guardrail. So the contract is expressed STRUCTURALLY here.
//
// The rule, in one sentence: a spend tool executes nothing on its first call.
// It prices the request, mints a token bound to that price, and returns both.
// Only a second call carrying that exact token — for this tool, for these exact
// params, unused, unexpired — reaches executeFn.
//
// Consequences worth stating, because each closes a specific hole:
//
//   * "Price shown first" is enforced by construction: no quote -> no token ->
//     no execute. There is no code path that spends without pricing first.
//   * Execution runs on the params the QUOTE resolved, not on whatever the
//     second call carried. Quote-cheap-execute-expensive is not merely checked,
//     it is unrepresentable.
//   * The token burns before executeFn runs, so a failed or retried execute
//     cannot re-spend against the same approval.
//   * None of this depends on the client. Elicitation gives the same contract
//     nicer UX where it exists; where it does not, this holds unchanged. That is
//     why the module takes no server, transport, or capability object.
//
// A second tier needs the same guarantee for a different reason: a write tool
// has no price, but creating an SSH key is still not something a prompt-injected agent
// should be able to do without asking. `withWriteConfirmation` is that entry
// point, and it is a DELEGATION, not a copy — one token store, one
// normalization, one single-use burn, so safety scales with tiers rather than
// with tools. The only difference is the wording of the instructions.
//
// Deliberately NOT here: any import of the API client, any network call,
// any logging. (Named without the package specifier on purpose, so a literal
// "does this file import the SDK" grep stays a clean check.)
// The guardrail layer is transport-agnostic pure logic — that keeps it directly
// unit-testable, which is where most guardrail regression signal lives. It also never sees the API key, so it cannot leak one.
import { randomUUID } from "node:crypto";

/** Default token lifetime: five minutes. Long enough for a human to read a
 * price and answer, short enough that a stale approval cannot be cashed in
 * against a price that has since moved. */
export const DEFAULT_TTL_MS = 300_000;

/**
 * What a tool's quote function produces: the human-legible price line, and the
 * fully-resolved params that price applies to.
 *
 * `resolvedParams` is normally RICHER than what the agent sent — a region
 * picked, a default filled, a rate looked up. That richer object is what gets
 * executed later, which is what ties the charge to the quote.
 */
export interface Quote {
  /** Human-legible price line, e.g. "1x h100_sxm (on_demand) at $2.49/hr". */
  summary: string;
  /** The exact params this price applies to — and the params that will run. */
  resolvedParams: Record<string, unknown>;
}

/**
 * The first-call response: the quote, plus the minted token and the text that
 * tells the agent what to do with it.
 *
 * This is a RESPONSE, not an error. A refusal that reads as an error invites a
 * retry-with-force; a quote puts the agent back on the only path that works.
 */
export interface QuoteResult extends Quote {
  /** Discriminator so a tool handler can branch without guessing. */
  status: "confirmation_required";
  /** Single-use, short-lived, bound to (tool, params). */
  confirmationToken: string;
  /** Remaining validity, in whole seconds — for the agent to relay honestly. */
  expiresInSeconds: number;
  /** Agent-facing instructions naming the token and the required second call. */
  instructions: string;
}

export interface HandshakeOptions {
  /** Token lifetime in milliseconds. Defaults to {@link DEFAULT_TTL_MS}. */
  ttlMs?: number;
}

/** The param name every spend tool carries for the second call. */
export const CONFIRMATION_TOKEN_PARAM = "confirmation_token";

interface TokenRecord {
  toolName: string;
  /** Normalized form of the params this token was quoted for. */
  paramsHash: string;
  /** The params that will actually execute — the quote's resolution. */
  resolvedParams: Record<string, unknown>;
  expiresAt: number;
  used: boolean;
}

/**
 * In-process token store.
 *
 * A stdio MCP server is one process serving one client, so process memory is
 * the correct scope: a token must not survive a restart (the user's approval
 * did not), and there is no second replica to share it with. An external store
 * would add a dependency, a failure mode, and a persistence window — all of them
 * making the guarantee weaker, not stronger.
 */
const tokenStore = new Map<string, TokenRecord>();

/** Drop expired records. Called on access, so no timer keeps the process alive. */
function pruneExpired(now: number): void {
  for (const [token, record] of tokenStore) {
    if (record.expiresAt <= now) {
      tokenStore.delete(token);
    }
  }
}

/** Test-only: clear the store so cases cannot leak tokens into each other. */
export function _resetTokenStoreForTests(): void {
  tokenStore.clear();
}

/**
 * Reduce params to a stable string for identity comparison.
 *
 * Deep key-sorted, `undefined` dropped, then JSON. Two rules follow, and both
 * matter:
 *
 *   * Cosmetic differences are NOT drift. Reordered keys and explicitly-passed
 *     `undefined` optionals must compare equal, or every honest second call gets
 *     refused and the user routes around the server entirely.
 *   * Substantive differences ARE drift. A changed GPU type or count must not
 *     compare equal — that is the quote-cheap-execute-expensive gap.
 *
 * JSON is what keeps the second rule sharp: it preserves the string/number
 * distinction (`"1"` vs `1`) and array ORDER, both of which a hand-rolled
 * key=value join would flatten away.
 *
 * Schema-default filling is deliberately NOT done here — the tool layer owns
 * its own defaults. This normalizes what actually arrives.
 */
export function normalizeParams(params: Record<string, unknown>): string {
  return JSON.stringify(sortDeep(params));
}

function sortDeep(value: unknown): unknown {
  if (Array.isArray(value)) {
    // Array order is meaningful (a list of ssh keys, a device ordering), so it
    // is preserved; only the elements are normalized.
    return value.map(sortDeep);
  }
  if (value !== null && typeof value === "object") {
    const source = value as Record<string, unknown>;
    const sorted: Record<string, unknown> = {};
    for (const key of Object.keys(source).sort()) {
      const entry = source[key];
      // An explicitly-undefined optional is the same request as an absent one.
      if (entry === undefined) continue;
      sorted[key] = sortDeep(entry);
    }
    return sorted;
  }
  return value;
}

/** Strip the confirmation token before hashing — it is the envelope, not the request. */
function paramsWithoutToken(params: Record<string, unknown>): Record<string, unknown> {
  const { [CONFIRMATION_TOKEN_PARAM]: _token, ...rest } = params;
  return rest;
}

/**
 * Which contract a confirmation is being asked under.
 *
 * The MACHINERY is identical for both — same store, same normalization, same
 * single-use burn — and that is the point of naming the difference here rather
 * than in a second module: the only thing a write confirmation does differently
 * from a spend confirmation is what it asks the user to look at. A spend
 * confirmation is about a price; a write confirmation is about a change that has
 * no price but is still not reversible by re-reading. Everything else — no
 * quote -> no token, execute the quote's params, burn before execute — is the
 * same code path, so a tightening to one is a tightening to both.
 */
type ConfirmationFlavor = "spend" | "write";

function buildInstructions(token: string, flavor: ConfirmationFlavor): string {
  const subject = flavor === "spend" ? "this price" : "this summary of what will change";
  const nothingHappened =
    flavor === "spend"
      ? "Nothing has been created and nothing has been charged by this call."
      : "Nothing has been changed by this call.";

  return [
    `Show the user ${subject} and get an explicit yes, then call this tool again`,
    `with ${CONFIRMATION_TOKEN_PARAM} set to ${token} and identical parameters.`,
    ``,
    `${nothingHappened} Do not`,
    `assume approval, and do not change the parameters — any change invalidates`,
    `the token and requires a fresh quote.`,
  ].join("\n");
}

/**
 * Wrap a spend-capable tool in the two-call handshake.
 *
 * Call 1 (no valid token) prices the request and returns a {@link QuoteResult}.
 * Call 2 (valid token, identical params) runs `executeFn` on the QUOTED params
 * and returns its result.
 *
 * Every rejection path re-quotes rather than erroring: an unknown token, an
 * expired one, a used one, one minted for a different tool, one minted for
 * different params. The agent always gets a usable next step, and the only next
 * step available is the one that shows the user a price.
 *
 * @param toolName  the tool this call is for; tokens never cross tools
 * @param params    the agent-supplied params, possibly carrying a token
 * @param quoteFn   prices the request — must not spend anything
 * @param executeFn performs the spend; only ever reached via a valid token
 */
export async function withSpendGuardrail<P extends Record<string, unknown>, R>(
  toolName: string,
  params: P & { confirmation_token?: string },
  quoteFn: (p: P) => Promise<Quote>,
  executeFn: (p: P) => Promise<R>,
  opts: HandshakeOptions = {},
): Promise<QuoteResult | R> {
  return await withConfirmation("spend", toolName, params, quoteFn, executeFn, opts);
}

/**
 * Wrap a non-spend MUTATION in the same single confirmation.
 *
 * This is deliberately a thin delegation rather than a parallel implementation.
 * A write tool creates an SSH key that grants persistent access, deletes one,
 * or renames an instance — none of those have a price, and all of them are things a prompt-injected agent should
 * not be able to do silently. The confirmation they need is structurally the
 * spend confirmation with the price line replaced by a consequence sentence.
 *
 * Sharing the store is the safety property, not an economy of lines. A second
 * `Map` here would be a second place tokens can be minted, a second TTL to keep
 * in step, and a second normalization to drift out of agreement with
 * {@link normalizeParams} — which is exactly the quote-cheap-execute-expensive
 * gap, re-opened one tier down. There is one token store in this process and
 * this function does not add another.
 *
 * @param toolName  the tool this call is for; tokens never cross tools
 * @param params    the agent-supplied params, possibly carrying a token
 * @param quoteFn   describes the consequence — must not mutate anything
 * @param executeFn performs the mutation; only ever reached via a valid token
 */
export async function withWriteConfirmation<P extends Record<string, unknown>, R>(
  toolName: string,
  params: P & { confirmation_token?: string },
  quoteFn: (p: P) => Promise<Quote>,
  executeFn: (p: P) => Promise<R>,
  opts: HandshakeOptions = {},
): Promise<QuoteResult | R> {
  return await withConfirmation("write", toolName, params, quoteFn, executeFn, opts);
}

/** The one implementation both tiers run through. */
async function withConfirmation<P extends Record<string, unknown>, R>(
  flavor: ConfirmationFlavor,
  toolName: string,
  params: P & { confirmation_token?: string },
  quoteFn: (p: P) => Promise<Quote>,
  executeFn: (p: P) => Promise<R>,
  opts: HandshakeOptions = {},
): Promise<QuoteResult | R> {
  const ttlMs = opts.ttlMs ?? DEFAULT_TTL_MS;
  const now = Date.now();
  pruneExpired(now);

  const requestParams = paramsWithoutToken(params) as P;
  const presented = params[CONFIRMATION_TOKEN_PARAM];

  if (typeof presented === "string" && presented !== "") {
    const record = tokenStore.get(presented);

    if (record !== undefined && !record.used && record.expiresAt > now) {
      const matches =
        record.toolName === toolName &&
        record.paramsHash === normalizeParams(requestParams);

      // Burn on ANY live-token presentation, match or mismatch. On a match this
      // is the single-use guarantee, and it happens BEFORE executeFn so a throw
      // or a retry cannot re-spend the same approval. On a mismatch it denies an
      // attacker a free oracle: one token, one attempt, no grinding params
      // against a price the user already approved.
      record.used = true;
      tokenStore.delete(presented);

      if (matches) {
        // Execute the QUOTE's params, not the caller's. The user approved a
        // price; this is what makes that price the thing that runs.
        return await executeFn(record.resolvedParams as P);
      }
    }
    // Anything else — unknown, expired, already used, wrong tool, drifted
    // params — falls through to a fresh quote below.
  }

  // No valid token: price it, mint a token bound to (tool, params), return both.
  // quoteFn throwing propagates unchanged and mints nothing: if pricing failed
  // there is no price the user could have seen, so there must be no token. Error
  // taxonomy belongs to the tool layer, not to the guardrail.
  const quote = await quoteFn(requestParams);

  const token = randomUUID();
  tokenStore.set(token, {
    toolName,
    paramsHash: normalizeParams(requestParams),
    resolvedParams: quote.resolvedParams,
    expiresAt: Date.now() + ttlMs,
    used: false,
  });

  return {
    status: "confirmation_required",
    summary: quote.summary,
    resolvedParams: quote.resolvedParams,
    confirmationToken: token,
    expiresInSeconds: Math.floor(ttlMs / 1000),
    instructions: buildInstructions(token, flavor),
  };
}
