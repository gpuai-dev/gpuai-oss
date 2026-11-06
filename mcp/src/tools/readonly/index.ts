// The read-only tool tier: free, non-mutating tools.
//
// The tier split is read-vs-mutate. Everything here is a GET that changes
// nothing, so nothing here asks for confirmation; every mutation lives in the
// write tier (../write) or the guarded tier (../spend).
//
// Two implementation rules, both structural rather than advisory:
//
//   1. Every call goes through a typed `@gpuai/sdk` Api method. No raw HTTP is
//      issued from this file, no direct fetch call, no hand-built request. If
//      the API changes shape, the build breaks here rather than the tool
//      silently returning something wrong at runtime.
//   2. Every failure goes through mapSdkError / mapSdkErrorAsync. A raw SDK
//      error reaching an agent is both unhelpful and a credential-leak path.
import {
  Configuration,
  EnvironmentsApi,
  GpuTypesApi,
  InstancesApi,
  MetaApi,
  OperationsApi,
  PricingApi,
  SshKeysApi,
} from "@gpuai/sdk";
import { z, type ZodRawShape } from "zod";

import { buildConfiguration, resolveBaseUrl, type Env } from "../../auth.js";
import { mapSdkError, mapSdkErrorAsync, sanitizeCredentialFields } from "../../errors.js";

export interface ToolResult {
  content: Array<{ type: "text"; text: string }>;
  isError?: boolean;
}

/** Per-call context. `env` exists so tests need not mutate process.env. */
export interface ToolContext {
  env?: Env;
}

/**
 * Annotations every free tool carries.
 *
 * `readOnlyHint` is the one clients actually act on — it is what stops a host
 * from prompting for confirmation on a catalog read, and what makes the
 * spend/free split visible in the tool list rather than only in prose.
 */
export interface ReadonlyAnnotations {
  readOnlyHint: true;
  destructiveHint: false;
  idempotentHint: true;
  openWorldHint: true;
}

/** The shape plan 05's registry consumes. */
export interface ReadonlyToolDefinition {
  name: string;
  title: string;
  description: string;
  /** Zod raw shape — the registry wraps it in z.object() when registering. */
  inputSchema: ZodRawShape;
  annotations: ReadonlyAnnotations;
  handler: (args?: Record<string, unknown>, ctx?: ToolContext) => Promise<ToolResult>;
}

const ANNOTATIONS: ReadonlyAnnotations = {
  readOnlyHint: true,
  destructiveHint: false,
  idempotentHint: true,
  openWorldHint: true,
};

/** Pagination params shared by the cursor-paged list endpoints. */
const PAGINATION = {
  cursor: z.string().optional().describe("Opaque cursor from a previous page's next_cursor."),
  limit: z.number().int().min(1).max(100).optional().describe("Page size (1-100)."),
};

/**
 * Serialise a successful result.
 *
 * Everything goes through sanitizeCredentialFields on the way out — one choke
 * point for the whole tier, for the same reason the error funnel is one
 * function: a tool cannot forget to use it, because the tools do not serialise
 * themselves. Concretely this is what keeps an instance's web console login
 * (`app_password`) out of the transcript while leaving `app_url` intact. See
 * errors.ts for the full argument.
 */
function ok(payload: unknown): ToolResult {
  return {
    content: [{ type: "text", text: JSON.stringify(sanitizeCredentialFields(payload), null, 2) }],
  };
}

function fail(err: Error): ToolResult {
  return { content: [{ type: "text", text: err.message }], isError: true };
}

/**
 * Wire one tool.
 *
 * The generic body is the whole reason every handler behaves identically: parse
 * args, build the SDK config, call a typed method, serialise. A tool cannot
 * accidentally skip the error funnel, because the funnel is here and not in the
 * call sites.
 */
function defineTool<S extends ZodRawShape>(spec: {
  name: string;
  title: string;
  description: string;
  inputSchema: S;
  /** Phrase naming the attempt, for error messages ("listing instances"). */
  context: (args: z.infer<z.ZodObject<S>>) => string;
  call: (args: z.infer<z.ZodObject<S>>, config: Configuration, env: Env) => Promise<unknown>;
}): ReadonlyToolDefinition {
  const schema = z.object(spec.inputSchema);

  return {
    name: spec.name,
    title: spec.title,
    description: spec.description,
    inputSchema: spec.inputSchema,
    annotations: ANNOTATIONS,
    handler: async (args = {}, ctx = {}) => {
      const env = ctx.env ?? process.env;

      // Argument and configuration problems are caller errors, not API errors;
      // they must never reach the network. mapSdkError still formats them, so a
      // bad argument reads like every other failure and is still redacted.
      let parsed: z.infer<z.ZodObject<S>>;
      let config: Configuration;
      try {
        parsed = schema.parse(args) as z.infer<z.ZodObject<S>>;
        config = buildConfiguration(env);
      } catch (err) {
        const detail =
          err instanceof z.ZodError
            ? err.issues.map((i) => `${i.path.join(".") || "(root)"}: ${i.message}`).join("; ")
            : err instanceof Error
              ? err.message
              : undefined;
        return fail(
          mapSdkError(err, `preparing ${spec.name}`, { env, details: detail }),
        );
      }

      try {
        return ok(await spec.call(parsed, config, env));
      } catch (err) {
        return fail(await mapSdkErrorAsync(err, spec.context(parsed), env));
      }
    },
  };
}

