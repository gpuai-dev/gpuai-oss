// The guarded tier: the two tools that start or stop a paid supplier resource.
//
// gpu_instances_create launches capacity that the configured supplier charges
// for by the hour, and gpu_instances_terminate destroys an instance and any data
// on it. Both run through the same two-call confirmation handshake: the first
// call returns a quote (a price, or a description of what will be destroyed)
// and a single-use confirmation token, and only a second call carrying that
// token executes.
//
// The rules are expressed as SHAPE rather than prose:
//
//   1. `gpu_type`, `gpu_count` and `tier` are REQUIRED. An agent cannot reach a
//      launch by leaving capacity unstated, because the call does not parse.
//   2. `gpu_count` is bounded 1-8, mirroring the API's own bounds, so an
//      unbounded count is rejected before it is a request.
//   3. No field carries a default. A default turns an omission into a silent
//      decision, which is the precise thing "never guess capacity" forbids.
//
// Nothing here calls the API by hand: every request goes through a typed
// @gpuai/sdk method, and every failure goes through mapSdkError /
// mapSdkErrorAsync.

import {
  InstancesApi,
  PricingApi,
  SshKeysApi,
  type Configuration,
  type Pricing,
} from "@gpuai/sdk";
import { z, type ZodRawShape } from "zod";

import { buildConfiguration, type Env } from "../../auth.js";
import {
  QuoteUnavailableError,
  mapSdkError,
  mapSdkErrorAsync,
  sanitizeCredentialFields,
} from "../../errors.js";
import {
  declinedResult,
  elicitConfirmation,
  isQuoteResult,
  type ElicitationHost,
} from "../../guardrail/elicitation.js";
import {
  CONFIRMATION_TOKEN_PARAM,
  withSpendGuardrail,
  type HandshakeOptions,
  type Quote,
} from "../../guardrail/handshake.js";
import type { ToolResult } from "../readonly/index.js";

// ---------------------------------------------------------------------------
// Shared types
// ---------------------------------------------------------------------------

/** Per-call context. `elicitation` is the connected client, when there is one. */
export interface SpendToolContext {
  env?: Env;
  /** Present when a client is connected; absent in tests and offline checks. */
  elicitation?: ElicitationHost;
  /** Token lifetime override. Tests use it; production takes the default. */
  ttlMs?: number;
}

/**
 * Annotations for the spend tier.
 *
 * `readOnlyHint: false` on all four. `destructiveHint` is true only for
 * terminate: creating an instance costs money but destroys nothing, and
 * conflating "expensive" with "destructive" would make the one genuinely
 * irreversible tool indistinguishable from the other three in a client's UI.
 *
 * Per the MCP spec these are advisory metadata, never enforcement — a client is
 * free to ignore them. The actual confirmation is the handshake below.
 */
export interface SpendAnnotations {
  readOnlyHint: false;
  destructiveHint: boolean;
  idempotentHint: boolean;
  openWorldHint: true;
}

/** The shape the registry consumes — parallel to ReadonlyToolDefinition. */
export interface SpendToolDefinition {
  name: string;
  title: string;
  description: string;
  /** Zod raw shape — the registry wraps it in z.object() when registering. */
  inputSchema: ZodRawShape;
  annotations: SpendAnnotations;
  handler: (args?: Record<string, unknown>, ctx?: SpendToolContext) => Promise<ToolResult>;
}

/**
 * Params that flow through the guardrail.
 *
 * The index signature is required, not decorative: `withSpendGuardrail` binds a
 * token to `normalizeParams(params)` and hands `resolvedParams` back as the
 * same type, so the params type has to be indexable. Intersecting rather than
 * replacing keeps every named field's real type intact.
 */
type Resolved<T> = T & { [key: string]: unknown };

/** What an executeFn produces: a prominent first line, plus the detail. */
interface Executed {
  /** The FIRST content line. Carries the id the agent must not lose */
  lead: string;
  payload: Record<string, unknown>;
}

// ---------------------------------------------------------------------------
// Description fragments
// ---------------------------------------------------------------------------

