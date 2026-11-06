// The single funnel every tool error path goes through.
//
// Two jobs, and neither is optional:
//
//  1. Make failures agent-legible. The generated SDK throws
//     `ResponseError: Response returned an error code` for every non-2xx —
//     a 401, a 402 and a 503 are indistinguishable from the message alone. An
//     agent that cannot tell "your key is bad" from "the server hiccuped" will
//     either retry forever or abandon a working setup. Each status becomes a
//     sentence naming the cause and the fix.
//
//  2. Keep the credential out of the model's context. Error
//     bodies come off the wire and are outside our control; forwarding one
//     verbatim is how a key ends up in a transcript that outlives the process.
//     Nothing here returns an Error whose message has not passed through
//     redactSecrets, and response bodies are never forwarded whole — only a
//     short summary field, truncated and redacted.
//
// Deliberately absent: any logging. On a stdio MCP server stdout is the JSON-RPC
// channel, and an error body written to stderr is still a body written
// somewhere. Errors travel back to the caller as return values, nowhere else.
import { ResponseError } from "@gpuai/sdk";

import {
  API_BASE_ENV,
  API_KEY_ENV,
  type Env,
  redactSecrets as redactConfiguredKey,
  resolveBaseUrl,
} from "./auth.js";

/** What replaces anything credential-shaped. */
const REDACTED = "[redacted]";

/**
 * API keys start with `gpuai_`. The pattern is deliberately
 * lower-case-sensitive: the env var NAME is `GPUAI_API_KEY`, and matching it
 * case-insensitively would redact the very word every setup message needs
 * ("check the [redacted] environment variable").
 */
const TOKEN_SHAPE = /gpuai_[A-Za-z0-9_]+/g;

/** Upper bound on any text lifted out of a response body. */
const MAX_SUMMARY_LENGTH = 320;

/**
 * Strip anything credential-shaped out of arbitrary text.
 *
 * Two passes, because either alone leaves a hole:
 *
 *   - the configured key's literal value, which catches credentials that do not
 *     look like gpuai_ tokens at all (a placeholder, a key from another vendor
 *     pasted into the env var by mistake);
 *   - the `gpuai_…` token shape, which catches a DIFFERENT key echoed back by
 *     the API — one this process never held and so cannot match by value.
 *
 * This is a superset of auth.ts's startup-path redactor and delegates to it for
 * the first pass rather than reimplementing it.
 */
export function redactSecrets(text: string, env: Env = process.env): string {
  return redactConfiguredKey(text, env).replace(TOKEN_SHAPE, REDACTED);
}

/**
 * What replaces a web-console credential in a tool result.
 *
 * Says where the credential actually is, because the agent's next sentence to
 * the user should be useful: `gpu instances open` prints the login. A bare
 * "[redacted]" would read as "gone".
 */
export const CREDENTIAL_REDACTION_NOTICE =
  "[redacted — view with `gpu instances open`]";

/**
 * The keys whose VALUES are credential material.
 *
 * Both spellings of each: the SDK deserializes to camelCase and the tool result
 * path stringifies those model objects, so camelCase is what actually appears
 * today — snake_case is defense-in-depth for any path that ever serializes a
 * raw API body.
 *
 * Exact match, deliberately. A substring or regex test over key names ("starts
 * with app") would also swallow `app_url` and `terminal_url`, which are the
 * whole point of an app or console URL — the link the user is handed. Over-redaction
 * here breaks the product as surely as under-redaction leaks the credential.
 */
const CREDENTIAL_KEYS: ReadonlySet<string> = new Set([
  "appPassword",
  "app_password",
  "appBasicAuthPass",
  "app_basic_auth_pass",
  "appUser",
  "app_user",
  "appBasicAuthUser",
  "app_basic_auth_user",
]);

/**
 * Strip per-instance web-console credentials out of a tool result payload.
 *
 * This extends the error-path rule (API keys never reach the model) to the
 * SUCCESS paths: `gpu_instances_get` and `gpu_operations_get` can carry the
 * HTTP Basic Auth login for the instance's app and browser terminal. The API
 * being able to re-read that credential is by design — the `gpu` CLI shows it —
 * but MCP is model-facing, and a tool result becomes part of a transcript that
 * outlives this process and is outside the operator's control. This layer is the last
 * place the credential can be dropped, so it is dropped here.
 *
 * Key-based rather than value-based: a console password has no token shape to
 * match on (unlike `gpuai_…`), so the only reliable signal is the field it
 * arrived in.
 *
 * Returns a new structure; the input is never mutated. SDK payloads are parsed
 * JSON with no cycles, so plain recursion is sufficient. Only STRING values are
 * replaced — an absent or null credential stays absent or null, because
 * injecting a placeholder would tell the agent a login exists on an instance
 * that has none.
 */