// ---------------------------------------------------------------------------
// gpu_auth_status
// ---------------------------------------------------------------------------

/**
 * There is no whoami endpoint, so authentication is proven by an authenticated
 * call succeeding, and the tool says so rather than inventing an identity
 * field. The instance listing is that call.
 *
 * The API key itself is never part of the result. Not the value, not a prefix,
 * not a last-4: a partial credential is still credential material.
 */
async function authStatus(config: Configuration, env: Env): Promise<unknown> {
  const page = await new InstancesApi(config).listInstances({ limit: 5 });
  return {
    authenticated: true,
    api_base: resolveBaseUrl(env),
    credential: {
      source: "GPUAI_API_KEY environment variable",
      note: "This server never returns, logs, or echoes the API key — not in results, not in errors.",
    },
    instances: {
      active_on_first_page: page.data.length,
      more_pages: page.nextCursor !== null && page.nextCursor !== undefined,
      note: "First page only — call gpu_instances_list for the full inventory.",
    },
    next_steps: [
      "Check pricing with gpu_pricing_list before proposing any provisioning.",
      "Provisioning tools quote a price first and require an explicit confirmation before they execute.",
    ],
  };
}

// ---------------------------------------------------------------------------
// The tier
// ---------------------------------------------------------------------------

export const readonlyTools: readonly ReadonlyToolDefinition[] = [
  defineTool({
    name: "gpu_auth_status",
    title: "Check authentication",
    description:
      "Call this FIRST, before any provisioning work. Verifies the configured API key is " +
      "valid against the configured gpuctl server and reports the server URL and a count of " +
      "active instances. " +
      "Free, read-only, and never returns the API key itself. If this fails with an auth error, " +
      "stop and tell the user to fix GPUAI_API_KEY rather than retrying other tools.",
    inputSchema: {},
    context: () => "checking authentication status",
    call: (_args, config, env) => authStatus(config, env),
  }),

  defineTool({
    name: "gpu_health_get",
    title: "Check API health",
    description:
      "Liveness probe for the gpuctl API server. Use it to tell a transport or platform outage apart from " +
      "a problem with the request or the API key: if this fails the service is unreachable, and if " +
      "it succeeds while another tool returns an auth error the credential is the problem. Requires " +
      "no authentication. Free and read-only.",
    inputSchema: {},
    context: () => "checking API health",
    call: (_args, config) => new MetaApi(config).getHealth(),
  }),

  defineTool({
    name: "gpu_types_list",
    title: "List GPU types",
    description:
      "Lists the GPU types available from the server's configured suppliers, with their memory and specifications. Use this to " +
      "resolve a vague request ('something with lots of VRAM') into an exact GPU type before " +
      "quoting a price. Free and read-only. Never guess a GPU type — if the user has not said " +
      "which one they want, show them this list and ask.",
    inputSchema: { ...PAGINATION },
    context: () => "listing GPU types",
    call: (args, config) => new GpuTypesApi(config).listGpuTypes(args),
  }),

  defineTool({
    name: "gpu_pricing_list",
    title: "List GPU pricing and availability",
    description:
      "Lists hourly prices and current availability per GPU type, region, and tier " +
      "(on_demand). Call this before proposing any provisioning: the user must see the price " +
      "before confirming anything that spends money. Prices are per instance per hour and cover " +
      "all GPUs in the instance. Offers with no availability are omitted unless " +
      "include_unavailable is true. Free and read-only.",
    inputSchema: {
      gpu_type: z.string().optional().describe("Exact-match filter on GPU type."),
      region: z.string().optional().describe("Exact-match filter on canonical region code."),
      tier: z
        .enum(["spot", "on_demand"])
        .optional()
        .describe(
          "Pricing tier. spot is soft-deprecated and suppliers may not offer it (then it " +
            "returns no rows); use on_demand.",
        ),
      include_unavailable: z
        .boolean()
        .optional()
        .describe("Include offers with zero current availability."),
      ...PAGINATION,
    },
    context: () => "listing pricing",
    call: (args, config) =>
      new PricingApi(config).listPricing({
        gpuType: args.gpu_type,
        region: args.region,
        tier: args.tier,
        includeUnavailable: args.include_unavailable,
        cursor: args.cursor,
        limit: args.limit,
      }),
  }),

  defineTool({
    name: "gpu_environments_list",
    title: "List launch environments",
    description:
      "Returns the launch environment catalog: the certified frameworks, the selectable version of " +
      "each, what software each one includes, and the raw VM image options. These are exactly the " +
      "values the environment field of a provisioning call accepts, so call this rather than " +
      "guessing an environment string — a rejected environment wastes a provisioning attempt. " +
      "Free and read-only.",
    inputSchema: {},
    context: () => "listing launch environments",
    call: (_args, config) => new EnvironmentsApi(config).getEnvironments(),
  }),

  defineTool({
    name: "gpu_instances_list",
    title: "List GPU instances",
    description:
      "Lists this organization's GPU instances. Terminated instances are excluded by default; " +
      "pass status=terminated for history or status=all for everything. Use this to check what is " +
      "currently running and therefore currently accruing charges — especially after a failed provision, " +
      "and before telling the user their work is cleaned up. Free and read-only.",
    inputSchema: {
      status: z
        .enum([
          "allocating",
          "starting",
          "running",
          "stopping",
          "stopped",
          "terminated",
          "error",
          "all",
        ])
        .optional()
        .describe("Filter by lifecycle status. Default: all non-terminated."),
      ...PAGINATION,
    },
    context: () => "listing instances",
    call: (args, config) =>
      new InstancesApi(config).listInstances({
        status: args.status,
        cursor: args.cursor,
        limit: args.limit,
      }),
  }),

  defineTool({
    name: "gpu_instances_get",
    title: "Get a GPU instance",
    description:
      "Fetches one GPU instance by id: lifecycle status, GPU type and count, region, hourly price, " +
      "and SSH connection details once it is running. Status 'allocating' means capacity is still " +
      "being acquired and can take around fifteen minutes for slow-boot hardware; 'starting' means " +
      "the machine exists and is booting; 'running' means it is ready to use. Free and read-only.",
    inputSchema: {
      id: z.string().min(1).describe("Instance id, e.g. gpu-abc12345."),
    },
    context: (args) => `getting instance ${args.id}`,
    call: (args, config) => new InstancesApi(config).getInstance({ id: args.id }),
  }),

  defineTool({
    name: "gpu_operations_get",
    title: "Get an async operation's status",
    description:
      "Polls a long-running operation by id — the id returned when an instance is created " +
      "or terminated. Poll on a backoff: wait 2s after the first call and increase the " +
      "gap by 1s each time up to a maximum of 10s, matching what the gpu CLI does. Do NOT poll in " +
      "a tight loop. Terminal states are succeeded, failed, and cancelled; anything else means " +
      "still in progress, so keep polling. On 'failed', immediately call gpu_instances_list and " +
      "terminate anything that was left behind — a failed provision can still leave a " +
      "running instance that accrues charges. Free and read-only.",
    inputSchema: {
      id: z.string().min(1).describe("Operation id (a UUID) from the Operation-Id response."),
    },
    context: (args) => `getting operation ${args.id}`,
    call: (args, config) => new OperationsApi(config).getOperation({ id: args.id }),
  }),

  defineTool({
    name: "gpu_ssh_keys_list",
    title: "List SSH keys",
    description:
      "Lists the SSH keys registered for the user this API key belongs to, with their names and " +
      "fingerprints. Use it to pick a key id to attach when provisioning, so the user can actually " +
      "log in to what they paid for. Never returns private key material. Free and read-only.",
    inputSchema: { ...PAGINATION },
    context: () => "listing SSH keys",
    call: (args, config) => new SshKeysApi(config).listSshKeys(args),
  }),

  defineTool({
    name: "gpu_ssh_keys_get",
    title: "Get one SSH key",
    description:
      "Fetches one registered SSH key by id: its name, fingerprint, and public key. Use it to " +
      "confirm the key about to be attached to a provisioning request is the one the user actually " +
      "holds the private half of — attaching the wrong key produces a machine they have paid for " +
      "and cannot log in to. Private key material is never stored and never returned. Free and " +
      "read-only.",
    inputSchema: {
      id: z.string().min(1).describe("SSH key id, as listed by gpu_ssh_keys_list."),
    },
    context: (args) => `getting SSH key ${args.id}`,
    call: (args, config) => new SshKeysApi(config).getSshKey({ id: args.id }),
  }),
];

/** The tier's names, in registration order. */
export const READONLY_TOOL_NAMES: readonly string[] = readonlyTools.map((t) => t.name);

/** Look a tool up by name. Returns undefined rather than throwing. */
export function findReadonlyTool(name: string): ReadonlyToolDefinition | undefined {
  return readonlyTools.find((t) => t.name === name);
}