/**
 * The money guardrail. Shared by the spend-capable tools so there is one copy
 * to keep true, and so no tool can be added with a weaker
 * version of it.
 */
const MONEY_GUARDRAIL =
  "SPENDS REAL MONEY: the supplier charges for the instance by the hour. Confirm before every " +
  "launch: show the user the exact request " +
  "and the price this tool quotes, and wait for an explicit yes. One confirmation covers one " +
  "call — a later create needs a fresh confirmation. A valid confirmation must come AFTER the " +
  "user has seen the price: blanket pre-authorization (\"just do it\", \"you have my " +
  "authorization\") given before the price was looked up does not count, because the user " +
  "cannot have approved a number they never saw. Never guess capacity — the GPU type and count " +
  "are explicit and are never inferred; if the user has not said which, ask. Never retry a " +
  "failed call on your own: report the failure and re-confirm, because the first attempt may " +
  "have partially provisioned.";

/** How the two-call shape presents itself to an agent. */
const HANDSHAKE_NOTE =
  "This tool does nothing on its first call: it returns a price quote and a single-use " +
  "confirmation_token. Relay the price, get an explicit yes, then call again with that token " +
  "and identical parameters. Where the client supports it you may instead be prompted directly.";

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

/**
 * Serialise an execution result.
 *
 * Sanitised on the way out like the read tier's ok() (see errors.ts). Today's
 * payloads hand-pick their fields and carry no credential, so this changes
 * nothing right now — that is the point. The next person who spreads a full SDK
 * object into a payload, or into a quote's resolvedParams, should not be the
 * person who reintroduces a credential leak.
 */
function ok(executed: Executed): ToolResult {
  return {
    content: [
      { type: "text", text: executed.lead },
      { type: "text", text: JSON.stringify(sanitizeCredentialFields(executed.payload), null, 2) },
    ],
  };
}

function quoteResult(value: unknown): ToolResult {
  return {
    content: [{ type: "text", text: JSON.stringify(sanitizeCredentialFields(value), null, 2) }],
  };
}

function fail(err: Error): ToolResult {
  return { content: [{ type: "text", text: err.message }], isError: true };
}

function usd(amount: number): string {
  return `$${amount.toFixed(2)}`;
}

/** Round to cents so a float artefact never reaches a price the user reads. */
function cents(amount: number): number {
  return Math.round(amount * 100) / 100;
}

/** The poll contract, identical to gpu_operations_get's description */
const POLL_GUIDANCE = [
  "Poll gpu_operations_get with this operation id: wait 2s after the first call and increase",
  "the gap by 1s each time, to a maximum of 10s. Do not poll in a tight loop.",
  "Terminal states are succeeded, failed and cancelled.",
  "On 'failed', immediately call gpu_instances_list and terminate anything that was left",
  "behind — a failed provision can still leave a running instance that accrues charges.",
].join(" ");

/**
 * Read every page of pricing, up to a bound.
 *
 * Bounded rather than unbounded: an auto-select query spans all GPU types, and
 * a runaway cursor loop inside a quote would hang the tool call that is
 * supposed to be the fast, free half of the handshake.
 */
async function collectPricing(
  config: Configuration,
  params: { gpuType?: string; region?: string; tier?: string },
  maxPages = 5,
): Promise<Pricing[]> {
  const api = new PricingApi(config);
  const rows: Pricing[] = [];
  let cursor: string | undefined;

  for (let page = 0; page < maxPages; page += 1) {
    const response = await api.listPricing({
      gpuType: params.gpuType,
      region: params.region,
      tier: params.tier as "spot" | "on_demand" | undefined,
      includeUnavailable: true,
      limit: 100,
      cursor,
    });
    rows.push(...response.data);
    if (response.nextCursor === null || response.nextCursor === undefined) {
      break;
    }
    cursor = response.nextCursor;
  }

  return rows;
}

