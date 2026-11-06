// The roster audit — the tier table as a machine check rather than a note.
//
// A written tier table is exactly as good as the next person's memory of it,
// and the failure it guards against is quiet: a tool that lands in the
// read-only tier gets NO confirmation, so a single misfiling turns a costly or
// mutating operation into a free one — with a `readOnlyHint: true` that tells
// the client not to bother the user about it either.
//
// So this file is the table, transcribed. It asserts the literal per-tier name
// lists rather than counts, because a count survives a swap: move one tool from
// write to read-only and a count check stays green while the guardrail is gone.
//
// It also carries two assertions that are about what is NOT here:
//
//   * the EXCLUDED families. Crypto movement, supplier onboarding, and file
//     uploads are not agent surfaces. A tool from any of those families
//     appearing in the roster is a scope decision that was made without being
//     made.
//   * raw HTTP. The typed-SDK-only discipline is what keeps every request
//     inside a generated client that redacts, maps errors, and cannot be
//     pointed somewhere else by a prompt. It covers the guardrail and every
//     tier file.
//
// Deliberately definition-level: no server, no transport, no network. The wire
// representation is `schemas.test.ts`'s job, and duplicating it here would make
// this file slower without making it stricter.
import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";

import { describe, expect, it } from "vitest";

import { ALL_TOOL_NAMES } from "../src/tools/index.js";
import { READONLY_TOOL_NAMES, readonlyTools } from "../src/tools/readonly/index.js";
import { SPEND_TOOL_NAMES, spendTools } from "../src/tools/spend/index.js";
import { WRITE_TOOL_NAMES, writeTools } from "../src/tools/write/index.js";

// ---------------------------------------------------------------------------
// The table. Each list is one tier, in registration order. Changing a tool's
// tier means editing it here, on purpose, in a diff someone reviews.
// ---------------------------------------------------------------------------

/** Free, non-mutating reads. No confirmation, no token, readOnlyHint: true. */
const READONLY = [
  "gpu_auth_status",
  "gpu_health_get",
  "gpu_types_list",
  "gpu_pricing_list",
  "gpu_environments_list",
  "gpu_instances_list",
  "gpu_instances_get",
  "gpu_operations_get",
  "gpu_ssh_keys_list",
  "gpu_ssh_keys_get",
];

/** Mutations that cost nothing and still need an explicit yes. */
const WRITE = ["gpu_ssh_keys_create", "gpu_ssh_keys_delete", "gpu_instances_update"];

/** The one tool that can create a charge, plus the one tool that destroys. */
const SPEND = ["gpu_instances_create", "gpu_instances_terminate"];

/**
 * Operation families deliberately NOT exposed. Matched as substrings of tool
 * names, so a differently-named member of the same family is caught too.
 */
const EXCLUDED_SUBSTRINGS: ReadonlyArray<[string, string]> = [
  ["crypto", "no agent-initiated crypto movement"],
  ["community", "community supply is an operator surface, not an agent one"],
  ["supplier", "supplier onboarding is not an agent surface"],
  ["file", "no upload surface, so no file tools"],
  ["video", "no media generation surface"],
  ["openapi", "the spec is not a tool; agents read docs, not schemas, through MCP"],
];

// ---------------------------------------------------------------------------
// 1. The roster is exactly the table.
// ---------------------------------------------------------------------------

describe("the advertised roster is exactly the enumerated table", () => {
  it("assigns each tier its literal list, in registration order", () => {
    // Literal, not counted. A count cannot tell a swap from a stable roster,
    // and a swap is the misfiling this file exists to catch.
    expect([...READONLY_TOOL_NAMES]).toEqual(READONLY);
    expect([...WRITE_TOOL_NAMES]).toEqual(WRITE);
    expect([...SPEND_TOOL_NAMES]).toEqual(SPEND);
  });

  it("splits 15 tools 10 / 3 / 2 across the three tiers", () => {
    expect(READONLY).toHaveLength(10);
    expect(WRITE).toHaveLength(3);
    expect(SPEND).toHaveLength(2);
    expect(ALL_TOOL_NAMES).toHaveLength(15);
  });

  it("composes ALL_TOOL_NAMES from the three tiers with no extras and no duplicates", () => {
    expect([...ALL_TOOL_NAMES]).toEqual([...READONLY, ...WRITE, ...SPEND]);
    expect(new Set(ALL_TOOL_NAMES).size).toBe(ALL_TOOL_NAMES.length);
  });

  it("namespaces every tool under gpu_", () => {
    for (const name of ALL_TOOL_NAMES) {
      expect(name, name).toMatch(/^gpu_[a-z0-9_]+$/);
    }
  });
});

// ---------------------------------------------------------------------------
// 2. The tier a tool is in is the guardrail it gets.
//    These are the assertions that make a misfiling FAIL rather than merely
//    look odd: the annotations and the confirmation parameter are how a tier
//    difference reaches a client at all.
// ---------------------------------------------------------------------------

