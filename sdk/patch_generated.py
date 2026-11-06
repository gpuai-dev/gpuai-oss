#!/usr/bin/env python3
"""Deterministic post-generation patches for the generated SDKs.

Run by sdk/generate.sh after openapi-generator. Each rule is an exact-string
replacement on the 7.24.0 output. A rule whose replacement is already present
is skipped, so the script is idempotent. A rule whose anchor is missing fails
the run: a generator upgrade that reshapes the output must be noticed, not
shipped unpatched.

What it changes:
  * Both SDKs read the API base URL from GPUAI_API_BASE when the caller does
    not pass one, falling back to http://localhost:8080/v1 (the spec's server).
  * Both SDKs send a User-Agent naming the SDK instead of the generator.
  * The generated READMEs describe installing from a source checkout. The
    packages are not published to any registry.
"""

import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
VERSION = "0.1.0"
DEFAULT_BASE = "http://localhost:8080/v1"

PY_README_INSTALL = f"""## Installation & Usage

The package is not published to a registry. Install it from a checkout of
this repository:

```sh
pip install ./sdk/python
```

Then import the package:
```python
import gpuai_sdk
```

### Tests

From `sdk/python`, run:

```sh
pip install -r requirements.txt -r test-requirements.txt
pytest
```

## Getting Started

The client talks to a self-hosted gpuctl server. The base URL comes from the
`host` argument, else the `GPUAI_API_BASE` environment variable, else
`{DEFAULT_BASE}`. Create an API key on the server with
`gpuctl apikey create --name <name>`.

```python
import os

import gpuai_sdk
from gpuai_sdk.rest import ApiException
from pprint import pprint

configuration = gpuai_sdk.Configuration(
    host=os.environ.get("GPUAI_API_BASE", "{DEFAULT_BASE}"),
    access_token=os.environ["GPUAI_API_KEY"],
)
"""

TS_README_USAGE = f"""## Usage

The package is not published to a registry. Build it from a checkout of this
repository and install it from the local path:

```bash
cd sdk/typescript && npm install && npm run build
cd /path/to/your/project && npm install /path/to/gpuai-oss/sdk/typescript
```

The client talks to a self-hosted gpuctl server. The base URL comes from the
`basePath` option, else the `GPUAI_API_BASE` environment variable (Node only),
else `{DEFAULT_BASE}`. Create an API key on the server with
`gpuctl apikey create --name <name>`.

```ts
import {{ Configuration, InstancesApi }} from '@gpuai/sdk';

const config = new Configuration({{
  basePath: process.env.GPUAI_API_BASE ?? '{DEFAULT_BASE}',
  accessToken: process.env.GPUAI_API_KEY,
}});

const instances = await new InstancesApi(config).listInstances();
console.log(instances.data);
```
"""


def section(text, start, end):
    """Return the substring from start (inclusive) to end (exclusive)."""
    i = text.index(start)
    j = text.index(end, i)
    return text[i:j]


RULES = {
    "sdk/python/gpuai_sdk/configuration.py": [
        ("import copy\n", "import copy\nimport os\n"),
        (
            f'        self._base_path = "{DEFAULT_BASE}" if host is None else host\n',
            '        if host is None:\n'
            '            host = os.environ.get("GPUAI_API_BASE") or None\n'
            f'        self._base_path = "{DEFAULT_BASE}" if host is None else host\n',
        ),
    ],
    "sdk/python/gpuai_sdk/api_client.py": [
        (f"self.user_agent = 'OpenAPI-Generator/{VERSION}/python'",
         f"self.user_agent = 'gpuai-sdk-python/{VERSION}'"),
    ],
    "sdk/typescript/src/runtime.ts": [
        (
            f'export const BASE_PATH = "{DEFAULT_BASE}".replace(/\\/+$/, "");',
            "declare const process: { env?: Record<string, string | undefined> } | undefined;\n"
            "const ENV_BASE_PATH = typeof process !== 'undefined' ? process?.env?.GPUAI_API_BASE : undefined;\n"
            f'export const BASE_PATH = (ENV_BASE_PATH || "{DEFAULT_BASE}").replace(/\\/+$/, "");',
        ),
        (
            "    get headers(): HTTPHeaders | undefined {\n"
            "        return this.configuration.headers;\n"
            "    }",
            "    get headers(): HTTPHeaders | undefined {\n"
            f"        return {{ 'User-Agent': 'gpuai-sdk-ts/{VERSION}', ...this.configuration.headers }};\n"
            "    }",
        ),
    ],
}


def die(msg):
    print(f"patch_generated: {msg}", file=sys.stderr)
    sys.exit(1)


def apply(path, rules):
    p = ROOT / path
    text = p.read_text(encoding="utf-8")
    for old, new in rules:
        if new in text:
            continue
        if text.count(old) != 1:
            die(f"{path}: anchor not found exactly once: {old[:70]!r}")
        text = text.replace(old, new)
    p.write_text(text, encoding="utf-8")


def patch_python_readme():
    p = ROOT / "sdk/python/README.md"
    text = p.read_text(encoding="utf-8")
    if "The package is not published to a registry." in text:
        return
    old = section(text, "## Installation & Usage", "\n\n\n# Enter a context")
    text = text.replace(old, PY_README_INSTALL.rstrip("\n"))
    p.write_text(text, encoding="utf-8")


def patch_ts_readme():
    p = ROOT / "sdk/typescript/README.md"
    text = p.read_text(encoding="utf-8")
    if "The package is not published to a registry." in text:
        return
    text = text.replace("A TypeScript SDK client for the localhost API.",
                        "TypeScript SDK for the GPU.ai Open Source API.")
    old = section(text, "## Usage", "## Documentation")
    text = text.replace(old, TS_README_USAGE + "\n\n")
    old = section(text, "### Publishing", "## License")
    text = text.replace(old, "")
    text = text.replace("[Apache-2.0]()", "Apache-2.0")
    p.write_text(text, encoding="utf-8")


def patch_ts_package():
    import json
    p = ROOT / "sdk/typescript/package.json"
    pkg = json.loads(p.read_text(encoding="utf-8"))
    pkg["description"] = "TypeScript SDK for the GPU.ai Open Source API"
    pkg["author"] = "GPU.ai Open Source contributors"
    pkg["license"] = "Apache-2.0"
    pkg["private"] = True
    pkg["scripts"]["test"] = "npm run build && node --test test/client.test.mjs"
    p.write_text(json.dumps(pkg, indent=2) + "\n", encoding="utf-8")


def main():
    for path, rules in RULES.items():
        apply(path, rules)
    patch_python_readme()
    patch_ts_readme()
    patch_ts_package()
    print("patch_generated OK")


if __name__ == "__main__":
    main()