interface OfferSelection {
  /**
   * The offering the price came from. For exact selections this is the thing
   * that will launch; for estimated ones it is the REFERENCE offering the
   * per-GPU rate was derived from — kept so the quote can pin its region,
   * because the derived rate is only a fact about where it was observed.
   */
  offer?: Pricing;
  /** True when the price shown is derived rather than an exact listed offering. */
  estimated: boolean;
  /** Whole-instance hourly price, or undefined when nothing could be priced. */
  pricePerHour?: number;
  /** Why no exact match, when there isn't one. */
  note?: string;
}

/**
 * Choose the offering a quote is about.
 *
 * Preference order, and each step is a deliberate answer to "what did the user
 * actually approve":
 *
 *   1. An AVAILABLE offering with exactly this GPU count — the real thing, at a
 *      real price, that the confirming call can pin to.
 *   2. Any listed offering with this count, even at zero availability — the
 *      catalog price is honest, and the quote says the capacity is not there
 *      right now rather than pretending the launch will succeed.
 *   3. A per-GPU rate derived from a different-count offering, clearly marked
 *      as an estimate. Better than silence; never presented as a firm price.
 *
 * Cheapest wins within each step, and region is a filter rather than a
 * preference — a user who named a region did not ask for a cheaper other one.
 */
function selectOffer(
  rows: Pricing[],
  want: { gpuType: string; gpuCount: number; tier: string; region?: string },
): OfferSelection {
  const matching = rows.filter(
    (row) =>
      row.gpuType === want.gpuType &&
      row.tier === want.tier &&
      (want.region === undefined || row.region === want.region),
  );

  const byPrice = (a: Pricing, b: Pricing): number => a.pricePerHour - b.pricePerHour;

  const exact = matching.filter((row) => row.gpuCount === want.gpuCount);
  const exactAvailable = exact.filter((row) => row.available > 0).sort(byPrice);
  if (exactAvailable.length > 0) {
    const offer = exactAvailable[0]!;
    return { offer, estimated: false, pricePerHour: offer.pricePerHour };
  }

  const exactAny = [...exact].sort(byPrice);
  if (exactAny.length > 0) {
    const offer = exactAny[0]!;
    return {
      offer,
      estimated: false,
      pricePerHour: offer.pricePerHour,
      note:
        "No capacity is currently available at this configuration — the price is the catalog " +
        "price and the launch may fail to place. Consider another region or tier.",
    };
  }

  const perGpu = [...matching].sort(
    (a, b) => a.pricePerHour / a.gpuCount - b.pricePerHour / b.gpuCount,
  );
  if (perGpu.length > 0) {
    const reference = perGpu[0]!;
    return {
      // The reference offering, not the thing that will launch — carried so
      // the quote pins its region (see the OfferSelection doc).
      offer: reference,
      estimated: true,
      pricePerHour: cents((reference.pricePerHour / reference.gpuCount) * want.gpuCount),
      note:
        `No offering with exactly ${want.gpuCount} GPU(s) of this type is listed right now. ` +
        "The figure shown is derived from the per-GPU rate of a different configuration and is " +
        "an estimate, not a quoted price.",
    };
  }

  return {
    estimated: true,
    note:
      "No pricing is currently listed for this GPU type and tier. Check gpu_pricing_list for " +
      "what is available — this request cannot be quoted right now.",
  };
}

/** The price line the user reads, plus an estimate for 24 hours. */
function priceLine(
  count: number,
  gpuType: string,
  tier: string,
  region: string | undefined,
  selection: OfferSelection,
): string {
  const where = region === undefined ? "" : ` in ${region}`;
  if (selection.pricePerHour === undefined) {
    return `${count}x ${gpuType} (${tier})${where} — no price could be resolved.`;
  }
  const hourly = usd(selection.pricePerHour);
  const daily = usd(cents(selection.pricePerHour * 24));
  const qualifier = selection.estimated ? " estimated" : "";
  return (
    `${count}x ${gpuType} (${tier})${where} at ${hourly}/hr${qualifier} ` +
    `(~${daily} est for 24h, and it keeps accruing charges until terminated)`
  );
}

// ---------------------------------------------------------------------------
// The generic spend-tool wrapper
// ---------------------------------------------------------------------------

