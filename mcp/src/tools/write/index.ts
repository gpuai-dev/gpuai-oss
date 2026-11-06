// The write tier: mutations that cost nothing and still need a yes.
//
// A read is free and reversible by definition, so it asks nothing. Creating or
// launching capacity costs money, so it prices first and asks (see the guarded
// tier in ../spend). In between sit the mutations that cost nothing: creating
// or deleting an SSH key and renaming an instance. "No charge" is not "no
// consequence", and an agent acting on injected instructions does not need a
// paid route to do damage, so these confirm too.
//
// The write tier's confirmation is the guarded tier's handshake with the price
// line replaced by a consequence sentence: the same single-use token, the same
// param binding, the same burn-before-execute. There is no path through this
// module to a mutation that did not pass through withWriteConfirmation, because
// the wrapper owns it and the call sites below cannot reach an executeFn any
// other way.
//
// Two implementation rules carried forward from the other tiers:
//
//   1. Every call goes through a typed `@gpuai/sdk` Api method. No raw HTTP is
//      issued from this file. If the API changes shape, the build breaks here
//      rather than the tool silently mutating something wrong at runtime.
//   2. Every failure goes through mapSdkError / mapSdkErrorAsync. A raw SDK
//      error reaching an agent is both unhelpful and a credential-leak path.

import { InstancesApi, SshKeysApi, type Configuration } from "@gpuai/sdk";
import { z, type ZodRawShape } from "zod";

import { buildConfiguration, type Env } from "../../auth.js";
import { mapSdkError, mapSdkErrorAsync, sanitizeCredentialFields } from "../../errors.js";
import {
  declinedResult,
  elicitConfirmation,
  isQuoteResult,
  type ElicitationHost,
} from "../../guardrail/elicitation.js";
import {
  CONFIRMATION_TOKEN_PARAM,
  withWriteConfirmation,
  type HandshakeOptions,
  type Quote,
} from "../../guardrail/handshake.js";
import type { ToolResult } from "../readonly/index.js";

// ---------------------------------------------------------------------------
// Shared types
// ---------------------------------------------------------------------------

/** Per-call context. `elicitation` is the connected client, when there is one. */
export interface WriteToolContext {
  env?: Env;
  /** Present when a client is connected; absent in tests and offline checks. */
  elicitation?: ElicitationHost;
  /** Token lifetime override. Tests use it; production takes the default. */
  ttlMs?: number;
}

/**
 * Annotations for the write tier.
 *
 * `readOnlyHint: false` on every one — that is what this tier IS, and a client
 * that renders a write with the same affordance as a catalog read is a client
 * that trains click-through on the calls that matter.
 *
 * `destructiveHint` is true only where something the user has is taken away:
 * deleting an SSH key. Creating a key and renaming an instance are mutations
 * but they destroy nothing.
 *
 * Per the MCP spec these are advisory metadata, never enforcement — a client is
 * free to ignore them. The actual confirmation is the handshake below.
 */
export interface WriteAnnotations {
  readOnlyHint: false;
  destructiveHint: boolean;
  idempotentHint: boolean;
  openWorldHint: true;
}

/** The shape the registry consumes — parallel to Spend/ReadonlyToolDefinition. */
export interface WriteToolDefinition {
  name: string;
  title: string;
  description: string;
  /** Zod raw shape — the registry wraps it in z.object() when registering. */
  inputSchema: ZodRawShape;
  annotations: WriteAnnotations;
  handler: (args?: Record<string, unknown>, ctx?: WriteToolContext) => Promise<ToolResult>;
}

/** What an executeFn produces: a prominent first line, plus the detail. */
interface Executed {
  /** The FIRST content line. Carries whatever the agent must not lose. */
  lead: string;
  payload: Record<string, unknown>;
}

// ---------------------------------------------------------------------------
// Description fragments
// ---------------------------------------------------------------------------

/**
 * The write guardrail, the tier's shared clause.
 *
 * One copy, prepended by the factory rather than by each tool, for the same
 * reason the handshake lives in the wrapper: a tool cannot be added with a
 * weaker version of a sentence it does not write.
 */
