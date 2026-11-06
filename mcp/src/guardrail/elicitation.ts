// The elicitation path — the same spend contract, with nicer UX.
//
// Elicitation lets a server ask the user a question mid-tool-call, so a
// confirmation can be a dialog the human answers rather than a token the model
// relays. It is the better experience where it exists, and it is genuinely new
// (spec revision 2025-06-18), which means it does not exist everywhere.
//
// The rule this module is built around: elicitation is never a SECOND way to
// spend. It is the same handshake with a friendlier prompt in the middle.
//
// That is enforced by what this module does NOT contain. There is no call to
// any execute function here, no token minting, and no import of the API client.
// All this module does is ask a question and report the answer. The spend-tool
// wrapper owns both withSpendGuardrail calls: it mints a token exactly as an
// ordinary first call would, asks here, and then spends that token back through
// withSpendGuardrail. So every invariant the handshake owns — execution runs
// the QUOTE's resolved params, one token one attempt, burn-before-execute,
// nothing runs that was not priced — holds identically on both paths, because
// it is literally the same code enforcing them.
//
// The alternative (branch: if elicitation, call executeFn directly) would be
// shorter and is the shape most servers take. It is also how the two paths
// drift: the handshake gets a tightening, the elicitation branch does not, and
// the weaker path is the one an adversarial client will choose.
//
// Failing safe, in every direction:
//
//   * client has no elicitation capability  -> "unavailable" (handshake stands)
//   * elicitInput throws / times out        -> "unavailable" (handshake stands)
//   * user declines or cancels              -> "declined", and the caller drops
//                                              the token on the floor, so there
//                                              is nothing left to cash in
//   * anything other than an explicit true  -> "declined". Silence is not yes.
//
// The prompt text NEVER contains the API key or any credential. Form-mode
// elicitation data is not protected the way an OAuth/URL-mode flow is, and
// The never-echo-the-key rule extends to anything shown to a user.
import type { QuoteResult } from "./handshake.js";

/**
 * The slice of the MCP server this module needs.
 *
 * Deliberately structural rather than an import of the MCP SDK's `Server`:
 * the guardrail layer stays unit-testable with a plain object, and a test can
 * model "a client that claims elicitation and then throws" without standing up
 * a transport. The registry adapts the real server to this shape at the one
 * boundary where the two meet.
 *
 * NOTE on the pinned SDK: `elicitInput` lives on the low-level `Server`
 * (`server.server.elicitInput`), NOT on the per-request `extra` object that
 * tool callbacks receive — `RequestHandlerExtra` in @modelcontextprotocol/sdk
 * 1.30.0 exposes signal/sessionId/sendRequest/sendNotification and no
 * elicitation helper.
 */
export interface ElicitationHost {
  /** Client capabilities from the completed initialize handshake. */
  getClientCapabilities(): { elicitation?: unknown } | undefined;
  /** Ask the user a structured question. */
  elicitInput(params: ElicitFormRequest): Promise<ElicitOutcome>;
}

/** Form-mode elicitation request — the only mode used here. */
export interface ElicitFormRequest {
  mode?: "form";
  message: string;
  requestedSchema: {
    type: "object";
    properties: Record<string, unknown>;
    required?: string[];
  };
}

/** What a client answers with. `action` is accept / decline / cancel. */
export interface ElicitOutcome {
  action: string;
  content?: Record<string, unknown>;
}

/**
 * The user said no.
 *
 * Carries the summary they were shown (so the agent can report accurately what
 * was declined) and deliberately NO confirmation token: a token the agent never
 * receives is a token that cannot be replayed, which is what makes "declined"
 * mean declined rather than "declined, for now".
 */
export interface DeclinedResult {
  status: "declined";
  /** The price line the user saw and rejected. */
  summary: string;
  /** Agent-facing instruction — do not retry unprompted. */
  message: string;
}

/** The field name the confirmation form asks for. */
const CONFIRM_FIELD = "confirm";