/**
 * One trip through the two-call handshake, ending in a ToolResult.
 *
 * Kept separate from {@link defineSpendTool} so the confirmation flow is one
 * function that can be read and tested on its own.
 *
 * Throws whatever quoteFn or executeFn throws; the caller owns error mapping.
 */
async function confirmThenExecute<P extends Record<string, unknown>>(
  toolName: string,
  params: P,
  quoteFn: (p: P) => Promise<Quote>,
  executeFn: (p: P) => Promise<Executed>,
  question: string,
  ctx: SpendToolContext,
): Promise<ToolResult> {
  const options: HandshakeOptions = ctx.ttlMs === undefined ? {} : { ttlMs: ctx.ttlMs };

  // Call 1 through the guardrail. If this call already carries a valid
  // token, this IS the execution and no prompt is shown — the token is
  // the approval, and re-asking would let a client veto a confirmation
  // the handshake already validated.
  const first = await withSpendGuardrail(toolName, params, quoteFn, executeFn, options);
  if (!isQuoteResult(first)) {
    return ok(first);
  }

  // A quote exists, so there is a price to show. Ask directly where the
  // client can ask; otherwise hand the quote and its token back and let
  // the agent relay it. Both paths end at the same guardrail.
  const answer = await elicitConfirmation(ctx.elicitation, first, question);
  if (answer === "unavailable") {
    return quoteResult(first);
  }
  if (answer === "declined") {
    // The token is simply never handed out. A token the agent does not
    // have is a token that cannot be replayed, which is what makes a
    // decline mean declined rather than declined-for-now.
    return quoteResult(declinedResult(first));
  }

  // Confirmed: spend the token we just minted, through the same wrapper.
  const confirmed = await withSpendGuardrail(
    toolName,
    { ...params, [CONFIRMATION_TOKEN_PARAM]: first.confirmationToken },
    quoteFn,
    executeFn,
    options,
  );
  return isQuoteResult(confirmed) ? quoteResult(confirmed) : ok(confirmed);
}

/**
 * Wire one spend tool.
 *
 * The guardrail lives HERE, in the wrapper, and not in the four call sites.
 * That is the point: a provisioning spend tool cannot be written without the
 * handshake, because there is no path through this factory that reaches an
 * executeFn except the one confirmThenExecute opens.
 */
function defineSpendTool<A extends Record<string, unknown>>(spec: {
  name: string;
  title: string;
  description: string;
  inputSchema: ZodRawShape;
  annotations: SpendAnnotations;
  /** The question shown above the confirm box on the elicitation path. */
  question: string;
  /** Phrase naming the attempt, for error messages. */
  context: (args: A) => string;
  quote: (args: A, config: Configuration, env: Env) => Promise<Quote>;
  execute: (args: A, config: Configuration, env: Env) => Promise<Executed>;
  /**
   * Replace the mapped error entirely, when the generic mapping would be
   * actively misleading for this tool. Returning undefined falls through to the
   * shared funnel, which is what every tool but one does.
   *
   * The message returned here must be composed from STATIC text — it bypasses
   * the funnel, so it must not be built from a response body. Mirrors
   * `defineWriteTool`'s hook of the same name.
   */
  overrideError?: (err: unknown, args: A) => Error | undefined;
}): SpendToolDefinition {
  const schema = z.object(spec.inputSchema);

  return {
    name: spec.name,
    title: spec.title,
    description: spec.description,
    inputSchema: spec.inputSchema,
    annotations: spec.annotations,
    handler: async (args = {}, ctx = {}) => {
      const env = ctx.env ?? process.env;

      let parsed: A;
      let config: Configuration;
      try {
        parsed = schema.parse(args) as A;
        config = buildConfiguration(env);
      } catch (err) {
        const detail =
          err instanceof z.ZodError
            ? err.issues.map((i) => `${i.path.join(".") || "(root)"}: ${i.message}`).join("; ")
            : err instanceof Error
              ? err.message
              : undefined;
        return fail(mapSdkError(err, `preparing ${spec.name}`, { env, details: detail }));
      }

      const quoteFn = (p: A): Promise<Quote> => spec.quote(p, config, env);
      const executeFn = (p: A): Promise<Executed> => spec.execute(p, config, env);

      try {
        return await confirmThenExecute(
          spec.name,
          parsed,
          quoteFn,
          executeFn,
          spec.question,
          ctx,
        );
      } catch (err) {
        const override = spec.overrideError?.(err, parsed);
        if (override !== undefined) {
          return fail(override);
        }
        return fail(await mapSdkErrorAsync(err, spec.context(parsed), env));
      }
    },
  };
}

