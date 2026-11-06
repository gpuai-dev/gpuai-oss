// Startup configuration for the GPU.ai Open Source MCP server.
//
// Two env vars, both reused verbatim from the `gpu` CLI so one setup covers
// both tools:
//
//   GPUAI_API_KEY   required — the credential, read from the environment only,
//                   never from argv (it would land in `ps` and shell history)
//   GPUAI_API_BASE  optional — the gpuctl server's base URL. Unset means a
//                   server on this machine, http://localhost:8080/v1.
//
// Two rules this module exists to enforce structurally:
//
//  1. Fail fast, offline. The key is presence-checked at startup and nothing
//     else — no network call at boot. A live call here would mean a transient
//     network blip makes the server fail to start, and the operator cannot tell
//     a blip from a bad key. A bad key instead surfaces as a clean mapped error
//     on first real use.
//  2. The credential never reaches output. It is read here, handed straight to
//     the SDK's Configuration, and every error string this module can produce
//     goes through redactSecrets() first.
import { Configuration } from "@gpuai/sdk";

import { readVersion } from "./version.js";

/** Env var holding the API credential. Same name as the `gpu` CLI uses. */
export const API_KEY_ENV = "GPUAI_API_KEY";

/** Env var overriding the API base URL. Same name as the `gpu` CLI uses. */
export const API_BASE_ENV = "GPUAI_API_BASE";

/**
 * The default API base when GPUAI_API_BASE is unset: a gpuctl server on this
 * machine on its default port. Same default as the `gpu` CLI and the SDKs.
 */
export const DEFAULT_API_BASE = "http://localhost:8080/v1";

/** The subset of the environment this module reads. */
export type Env = Record<string, string | undefined>;

/**
 * A configuration problem detected at startup, before any request is made.
 *
 * Carries a `remedy` describing the concrete fix, so the process can print
 * something actionable instead of a bare stack trace.
 */
export class StartupConfigError extends Error {
  readonly remedy: string;

  constructor(message: string, remedy: string) {
    super(message);
    this.name = "StartupConfigError";
    this.remedy = remedy;
  }
}

/**
 * Strip the credential out of arbitrary text.
 *
 * Every error path funnels through here rather than relying on each call site
 * remembering not to interpolate the key. Defence in depth: nothing in this
 * module interpolates the key today, and this makes sure nothing added later
 * can leak it either.
 */
export function redactSecrets(text: string, env: Env = process.env): string {
  const secret = env[API_KEY_ENV];
  if (secret === undefined || secret.trim() === "") {
    return text;
  }
  return text.split(secret).join("[redacted]");
}

/**
 * Render a startup failure as operator-facing text.
 *
 * Always redacted. Never includes a stack trace — the useful information is
 * which env var is wrong and what to set it to.
 */
export function describeStartupError(err: unknown, env: Env = process.env): string {
  if (err instanceof StartupConfigError) {
    return redactSecrets(`${err.message}\n\n${err.remedy}`, env);
  }
  const message = err instanceof Error ? err.message : String(err);
  return redactSecrets(message, env);
}

const KEY_REMEDY = [
  `Set ${API_KEY_ENV} in the environment block of your MCP client configuration,`,
  `alongside the server's command. Create a key on the server with`,
  `\`gpuctl apikey create --name <name>\`.`,
  ``,
  `A dedicated key for agent use is recommended: it can be revoked on its own`,
  `and keeps usage attribution separate.`,
  ``,
  `Optional: set ${API_BASE_ENV} to your gpuctl server's base URL.`,
  `Leave it unset for ${DEFAULT_API_BASE}.`,
].join("\n");

/**
 * Read the API credential from the environment, or fail.
 *
 * Presence check only — no validation call, no network. Returns the value
 * verbatim (keys are opaque; trimming or normalising could corrupt one).
 */
export function requireApiKey(env: Env = process.env): string {
  const raw = env[API_KEY_ENV];
  if (raw === undefined || raw.trim() === "") {
    throw new StartupConfigError(
      `Not authenticated — ${API_KEY_ENV} is not set.`,
      KEY_REMEDY,
    );
  }
  return raw;
}

const BASE_REMEDY = [
  `${API_BASE_ENV} must be an absolute http(s) URL, for example`,
  `https://gpu.example.com/v1`,
  ``,
  `Unset it entirely to use ${DEFAULT_API_BASE}.`,
].join("\n");

/**
 * Resolve the API base URL.
 *
 * Unset or empty resolves to DEFAULT_API_BASE. An override is validated as an
 * absolute http(s) URL and then returned EXACTLY as given — no trailing-slash
 * normalisation, no rewriting. Callers configure what they typed.
 */
export function resolveBaseUrl(env: Env = process.env): string {
  const raw = env[API_BASE_ENV];
  if (raw === undefined || raw.trim() === "") {
    return DEFAULT_API_BASE;
  }

  let parsed: URL;
  try {
    parsed = new URL(raw);
  } catch {
    throw new StartupConfigError(
      `${API_BASE_ENV} is not a valid URL: ${JSON.stringify(raw)}`,
      BASE_REMEDY,
    );
  }

  if (parsed.protocol !== "https:" && parsed.protocol !== "http:") {
    throw new StartupConfigError(
      `${API_BASE_ENV} must use http or https, got ${JSON.stringify(parsed.protocol)}.`,
      BASE_REMEDY,
    );
  }

  return raw;
}

/**
 * Build the SDK client configuration from the environment.
 *
 * The credential is handed to the SDK's Configuration, which owns constructing
 * the bearer header. The header is never assembled by hand here — doing so
 * would duplicate an auth detail the SDK already gets right, and put the raw
 * credential into a second place it could leak from.
 */
export function buildConfiguration(env: Env = process.env): Configuration {
  const accessToken = requireApiKey(env);
  const basePath = resolveBaseUrl(env);
  // A distinct User-Agent lets the server's logs tell MCP calls apart from
  // direct SDK calls. Set here, on the ONE Configuration this package
  // constructs, so no tool can miss it; the SDK merges caller headers over its
  // own default, so this wins.
  return new Configuration({
    accessToken,
    basePath,
    headers: { "User-Agent": `gpuai-mcp/${readVersion()}` },
  });
}