export function sanitizeCredentialFields(value: unknown): unknown {
  if (Array.isArray(value)) {
    return value.map((item) => sanitizeCredentialFields(item));
  }

  // Dates, Maps and the like are left whole: rebuilding them key-by-key would
  // silently turn them into plain objects and change what the agent reads.
  if (
    typeof value !== "object" ||
    value === null ||
    Object.getPrototypeOf(value) !== Object.prototype
  ) {
    return value;
  }

  const out: Record<string, unknown> = {};
  for (const [key, item] of Object.entries(value as Record<string, unknown>)) {
    out[key] =
      CREDENTIAL_KEYS.has(key) && typeof item === "string"
        ? CREDENTIAL_REDACTION_NOTICE
        : sanitizeCredentialFields(item);
  }
  return out;
}

/**
 * Thrown by a spend tool's quote function when the request cannot be priced —
 * no eligible GPU, no listed offering. Throwing (rather than returning a
 * price-less "quote") is what keeps the guardrail from minting a confirmable
 * token for a request with no price: no quote -> no token -> no execute.
 *
 * The mapper preserves this error's wording verbatim (see networkMessage) —
 * without that, a plain throw with no HTTP status reads as the misleading
 * "Could not reach the API" network diagnosis.
 */
export class QuoteUnavailableError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "QuoteUnavailableError";
  }
}

/** Options for {@link mapSdkError}. */
export interface MapErrorOptions {
  /**
   * A short validation summary lifted from the response body — pass the output
   * of {@link summarizeErrorBody}, never a raw body. Used by the 4xx branches.
   */
  details?: string;
  /** Environment override; tests pass one instead of mutating process.env. */
  env?: Env;
}

/**
 * Read the HTTP status off an SDK error, if it is one.
 *
 * Exported so a caller that needs to branch on status BEFORE mapping uses the
 * same detection as the mapper instead of re-deriving it.
 */
export function httpStatusOf(err: unknown): number | undefined {
  if (err instanceof ResponseError) {
    return err.response.status;
  }
  // Duck-typed fallback: a bundler or version skew can produce a structurally
  // identical error that fails instanceof. Behaving differently in that case
  // would turn a dependency detail into a silent guardrail regression.
  const candidate = err as { response?: { status?: unknown } } | null | undefined;
  const status = candidate?.response?.status;
  return typeof status === "number" ? status : undefined;
}

/** Best-effort host of the configured API base, for network-failure messages. */
function baseUrlHost(env: Env): string {
  try {
    return new URL(resolveBaseUrl(env)).host;
  } catch {
    // An unusable override is its own (already-reported) startup problem; a
    // network message must still be produced rather than throwing from a
    // function whose whole job is turning failures into messages.
    return `the configured ${API_BASE_ENV}`;
  }
}

/** Collapse to a single line — multi-line text in a tool error reads as a dump. */
function oneLine(text: string): string {
  return text.replace(/\s+/g, " ").trim();
}

/** Append a validation summary when one was supplied. */
function withDetails(message: string, details: string | undefined): string {
  if (details === undefined || details.trim() === "") {
    return message;
  }
  return `${message} ${oneLine(details)}`;
}

/**
 * Turn any thrown value into an agent-legible Error.
 *
 * `context` is a short phrase naming what was being attempted
 * ("listing instances", "getting operation <id>"). It appears in the message so
 * a failure surfacing several tool calls deep is still attributable.
 *
 * Never returns the SDK's own error, and never includes a response body beyond
 * the caller-supplied `details` summary.
 */
export function mapSdkError(
  err: unknown,
  context: string,
  options: MapErrorOptions = {},
): Error {
  const env = options.env ?? process.env;
  const details = options.details;
  const status = httpStatusOf(err);

  return new Error(redactSecrets(oneLine(buildMessage(status, context, details, env, err)), env));
}

function buildMessage(
  status: number | undefined,
  context: string,
  details: string | undefined,
  env: Env,
  err: unknown,
): string {
  if (status === undefined) {
    // A schema rejection never left the process. Describing it as a network
    // fault would send the agent debugging connectivity instead of fixing its
    // parameters — the caller-supplied issue summary is the actionable part.
    // Name-matched rather than instanceof, like the networkMessage cases: a
    // bundler or version skew must not silently turn this back into a
    // misleading network diagnosis.
    if (err instanceof Error && err.name === "ZodError") {
      return withDetails(`Invalid parameters (while ${context}).`, details);
    }
    return networkMessage(context, env, err);
  }

  switch (status) {
    case 401:
      // Deliberately says nothing about the request that
      // failed beyond the context — a 401 is about the credential, and adding
      // response detail here is the most likely place to leak header content.
      return `API key invalid or revoked — check the ${API_KEY_ENV} environment variable in your MCP client configuration (while ${context}).`;
    case 403:
      return withDetails(
        `Not permitted — this API key lacks the scope required for ${context}, or the account lacks the role. A read-only key cannot provision.`,
        details,
      );
    case 404:
      return withDetails(`Not found — ${context} referred to something that does not exist or is already gone.`, details);
    case 409:
      return withDetails(`Conflict — ${context} collided with existing state.`, details);
    case 422:
      // The summary only. The full body is where a caller-echoed credential
      // would live, and it is also the least useful part for the agent.
      return withDetails(`Invalid request — ${context} was rejected as unprocessable.`, details);
    case 429:
      return `Rate limited — wait a moment and retry (while ${context}). Do not retry in a tight loop.`;
    default:
      break;
  }

  if (status >= 500) {
    return serverErrorMessage(status, context, env);
  }
  if (status >= 400) {
    return withDetails(`Request rejected (HTTP ${status}) while ${context}.`, details);
  }
  return `Unexpected response (HTTP ${status}) while ${context}.`;
}