// ---------------------------------------------------------------------------
// Shared schema fragments
// ---------------------------------------------------------------------------

/**
 * The confirmation token.
 *
 * Optional in the schema and mandatory in practice: the first call must be
 * able to arrive without one (that is how a price gets quoted at all), and the
 * handshake — not the schema — is what refuses to execute without a valid one.
 * Making it required would only mean an agent inventing a value.
 */
const CONFIRMATION_TOKEN = {
  [CONFIRMATION_TOKEN_PARAM]: z
    .string()
    .optional()
    .describe(
      "The single-use token returned by this tool's own quote. Omit on the first call. " +
        "Never invent one: an unrecognised token simply produces a fresh quote.",
    ),
};

const GPU_COUNT = z
  .number()
  .int()
  .min(1)
  .max(8)
  .describe(
    "Number of GPUs in the instance (1-8). Required and never inferred — ask the user if " +
      "they have not said.",
  );

const TIER = z
  .enum(["spot", "on_demand"])
  .describe(
    "on_demand for most launches; spot is soft-deprecated and suppliers may not offer it, in " +
      "which case a spot request returns no offerings. Required — never chosen for the user.",
  );

const COMMON_INSTANCE_FIELDS = {
  region: z
    .string()
    .optional()
    .describe("Region code. Omit to take the cheapest available region, which the quote names."),
  ssh_key_ids: z
    .array(z.string())
    .optional()
    .describe(
      "SSH key ids to install, from gpu_ssh_keys_list. Without one the user cannot log in to " +
        "what they are paying for.",
    ),
  name: z.string().optional().describe("Human-readable instance name."),
  environment: z
    .string()
    .optional()
    .describe(
      "Launch environment, e.g. certified, certified:pytorch, raw_vm, raw_vm:ubuntu-24.04. " +
        "Omit for the supplier default.",
    ),
  disk_gb: z
    .number()
    .int()
    .min(10)
    .max(1000)
    .optional()
    .describe(
      "Filesystem size in GB. Omit for the 100 GB default; disk beyond the allowance is priced " +
        "into the hourly rate.",
    ),
  max_price_per_hour: z
    .number()
    .positive()
    .optional()
    .describe(
      "Refuse to place above this whole-instance hourly price. Omit and the quoted price — " +
        "estimated ones included — is used as the ceiling automatically, except when disk_gb " +
        "is customized (extra disk folds into the hourly rate); set this yourself there.",
    ),
};

// ---------------------------------------------------------------------------
// gpu_instances_create
// ---------------------------------------------------------------------------

const createSchema = {
  gpu_type: z
    .string()
    .min(1)
    .describe(
      "Exact GPU type id from gpu_types_list, e.g. h100_sxm. Required and never inferred.",
    ),
  gpu_count: GPU_COUNT,
  tier: TIER,
  ...COMMON_INSTANCE_FIELDS,
  ...CONFIRMATION_TOKEN,
};

type CreateArgs = z.infer<z.ZodObject<typeof createSchema>>;

/** What the quote resolves and therefore what actually executes. */
type CreateResolved = Resolved<
  CreateArgs & {
    quoted_price_per_hour?: number;
    quoted_price_is_estimate?: boolean;
    quoted_availability_note?: string;
  }
>;

