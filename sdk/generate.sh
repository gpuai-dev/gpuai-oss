#!/usr/bin/env bash
# Regenerates sdk/python and sdk/typescript from openapi/v1.yaml.
#
# Needs Node (for npx) and a Java 11+ runtime. On macOS: brew install openjdk.
# The generator version is pinned in openapitools.json at the repo root
# (openapi-generator 7.24.0, driven by @openapitools/openapi-generator-cli 2.40.1).
#
# Usage, from the repository root:
#   sdk/generate.sh
set -euo pipefail

cd "$(dirname "$0")/.."

if ! java -version 2>&1 | grep -qE 'version "(1[1-9]|[2-9][0-9])'; then
  if JH="$(brew --prefix openjdk 2>/dev/null)" && [ -x "$JH/bin/java" ]; then
    export JAVA_HOME="$JH" PATH="$JH/bin:$PATH"
  else
    echo "sdk/generate.sh: need a Java 11+ runtime (brew install openjdk, or actions/setup-java in CI)" >&2
    exit 2
  fi
fi

VERSION=0.1.0
CLI="@openapitools/openapi-generator-cli@2.40.1"

npx --yes "$CLI" generate \
  -i openapi/v1.yaml -g python -o sdk/python \
  --git-user-id gpuai-dev --git-repo-id gpuai-oss \
  --additional-properties "packageName=gpuai_sdk,packageVersion=$VERSION,projectName=gpuai-sdk"

npx --yes "$CLI" generate \
  -i openapi/v1.yaml -g typescript-fetch -o sdk/typescript \
  --git-user-id gpuai-dev --git-repo-id gpuai-oss \
  --additional-properties "npmName=@gpuai/sdk,supportsES6=true,npmVersion=$VERSION"

python3 sdk/patch_generated.py
