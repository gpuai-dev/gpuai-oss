#!/usr/bin/env bash
# Fails if any tracked file contains a string that must never ship in
# GPU.ai Open Source: names of suppliers other than generic, vultr and mock,
# internal hostnames and deployment names, cloud account identifiers, hosted
# service endpoints, and anything shaped like a live credential.
#
# Usage, from anywhere inside the repository:
#   scripts/check-forbidden.sh            # scan tracked files
#   scripts/check-forbidden.sh FILE...    # scan only these files
#
# Matching is case-insensitive and anchored on word boundaries, so ordinary
# words that merely contain a forbidden name do not match. Allowed exceptions
# are listed in ALLOW below, each with the reason it is safe.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

if [ "$#" -gt 0 ]; then
  printf '%s\0' "$@" > "${TMPDIR:-/tmp}/check-forbidden.$$"
else
  git ls-files -z > "${TMPDIR:-/tmp}/check-forbidden.$$"
fi
trap 'rm -f "${TMPDIR:-/tmp}/check-forbidden.$$"' EXIT

python3 - "${TMPDIR:-/tmp}/check-forbidden.$$" <<'PY'
import re
import sys

# (name, regex). Every regex is compiled case-insensitive.
RULES = [
    # Supplier names. Only generic, vultr and mock may appear.
    ("supplier: runpod", r"\brun[\s_-]?pod\b"),
    ("supplier: vast", r"\bvast(?:[\s._-]?ai)?\b"),
    ("supplier: hyperstack", r"\bhyperstack\b"),
    ("supplier: massed compute", r"\bmassed(?:[\s_-]?compute)?\b"),
    ("supplier: jarvis", r"\bjarvis(?:labs)?\b"),
    ("supplier: latitude", r"\blatitude(?:\.sh)?\b"),
    ("supplier: gcore", r"\bg[\s_-]?core\b"),
    ("supplier: ionos", r"\bionos\b"),
    ("supplier: e2e networks", r"\be2e[\s_-]?networks\b"),
    ("supplier: io.net", r"\bio[\s._-]?net\b"),
    ("supplier: together", r"\btogether(?:\.ai|[\s_-]?ai|[\s_-]?computer)\b"),
    ("supplier: fireworks", r"\bfireworks(?:\.ai)?\b"),
    ("supplier: openrouter", r"\bopen[\s_-]?router\b"),
    ("supplier: ecohash", r"\becohash\b"),
    # Internal deployments, hostnames and hosted services.
    ("internal deployment name", r"\bgpuai-(?:prod|demo)\b"),
    ("AWS endpoint", r"amazonaws\.com"),
    ("AWS ARN with account id", r"\barn:aws[a-z-]*:[a-z0-9-]*:[a-z0-9-]*:\d{12}\b"),
    ("AWS access key id", r"\b(?:AKIA|ASIA)[0-9A-Z]{16}\b"),
    ("Sentry DSN", r"https?://[0-9a-f]{16,}@[a-z0-9.-]*sentry\.io"),
    ("Sentry host", r"\bsentry\.io\b"),
    ("Slack webhook", r"hooks\.slack\.com"),
    ("Clerk instance", r"clerk\.accounts"),
    # Live credentials.
    ("Stripe live key", r"\b[spr]k_live_"),
    ("GPU.ai API key", r"\bgpuai_live_[A-Za-z0-9]{24,}"),
    ("GitHub token", r"\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36}\b|\bgithub_pat_[A-Za-z0-9_]{40,}"),
    ("Slack token", r"\bxox[abposr]-[A-Za-z0-9-]{10,}"),
    ("OpenAI/Anthropic key", r"\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{32,}"),
    ("private key", r"-----BEGIN (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----"),
]

# Allowed exceptions: (path regex, rule name, line regex, reason).
ALLOW = [
    # This script spells out every pattern it forbids.
    (r"^scripts/check-forbidden\.sh$", None, r".*", "the pattern list itself"),
]

compiled = [(n, re.compile(r, re.IGNORECASE)) for n, r in RULES]
allow = [(re.compile(p), n, re.compile(l)) for p, n, l, _ in ALLOW]

paths = [p for p in open(sys.argv[1], "rb").read().decode().split("\0") if p]
hits = 0
for path in paths:
    try:
        data = open(path, "rb").read()
    except (IsADirectoryError, FileNotFoundError):
        continue
    if b"\0" in data[:8192]:
        continue  # binary
    text = data.decode("utf-8", "replace")
    for lineno, line in enumerate(text.splitlines(), 1):
        for name, rx in compiled:
            m = rx.search(line)
            if not m:
                continue
            if any(p.search(path) and (an is None or an == name) and lx.search(line)
                   for p, an, lx in allow):
                continue
            hits += 1
            print(f"{path}:{lineno}: [{name}] {line.strip()[:160]}")

if hits:
    print(f"\ncheck-forbidden: {hits} forbidden string(s) found", file=sys.stderr)
    sys.exit(1)
print(f"check-forbidden: OK ({len(paths)} files scanned)")
PY