/**
 * Price a launch, and resolve everything the launch will use.
 *
 * Two resolutions matter beyond the price itself:
 *
 *   - REGION. Omitted means "cheapest", and cheapest is a fact about right now.
 *     Pinning the region the quote priced is what stops a confirmed launch from
 *     landing somewhere more expensive than the number the user said yes to.
 *     Estimated quotes pin the REFERENCE offering's region for the same reason:
 *     the derived rate is only a fact about where it was observed.
 *   - max_price_per_hour. When the user did not set one, the quoted price
 *     becomes the ceiling, so the API itself refuses to place above what was
 *     approved. Deliberately exact, with no headroom: a launch that fails
 *     because the price moved is recoverable, a launch that silently costs more
 *     than the user agreed to is not. This includes ESTIMATED prices — the
 *     estimate is the number the user approved, so it is the number placement
 *     is bounded by, and the summary says so. The ceiling is skipped only when
 *     disk_gb is set, since extra disk folds into the hourly rate and would
 *     trip the ceiling for a reason that has nothing to do with price drift.
 *
 * And one refusal: when nothing could be priced at all, this THROWS rather
 * than returning a quote. A summary reading "no price could be resolved" is
 * not a quote, and returning it would let the guardrail mint a confirmable
 * token for a launch with no price and no ceiling ("no quote -> no token").
 */
/**
 * The provision engine hard-requires at least one attachable SSH key for
 * every launch, and without one the
 * request is accepted, confirmed, and only then fails (`validation_failed:
 * ssh keys not found`). That failure is knowable at quote time, so the quote
 * refuses instead: the user is never asked to approve a launch that cannot
 * start. Two shapes:
 *
 *   - explicit ssh_key_ids: every named id must exist on the account, or the
 *     confirmed launch would fail on exactly that validation;
 *   - no ssh_key_ids: the account must have at least one key for the engine
 *     to auto-attach. The empty-account case, which is every fresh
 *     account's first launch, is caught here.
 */
async function assertSshKeysAttachable(
  args: { ssh_key_ids?: string[] },
  config: Configuration,
): Promise<void> {
  const api = new SshKeysApi(config);
  const ids = new Set<string>();
  let truncated = false;
  let cursor: string | undefined;
  try {
    // Bounded pagination: limit 200 x 5 pages = a 1000-key scan bound, never
    // an unbounded loop against a hostile cursor. A scan that exhausts the
    // bound with a cursor still set is recorded as truncated, and truncation
    // makes the missing-ids branch inconclusive rather than a false refusal.
    for (let page = 0; page < 5; page++) {
      const res = await api.listSshKeys(
        cursor === undefined ? { limit: 200 } : { limit: 200, cursor },
      );
      for (const key of res.data ?? []) {
        ids.add(key.id);
      }
      if (res.nextCursor === null || res.nextCursor === undefined) {
        break;
      }
      cursor = res.nextCursor;
      if (page === 4) {
        truncated = true;
      }
    }
  } catch {
    // INCONCLUSIVE — skip the check, never refuse. This pre-flight is
    // advisory: the engine enforces the real requirement at execution. A
    // fetch failure here includes the deterministic case of a granular key
    // scoped to instances:write but not ssh_keys:read — a key shape that
    // could launch fine on 0.1.0 and must keep launching. Refusing on a
    // failed read would convert "we could not check" into "we checked and it
    // was bad", which is exactly the overclaimed-safety failure mode this
    // package refuses elsewhere (no quote -> no token, never the reverse).
    return;
  }

  if (args.ssh_key_ids !== undefined && args.ssh_key_ids.length > 0) {
    if (truncated) {
      return;
    }
    const missing = args.ssh_key_ids.filter((id) => !ids.has(id));
    if (missing.length > 0) {
      throw new QuoteUnavailableError(
        `These ssh_key_ids do not exist on this account: ${missing.join(", ")}. ` +
          "A confirmed launch would fail on exactly that check. Pick ids from " +
          "gpu_ssh_keys_list, or omit ssh_key_ids to use the account's default keys.",
      );
    }
    return;
  }

  if (ids.size === 0 && !truncated) {
    throw new QuoteUnavailableError(
      "This account has no SSH keys, and every instance launch requires at least one — " +
        "a confirmed launch would be accepted and then fail without provisioning. " +
        "Register a key first (gpu_ssh_keys_create, or `gpu keys create`), then quote again.",
    );
  }
}