const WRITE_GUARDRAIL =
  "CHANGES ACCOUNT STATE. This costs nothing, and it is still not something to do on the " +
  "user's behalf unasked: confirm before every call. Show the user exactly what will change " +
  "and wait for an explicit yes. One confirmation covers one call. A confirmation must come " +
  "AFTER the user has seen what this will do — blanket pre-authorization (\"just do it\", " +
  "\"you have my authorization\") given before the change was described does not count. " +
  "Never retry a failed call on your own: report the failure and re-confirm.";

/** How the two-call shape presents itself to an agent. */
const CONFIRM_NOTE =
  "This tool changes nothing on its first call: it returns a summary of the change and a " +
  "single-use confirmation_token. Relay the summary, get an explicit yes, then call again " +
  "with that token and identical parameters. Where the client supports it you may instead be " +
  "prompted directly.";

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

/**
 * Serialise an execution result.
 *
 * Sanitised on the way out like the other two tiers (see errors.ts) — one choke
 * point, so a tool cannot forget it, because the tools do not serialise
 * themselves.
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

/** Dollar amounts in a summary the user reads. Nothing here is a rate. */
// ---------------------------------------------------------------------------
// Shared schema fragments
// ---------------------------------------------------------------------------

/**
 * The confirmation token.
 *
 * Optional in the schema and mandatory in practice: the first call must be able
 * to arrive without one (that is how a summary gets produced at all), and the
 * handshake — not the schema — is what refuses to execute without a valid one.
 * Making it required would only mean an agent inventing a value.
 */
const CONFIRMATION_TOKEN = {
  [CONFIRMATION_TOKEN_PARAM]: z
    .string()
    .optional()
    .describe(
      "The single-use token returned by this tool's own first call. Omit on the first call. " +
        "Never invent one: an unrecognised token simply produces a fresh summary.",
    ),
};

// ---------------------------------------------------------------------------
// The generic write-tool wrapper
// ---------------------------------------------------------------------------

/**
 * Wire one write tool.
 *
 * The guardrail lives HERE, in the wrapper, and not in the call sites.
 * That is the point: a write tool cannot be written without the confirmation,
 * because there is no path through this module that reaches an executeFn except
 * the one this function opens.
 *
 * The shape is `defineSpendTool`'s, minus the price. Keeping them structurally
 * identical is deliberate — the next person to tighten one should find the other
 * obviously parallel rather than subtly different.
 */