/** True when the connected client advertised the elicitation capability. */
export function clientSupportsElicitation(host?: ElicitationHost): boolean {
  if (host === undefined || typeof host.elicitInput !== "function") {
    return false;
  }
  if (typeof host.getClientCapabilities !== "function") {
    // No way to ask -> assume no. Falling back to the handshake costs one extra
    // round trip; assuming support that is absent costs a hung tool call.
    return false;
  }
  let capabilities: { elicitation?: unknown } | undefined;
  try {
    capabilities = host.getClientCapabilities();
  } catch {
    return false;
  }
  return capabilities?.elicitation !== undefined;
}

/** Narrow the handshake's union without a cast at every call site. */
export function isQuoteResult(value: unknown): value is QuoteResult {
  return (
    typeof value === "object" &&
    value !== null &&
    (value as { status?: unknown }).status === "confirmation_required"
  );
}

/** Narrow a declined result. */
export function isDeclinedResult(value: unknown): value is DeclinedResult {
  return (
    typeof value === "object" &&
    value !== null &&
    (value as { status?: unknown }).status === "declined"
  );
}

/**
 * Build the prompt the user actually reads.
 *
 * The price line comes first because it is the thing consent is about. The
 * resolved params follow so "1x h100_sxm" is not the whole story a user
 * approves — region, tier, and disk are visible too. Nothing here is derived
 * from the environment, so the credential cannot reach the prompt by accident.
 */
function buildPrompt(quote: QuoteResult, action: string): string {
  const params = Object.entries(quote.resolvedParams)
    .filter(([, value]) => value !== undefined && value !== null)
    .map(([key, value]) => `  ${key}: ${formatValue(value)}`)
    .join("\n");

  return [
    quote.summary,
    "",
    params === "" ? "(no additional parameters)" : params,
    "",
    `${action} Nothing has been created and nothing has been charged yet.`,
  ].join("\n");
}

function formatValue(value: unknown): string {
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  return JSON.stringify(value);
}

/** The three answers a prompt attempt can produce. */
export type ElicitationAnswer = "confirmed" | "declined" | "unavailable";

/**
 * Ask the user to confirm a quote, if the client can ask at all.
 *
 * Returns `"unavailable"` — never throws — when there is no client, no
 * elicitation capability, or the prompt fails. The caller treats that as "use
 * the handshake", which is a complete answer on its own, so a client's
 * elicitation problems can never turn a working tool into a broken one.
 *
 * `"confirmed"` requires an explicit `accept` AND an explicit boolean `true`.
 * Every other shape — a decline, a cancel, an accept with the box unticked, a
 * missing field, a string "true" — is `"declined"`. Silence is not consent, and
 * a permissive coercion here would be a permissive coercion about money.
 */
export async function elicitConfirmation(
  host: ElicitationHost | undefined,
  quote: QuoteResult,
  question: string,
): Promise<ElicitationAnswer> {
  if (!clientSupportsElicitation(host)) {
    return "unavailable";
  }

  let outcome: ElicitOutcome;
  try {
    outcome = await host!.elicitInput({
      mode: "form",
      message: buildPrompt(quote, question),
      requestedSchema: {
        type: "object",
        properties: {
          [CONFIRM_FIELD]: {
            type: "boolean",
            title: "Confirm",
            description: "Yes — go ahead and run this at the price shown above.",
          },
        },
        required: [CONFIRM_FIELD],
      },
    });
  } catch {
    // A client that advertises elicitation and then fails is a client without
    // working elicitation. The quote (with its token) is still a complete,
    // usable answer, so the tool degrades to the handshake instead of erroring.
    return "unavailable";
  }

  return outcome?.action === "accept" && outcome.content?.[CONFIRM_FIELD] === true
    ? "confirmed"
    : "declined";
}

/** The standard declined payload, built from the quote the user rejected. */
export function declinedResult(quote: QuoteResult): DeclinedResult {
  return {
    status: "declined",
    summary: quote.summary,
    message:
      "The user declined. Nothing was created and nothing was charged. Do not retry this " +
      "call unless the user asks for it again — a decline is an answer, not a timeout.",
  };
}