async function quoteCreate(
  args: CreateResolved,
  config: Configuration,
  label: string,
  extraLines: string[] = [],
  extraParams: Record<string, unknown> = {},
): Promise<Quote> {
  await assertSshKeysAttachable(args, config);
  const rows = await collectPricing(config, {
    gpuType: args.gpu_type,
    region: args.region,
    tier: args.tier,
  });
  const selection = selectOffer(rows, {
    gpuType: args.gpu_type,
    gpuCount: args.gpu_count,
    tier: args.tier,
    region: args.region,
  });

  const pricePerHour = selection.pricePerHour;
  if (pricePerHour === undefined) {
    throw new QuoteUnavailableError(
      selection.note ??
        `No pricing is currently listed for ${args.gpu_count}x ${args.gpu_type} (${args.tier}).`,
    );
  }

  const region = args.region ?? selection.offer?.region;
  const resolved: Record<string, unknown> = {
    ...args,
    ...extraParams,
    region,
    quoted_price_per_hour: pricePerHour,
    quoted_price_is_estimate: selection.estimated,
    quoted_availability_note: selection.note,
  };
  delete resolved[CONFIRMATION_TOKEN_PARAM];

  const autoCeiling = args.max_price_per_hour === undefined && args.disk_gb === undefined;
  if (autoCeiling) {
    resolved.max_price_per_hour = pricePerHour;
  }

  const lines = [
    `${label}${priceLine(args.gpu_count, args.gpu_type, args.tier, region, selection)}`,
    ...extraLines,
  ];
  if (selection.note !== undefined) {
    lines.push(selection.note);
  }
  if (autoCeiling && selection.estimated) {
    lines.push(
      "The estimated price is applied as this launch's ceiling (max_price_per_hour): a " +
        "placement above it fails rather than costing more than the number shown.",
    );
  }

  return { summary: lines.join(" "), resolvedParams: resolved };
}

async function executeCreate(
  args: CreateResolved,
  config: Configuration,
  what: string,
): Promise<Executed> {
  const operation = await new InstancesApi(config).createInstance({
    createInstanceRequest: {
      gpuType: args.gpu_type,
      gpuCount: args.gpu_count,
      tier: args.tier,
      region: args.region,
      sshKeyIds: args.ssh_key_ids,
      name: args.name,
      environment: args.environment,
      diskGb: args.disk_gb,
      maxPricePerHour: args.max_price_per_hour,
    },
  });

  return {
    lead:
      `operation_id: ${operation.operationId} — ${what} started. Report this id to the user now: ` +
      "it is the only handle on a request that may already be spending money.",
    payload: {
      operation_id: operation.operationId,
      operation_state: operation.state,
      instance_id: operation.resourceId ?? null,
      // Create-time advisories, present only on this 202 and never on a poll.
      // Dropping them here would lose them permanently.
      warnings: operation.warnings ?? [],
      requested: {
        gpu_type: args.gpu_type,
        gpu_count: args.gpu_count,
        tier: args.tier,
        region: args.region ?? null,
      },
      quoted_price_per_hour: args.quoted_price_per_hour ?? null,
      charges_note:
        "The supplier charges from when the instance runs until it is terminated with " +
        "gpu_instances_terminate. Nothing stops on its own unless auto-terminate was set.",
      next_steps: POLL_GUIDANCE,
      on_success:
        "Report the instance id, its status, the hourly cost, and how to terminate it " +
        "(gpu_instances_terminate).",
    },
  };
}

// ---------------------------------------------------------------------------
// gpu_instances_terminate
// ---------------------------------------------------------------------------

const terminateSchema = {
  instance_id: z
    .string()
    .min(1)
    .describe("Instance id to terminate, e.g. gpu-abc12345. Required."),
  ...CONFIRMATION_TOKEN,
};

type TerminateArgs = z.infer<z.ZodObject<typeof terminateSchema>>;
type TerminateResolved = Resolved<
  TerminateArgs & {
    instance_name?: string | null;
    gpu_type?: string;
    gpu_count?: number;
    price_per_hour?: number;
  }