describe("tier membership carries its guardrail", () => {
  it("flags every read-only tool read-only, and gives it no confirmation parameter", () => {
    for (const definition of readonlyTools) {
      expect(definition.annotations.readOnlyHint, definition.name).toBe(true);
      expect(Object.keys(definition.inputSchema), definition.name).not.toContain(
        "confirmation_token",
      );
    }
  });

  it("flags every write and spend tool NOT read-only, and gives it a confirmation parameter", () => {
    for (const definition of [...writeTools, ...spendTools]) {
      expect(definition.annotations.readOnlyHint, definition.name).toBe(false);
      expect(Object.keys(definition.inputSchema), definition.name).toContain(
        "confirmation_token",
      );
    }
  });

  it("marks exactly the two take-away tools destructive", () => {
    // Destructive means "removes something the user has", not "expensive".
    // Conflating the two makes the genuinely irreversible tools indistinguish-
    // able in a client's UI, which trains click-through on the ones that matter.
    // gpu_instances_create does NOT qualify: it adds a paid instance, which is
    // expensive rather than destructive.
    const destructive = new Set(["gpu_instances_terminate", "gpu_ssh_keys_delete"]);
    for (const definition of [...readonlyTools, ...writeTools, ...spendTools]) {
      const expected = destructive.has(definition.name);
      expect(Boolean(definition.annotations.destructiveHint), definition.name).toBe(expected);
    }
  });
});

// ---------------------------------------------------------------------------
// 3. What is deliberately absent stays absent.
// ---------------------------------------------------------------------------

describe("excluded operation families are not in the roster", () => {
  it("advertises nothing from an excluded family", () => {
    for (const [fragment, why] of EXCLUDED_SUBSTRINGS) {
      const offenders = ALL_TOOL_NAMES.filter((name) => name.includes(fragment));
      expect(offenders, `${fragment} — ${why}`).toEqual([]);
    }
  });
});

// ---------------------------------------------------------------------------
// 4. Every request goes through the typed SDK.
// ---------------------------------------------------------------------------

/** The files that are allowed to talk to the network, and must do it typed. */
const NETWORK_ADJACENT_SOURCES = [
  "src/tools/readonly/index.ts",
  "src/tools/write/index.ts",
  "src/tools/spend/index.ts",
  "src/tools/index.ts",
  "src/guardrail/handshake.ts",
  "src/guardrail/elicitation.ts",
  "src/errors.ts",
  "src/auth.ts",
];

/**
 * Resolve a package-relative source path.
 *
 * `import.meta.url` is unavailable here — the typecheck project builds this
 * file as CommonJS — so the anchor is the working directory, tried both as the
 * package root and as a repo root containing it. A path that resolves to
 * nothing THROWS rather than silently scanning zero files, because a source
 * scan that quietly reads nothing is a gate that passes on everything.
 */
function sourcePath(relative: string): string {
  for (const base of [process.cwd(), resolve(process.cwd(), "mcp")]) {
    const candidate = resolve(base, relative);
    if (existsSync(candidate)) {
      return candidate;
    }
  }
  throw new Error(`could not locate ${relative} from ${process.cwd()}`);
}

/**
 * Drop comments so the gate cannot be satisfied by the words that describe it.
 *
 * The rule is checked against CODE. Every one of these files explains the
 * no-raw-fetch discipline in its own header, and a scan that matched those
 * sentences would fail on files that are in fact compliant — a gate that fires
 * on its own documentation gets deleted, not fixed.
 *
 * Block comments go first, then line comments, with `://` protected so a URL
 * inside a string does not truncate the rest of its line.
 */
function stripComments(source: string): string {
  return source.replace(/\/\*[\s\S]*?\*\//g, "").replace(/(^|[^:])\/\/.*$/gm, "$1");
}

describe("no tool or guardrail file issues raw HTTP", () => {
  it("finds zero fetch call sites in the network-adjacent sources", () => {
    let scanned = 0;

    for (const relative of NETWORK_ADJACENT_SOURCES) {
      const code = stripComments(readFileSync(sourcePath(relative), "utf8"));
      scanned += 1;

      // A raw fetch is how a request escapes the generated client: no error
      // mapping, no redaction, and a URL a prompt could influence.
      expect(/\bfetch\s*\(/.test(code), `${relative} contains a fetch call site`).toBe(false);
    }

    // The scan is only worth anything if it actually read the files.
    expect(scanned).toBe(NETWORK_ADJACENT_SOURCES.length);
    expect(scanned).toBeGreaterThanOrEqual(4);
  });

  it("would notice a fetch call site if one were added", () => {
    // The gate's own negative control: without this, a stripComments bug that
    // deleted everything would leave every assertion above passing forever.
    expect(/\bfetch\s*\(/.test(stripComments("const r = await fetch(url);"))).toBe(true);
    expect(/\bfetch\s*\(/.test(stripComments("// no direct fetch call here"))).toBe(false);
  });
});