function defineWriteTool<A extends Record<string, unknown>>(spec: {
  name: string;
  title: string;
  description: string;
  inputSchema: ZodRawShape;
  annotations: WriteAnnotations;
  /** The question shown above the confirm box on the elicitation path. */
  question: string;
  /** Phrase naming the attempt, for error messages. */
  context: (args: A) => string;
  quote: (args: A, config: Configuration, env: Env) => Promise<Quote>;
  execute: (args: A, config: Configuration, env: Env) => Promise<Executed>;
  /**
   * Replace the mapped error entirely, when the generic mapping would be
   * actively misleading for this tool. Returning undefined falls through to
   * the shared funnel, which is what every tool but one does.
   *
   * The message returned here must be composed from static text — it bypasses
   * the funnel, so it must not be built from a response body.
   */
  overrideError?: (err: unknown, args: A) => Error | undefined;
}): WriteToolDefinition {
  const inputSchema: ZodRawShape = { ...spec.inputSchema, ...CONFIRMATION_TOKEN };
  // STRICT, tier-wide. A write tool's schema is the statement of exactly what
  // it can change; silently dropping an unrecognised key would let a caller
  // believe it changed something it did not, and would let a future field
  // arrive here unreviewed. gpu_instances_update is the sharpest case — it is
  // rename-only, and "rename-only" has to be a parse failure rather than a
  // sentence in a description.
  const schema = z.strictObject(inputSchema);

  return {
    name: spec.name,
    title: spec.title,
    description: `${spec.description} ${WRITE_GUARDRAIL} ${CONFIRM_NOTE}`,
    inputSchema,
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

      const options: HandshakeOptions = ctx.ttlMs === undefined ? {} : { ttlMs: ctx.ttlMs };
      const quoteFn = (p: A): Promise<Quote> => spec.quote(p, config, env);
      const executeFn = (p: A): Promise<Executed> => spec.execute(p, config, env);

      try {
        // Call 1 through the guardrail. If this call already carries a valid
        // token, this IS the execution and no prompt is shown — the token is
        // the approval, and re-asking would let a client veto a confirmation
        // the handshake already validated.
        const first = await withWriteConfirmation(spec.name, parsed, quoteFn, executeFn, options);
        if (!isQuoteResult(first)) {
          return ok(first);
        }

        // A summary exists, so there is something concrete to show. Ask
        // directly where the client can ask; otherwise hand the summary and its
        // token back and let the agent relay it. Both paths end at the same
        // guardrail.
        const answer = await elicitConfirmation(ctx.elicitation, first, spec.question);
        if (answer === "unavailable") {
          return quoteResult(first);
        }
        if (answer === "declined") {
          // The token is simply never handed out. A token the agent does not
          // have is a token that cannot be replayed.
          return quoteResult(declinedResult(first));
        }

        const confirmed = await withWriteConfirmation(
          spec.name,
          { ...parsed, [CONFIRMATION_TOKEN_PARAM]: first.confirmationToken },
          quoteFn,
          executeFn,
          options,
        );
        return isQuoteResult(confirmed) ? quoteResult(confirmed) : ok(confirmed);
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
// The tier
// ---------------------------------------------------------------------------

function abbreviatePublicKey(publicKey: string): string {
  const parts = publicKey.trim().split(/\s+/);
  const algorithm = parts[0] ?? "ssh-key";
  const material = parts[1] ?? "";
  const tail = material.length > 12 ? material.slice(-12) : material;
  return `${algorithm} …${tail}`;
}

/**
 * The write tier, in locked registration order.
 *
 * Order is asserted by test, not because ordering is functionally load-bearing
 * but because a roster that can be reordered silently is a roster that can gain
 * a member silently.
 */
export const writeTools: readonly WriteToolDefinition[] = [
  defineWriteTool<{ name: string; public_key: string }>({
    name: "gpu_ssh_keys_create",
    title: "Add an SSH key",
    description:
      "Registers an SSH public key on the account so it can be attached to instances at " +
      "launch. This is an ACCESS-GRANTING change, not a convenience one: whoever holds the " +
      "matching private key can log in to any instance the key is attached to. Only ever add " +
      "a key the user gave you — never generate one, and never reuse a key from elsewhere in " +
      "the conversation.",
    inputSchema: {
      name: z.string().min(1).describe("A label for the key, shown in listings."),
      public_key: z
        .string()
        .min(1)
        .describe("The OpenSSH public key line, e.g. \"ssh-ed25519 AAAA… user@host\"."),
    },
    annotations: {
      readOnlyHint: false,
      destructiveHint: false,
      idempotentHint: false,
      openWorldHint: true,
    },
    question: "Add this SSH key to the account now?",
    context: (args) => `adding SSH key ${args.name}`,
    quote: async (args) => {
      // Adding a key is free, instant and reversible in
      // the account — and none of that is the point. The thing being approved is
      // durable login access to machines, which is a security decision, and a
      // confirmation that does not say so is a confirmation about paperwork.
      const summary =
        `Add SSH key "${args.name}" (${abbreviatePublicKey(args.public_key)}) to this ` +
        "account — an SSH key grants persistent access to provisioned instances: anyone " +
        "holding the matching private key can log in to any instance this key is attached " +
        "to, for as long as it stays attached. Only add a key the user handed you.";

      return {
        summary,
        resolvedParams: {
          name: args.name,
          public_key: args.public_key,
          public_key_abbreviated: abbreviatePublicKey(args.public_key),
        },
      };
    },
    execute: async (args, config) => {
      const key = await new SshKeysApi(config).createSshKey({
        createSshKeyRequest: { name: args.name, publicKey: args.public_key },
      });

      return {
        lead: `ssh_key_id: ${key.id} — "${key.name}" (${key.fingerprint}) is now on the account.`,
        payload: {
          ssh_key_id: key.id,
          name: key.name,
          fingerprint: key.fingerprint,
          next_steps:
            "The key is registered but not attached to anything yet — instances take their " +
            "keys at launch. Report the fingerprint so the user can check it matches the key " +
            "they meant to add.",
        },
      };
    },
  }),

  defineWriteTool<{ ssh_key_id: string }>({
    name: "gpu_ssh_keys_delete",
    title: "Delete an SSH key",
    description:
      "Removes an SSH key from the account so it can no longer be attached to new instances. " +
      "This does not reach into instances that were already provisioned with it — their " +
      "authorized_keys are untouched by this call, so access already granted is not revoked " +
      "here. Say that plainly if the user's goal is to cut off access.",
    inputSchema: {
      ssh_key_id: z.string().min(1).describe("The SSH key id, from gpu_ssh_keys_list."),
    },
    annotations: {
      readOnlyHint: false,
      destructiveHint: true,
      idempotentHint: true,
      openWorldHint: true,
    },
    question: "Delete this SSH key now?",
    context: (args) => `deleting SSH key ${args.ssh_key_id}`,
    quote: async (args, config) => {
      // One SDK read, and it is the name and fingerprint that make the id mean
      // something. The second sentence is the honest limit — overclaiming
      // "revokes access" here would be worse than saying nothing, because the
      // user would stop looking for the access that is still live.
      const key = await new SshKeysApi(config).getSshKey({ id: args.ssh_key_id });

      const summary =
        `Delete SSH key "${key.name}" (${key.fingerprint}) from this account — it can no ` +
        "longer be attached to new instances. Instances already provisioned with it keep it " +
        "in their own authorized_keys, which this call does not touch, so this does not by " +
        "itself revoke access to machines that are already running.";

      return {
        summary,
        resolvedParams: {
          ssh_key_id: key.id,
          name: key.name,
          fingerprint: key.fingerprint,
        },
      };
    },
    execute: async (args, config) => {
      await new SshKeysApi(config).deleteSshKey({ id: args.ssh_key_id });

      return {
        lead: `Deleted SSH key ${args.ssh_key_id} from the account.`,
        payload: {
          ssh_key_id: args.ssh_key_id,
          deleted: true,
          access_note:
            "Running instances provisioned with this key may still accept it. If the goal was " +
            "to cut off access, that has to be done on the instances themselves.",
        },
      };
    },
  }),

  defineWriteTool<{ instance_id: string; name: string }>({
    name: "gpu_instances_update",
    title: "Rename a GPU instance",
    description:
      "Changes an instance's display name. That is the only mutable field — this tool cannot " +
      "resize, move, restart or reconfigure anything, and the schema rejects any other field " +
      "rather than ignoring it. Cost, connection details and state are untouched.",
    inputSchema: {
      instance_id: z.string().min(1).describe("The instance id, from gpu_instances_list."),
      name: z.string().min(1).describe("The new display name."),
    },
    annotations: {
      readOnlyHint: false,
      destructiveHint: false,
      idempotentHint: true,
      openWorldHint: true,
    },
    question: "Rename this instance now?",
    context: (args) => `renaming instance ${args.instance_id}`,
    quote: async (args, config) => {
      const instance = await new InstancesApi(config).getInstance({ id: args.instance_id });
      const previous = instance.name === undefined ? "(unnamed)" : `"${instance.name}"`;

      const summary =
        `Rename instance ${instance.id} from ${previous} to "${args.name}" — a display-name ` +
        `change only. The machine (${instance.gpuCount}x ${instance.gpuType} in ` +
        `${instance.region}, currently ${instance.status}) keeps running and keeps costing ` +
        "exactly what it did; nothing about its connection details changes.";

      return {
        summary,
        resolvedParams: {
          instance_id: instance.id,
          name: args.name,
          previous_name: instance.name ?? null,
        },
      };
    },
    execute: async (args, config) => {
      const instance = await new InstancesApi(config).updateInstance({
        id: args.instance_id,
        updateInstanceRequest: { name: args.name },
      });

      return {
        lead: `Renamed ${instance.id} to "${instance.name ?? args.name}".`,
        payload: {
          instance_id: instance.id,
          name: instance.name ?? args.name,
          status: instance.status,
          cost_note:
            "Renaming changes nothing about what this instance costs. It keeps accruing " +
            "charges at its usual rate until it is terminated.",
        },
      };
    },
  }),
];

/** The tier's names, in registration order. */
export const WRITE_TOOL_NAMES: readonly string[] = writeTools.map((t) => t.name);

/** Look a tool up by name. Returns undefined rather than throwing. */
export function findWriteTool(name: string): WriteToolDefinition | undefined {
  return writeTools.find((t) => t.name === name);
}