>;

// ---------------------------------------------------------------------------
// The tier
// ---------------------------------------------------------------------------

export const spendTools: readonly SpendToolDefinition[] = [
  defineSpendTool<CreateResolved>({
    name: "gpu_instances_create",
    title: "Create a GPU instance",
    description:
      "Provisions a GPU instance from one of the server's configured suppliers. " +
      MONEY_GUARDRAIL +
      " " +
      HANDSHAKE_NOTE +
      " Creation is asynchronous: the confirmed call returns an operation id immediately and " +
      "never waits. Poll with gpu_operations_get rather than creating again — a duplicate " +
      "create means two instances and double the charges.",
    inputSchema: createSchema,
    annotations: {
      readOnlyHint: false,
      destructiveHint: false,
      idempotentHint: false,
      openWorldHint: true,
    },
    question: "Create this instance now?",
    context: () => "creating an instance",
    quote: (args, config) => quoteCreate(args, config, "Create "),
    execute: (args, config) => executeCreate(args, config, "instance creation"),
  }),

  defineSpendTool<TerminateResolved>({
    name: "gpu_instances_terminate",
    title: "Terminate a GPU instance",
    description:
      "Terminates a GPU instance and STOPS ITS CHARGES. Destructive and irreversible: any data " +
      "on the instance that has not been copied off is gone. Confirm with the user before " +
      "terminating, and never terminate an instance the user did not ask you to touch. " +
      HANDSHAKE_NOTE +
      " The first call describes exactly what will be destroyed and what it currently costs. " +
      "After terminating, verify: terminate, then verify — a delete you did not confirm is an " +
      "instance that may still be accruing charges. Call gpu_instances_list afterwards and tell the user " +
      "what is still running.",
    inputSchema: terminateSchema,
    annotations: {
      readOnlyHint: false,
      destructiveHint: true,
      idempotentHint: true,
      openWorldHint: true,
    },
    question: "Terminate this instance now?",
    context: (args) => `terminating instance ${args.instance_id}`,
    quote: async (args, config) => {
      // The lighter destructive contract: the two-call shape is the
      // same, but what is shown is what will be DESTROYED rather than a price.
      // The hourly rate is included anyway, because "you are about to stop
      // paying $4.98/hr" is the fact that makes the decision obvious.
      const instance = await new InstancesApi(config).getInstance({ id: args.instance_id });

      const label = instance.name === undefined ? "" : ` ("${instance.name}")`;
      const summary =
        `Terminate ${instance.id}${label} — ${instance.gpuCount}x ${instance.gpuType} ` +
        `(${instance.tier}) in ${instance.region}, currently ${instance.status}, costing ` +
        `${usd(instance.pricePerHour)}/hr. This is irreversible: data on the instance that has ` +
        "not been copied off is lost.";

      return {
        summary,
        resolvedParams: {
          instance_id: instance.id,
          instance_name: instance.name ?? null,
          gpu_type: instance.gpuType,
          gpu_count: instance.gpuCount,
          price_per_hour: instance.pricePerHour,
        },
      };
    },
    execute: async (args, config) => {
      const operation = await new InstancesApi(config).deleteInstance({ id: args.instance_id });

      return {
        lead:
          `operation_id: ${operation.operationId} — termination started for ${args.instance_id}. ` +
          "It is not stopped until you have verified it.",
        payload: {
          operation_id: operation.operationId,
          operation_state: operation.state,
          instance_id: args.instance_id,
          next_steps:
            "Terminate, then verify: call gpu_instances_list and confirm this instance is gone " +
            "or terminating. " +
            POLL_GUIDANCE,
          spend_accounting:
            "Finish by telling the user what is still running and therefore still accruing " +
            "charges.",
        },
      };
    },
  }),
];


/** The tier's names, in registration order. */
export const SPEND_TOOL_NAMES: readonly string[] = spendTools.map((t) => t.name);

/** Look a tool up by name. Returns undefined rather than throwing. */
export function findSpendTool(name: string): SpendToolDefinition | undefined {
  return spendTools.find((t) => t.name === name);
}