/**
 * 5xx, with one special case.
 *
 * GET /v1/operations/{id} can return 500 rather than a 4xx for a malformed id,
 * so the mapping absorbs it: an agent polling with a mistyped id gets pointed
 * at the id instead of concluding the server is down and abandoning an
 * instance that is still running.
 */
function serverErrorMessage(status: number, context: string, _env: Env): string {
  if (/operation/i.test(context)) {
    return `Server error (HTTP ${status}) while ${context}. A malformed operation id is a known cause of this — check the operation id format (it is a UUID) before retrying.`;
  }
  return `Server error (HTTP ${status}) while ${context} — the API is temporarily unavailable. Retry shortly.`;
}

/**
 * No HTTP status: the request never completed.
 *
 * Names the host so the operator can tell "the internet is down" from "my
 * GPUAI_API_BASE override points somewhere wrong", without pasting the error
 * chain (which carries resolved addresses and syscall noise the agent cannot
 * act on).
 */
function networkMessage(context: string, env: Env, err: unknown): string {
  const host = baseUrlHost(env);
  if (
    err instanceof Error &&
    (err.name === "StartupConfigError" || err.name === "QuoteUnavailableError")
  ) {
    // Already a diagnosis in its own words — a configuration problem, or a
    // request that cannot be priced. Keep that wording rather than
    // re-describing either as a network fault.
    return `${err.message} (while ${context}).`;
  }
  return `Could not reach the API at ${host} while ${context} — check that the gpuctl server is running and the ${API_BASE_ENV} setting.`;
}

/** Fields carrying a short human message, in the order they are preferred. */
function pickSummary(parsed: unknown): string | undefined {
  if (typeof parsed !== "object" || parsed === null) {
    return undefined;
  }
  const body = parsed as Record<string, unknown>;

  // RFC 7807 problem+json (the /v1 platform surface).
  if (typeof body.detail === "string" && body.detail.trim() !== "") {
    return body.detail;
  }
  // OpenAI-style `{ error: { message } }` envelope.
  const nested = body.error;
  if (typeof nested === "object" && nested !== null) {
    const message = (nested as Record<string, unknown>).message;
    if (typeof message === "string" && message.trim() !== "") {
      return message;
    }
  }
  if (typeof body.message === "string" && body.message.trim() !== "") {
    return body.message;
  }
  if (typeof body.title === "string" && body.title.trim() !== "") {
    return body.title;
  }
  return undefined;
}

/**
 * Lift a short, redacted summary out of an SDK error's response body.
 *
 * Returns undefined rather than falling back to the raw body whenever the body
 * is not recognisably structured — an HTML error page or an opaque proxy
 * response has nothing useful in it and everything risky. Fail closed.
 */
export async function summarizeErrorBody(
  err: unknown,
  env: Env = process.env,
): Promise<string | undefined> {
  const response = (err as { response?: Response } | null | undefined)?.response;
  if (!response || typeof response.text !== "function") {
    return undefined;
  }

  let raw: string;
  try {
    // clone() so a caller that also wants the body is not left with a consumed
    // stream — and so calling this twice cannot throw.
    raw = await response.clone().text();
  } catch {
    return undefined;
  }

  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return undefined;
  }

  const summary = pickSummary(parsed);
  if (summary === undefined) {
    return undefined;
  }

  const cleaned = oneLine(summary);
  const truncated =
    cleaned.length > MAX_SUMMARY_LENGTH ? `${cleaned.slice(0, MAX_SUMMARY_LENGTH - 1)}…` : cleaned;
  return redactSecrets(truncated, env);
}

/**
 * The funnel tool handlers call: summarise the body, then map.
 *
 * Split from {@link mapSdkError} only because reading a body is async and the
 * mapping itself must stay synchronously testable. 401 deliberately ignores the
 * summary — see the 401 branch above.
 */
export async function mapSdkErrorAsync(
  err: unknown,
  context: string,
  env: Env = process.env,
): Promise<Error> {
  const status = httpStatusOf(err);
  const details = status === 401 ? undefined : await summarizeErrorBody(err, env);
  return mapSdkError(err, context, { env, details });
}
