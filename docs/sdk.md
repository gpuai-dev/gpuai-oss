# SDKs (Python and TypeScript)

The Python and TypeScript SDKs are generated from `openapi/v1.yaml` and cover
every endpoint in it: health, the OpenAPI document, GPU types, pricing,
environments, instances, operations, SSH keys and registry credentials.

Neither package is published to a registry. Install them from a checkout of
this repository.

## Configuration

Both SDKs resolve the API base URL the same way.

1. The base URL you pass in code (`host` in Python, `basePath` in TypeScript).
2. The `GPUAI_API_BASE` environment variable (in TypeScript, only under Node).
3. `http://localhost:8080/v1`.

The base URL includes the `/v1` prefix. Authenticate with an API key created on
the server with `gpuctl apikey create --name <name>`. The SDKs send it as
`Authorization: Bearer <key>`. The examples below read it from `GPUAI_API_KEY`,
the same variable the CLI and MCP server use.

## Python

Requires Python 3.10 or newer.

```sh
pip install ./sdk/python
```

Launch an instance, wait for it, print its SSH command, then terminate it.

```python
import os
import time

import gpuai_sdk

config = gpuai_sdk.Configuration(
    host=os.environ.get("GPUAI_API_BASE", "http://localhost:8080/v1"),
    access_token=os.environ["GPUAI_API_KEY"],
)

TERMINAL = {"succeeded", "failed", "cancelled"}


def wait(ops, operation_id, timeout=600):
    deadline = time.monotonic() + timeout
    delay = 2
    while time.monotonic() < deadline:
        op = ops.get_operation(id=operation_id)
        if op.state in TERMINAL:
            return op
        time.sleep(delay)
        delay = min(delay + 1, 10)
    raise TimeoutError(operation_id)


with gpuai_sdk.ApiClient(config) as client:
    pricing = gpuai_sdk.PricingApi(client).list_pricing(gpu_type="rtx_4090")
    for offer in pricing.data:
        print(offer.gpu_count, offer.region, offer.price_per_hour, offer.available)

    keys = gpuai_sdk.SshKeysApi(client).list_ssh_keys()
    instances = gpuai_sdk.InstancesApi(client)
    ops = gpuai_sdk.OperationsApi(client)

    op = instances.create_instance(
        gpuai_sdk.CreateInstanceRequest(
            gpu_type="rtx_4090",
            gpu_count=1,
            tier="on_demand",
            ssh_key_ids=[keys.data[0].id],
            max_price_per_hour=1.00,
        )
    )
    op = wait(ops, op.operation_id)
    if op.state != "succeeded":
        raise RuntimeError(op.error.detail if op.error else op.state)

    instance = instances.get_instance(id=op.resource_id)
    print(instance.status, instance.connection.ssh_command)

    done = wait(ops, instances.delete_instance(id=instance.id).operation_id)
    print("terminated:", done.state)
```

Errors raise `gpuai_sdk.ApiException` (with subclasses such as
`UnauthorizedException` and `NotFoundException`). The response body is an
RFC 7807 problem document whose `code` field is stable.

```python
from gpuai_sdk.exceptions import ApiException

try:
    instances.get_instance(id="does-not-exist")
except ApiException as e:
    print(e.status, e.body)
```

Run the tests from `sdk/python`.

```sh
pip install -r requirements.txt -r test-requirements.txt
pip install .
pytest
```

Per-endpoint and per-model reference pages are in `sdk/python/docs/`.

## TypeScript

Requires Node 18 or newer (the client uses the global `fetch`).

```sh
cd sdk/typescript && npm install && npm run build
# then, from your project
npm install /path/to/gpuai-oss/sdk/typescript
```

The package builds CommonJS to `dist/` and ES modules to `dist/esm/`. Model
fields are camelCase in TypeScript (`gpuType`, `pricePerHour`) and snake_case
on the wire.

```ts
import {
  Configuration,
  InstancesApi,
  OperationsApi,
  PricingApi,
  ResponseError,
  type Operation,
} from '@gpuai/sdk';

const config = new Configuration({
  basePath: process.env.GPUAI_API_BASE ?? 'http://localhost:8080/v1',
  accessToken: process.env.GPUAI_API_KEY,
});

const instances = new InstancesApi(config);
const operations = new OperationsApi(config);

async function wait(id: string, timeoutMs = 600_000): Promise<Operation> {
  const deadline = Date.now() + timeoutMs;
  let delay = 2_000;
  while (Date.now() < deadline) {
    const op = await operations.getOperation({ id });
    if (['succeeded', 'failed', 'cancelled'].includes(op.state)) return op;
    await new Promise((r) => setTimeout(r, delay));
    delay = Math.min(delay + 1_000, 10_000);
  }
  throw new Error(`operation ${id} did not finish`);
}

const offers = await new PricingApi(config).listPricing({ gpuType: 'rtx_4090' });
console.table(offers.data.map((o) => ({ count: o.gpuCount, region: o.region, price: o.pricePerHour })));

try {
  const created = await instances.createInstance({
    createInstanceRequest: {
      gpuType: 'rtx_4090',
      gpuCount: 1,
      tier: 'on_demand',
      sshKeyIds: ['key_123'],
      maxPricePerHour: 1.0,
    },
  });
  const op = await wait(created.operationId);
  if (op.state !== 'succeeded') throw new Error(op.error?.detail ?? op.state);

  const instance = await instances.getInstance({ id: op.resourceId! });
  console.log(instance.status, instance.connection?.sshCommand);

  const deleted = await instances.deleteInstance({ id: instance.id });
  console.log('terminated:', (await wait(deleted.operationId)).state);
} catch (err) {
  if (err instanceof ResponseError) {
    console.error(err.response.status, await err.response.text());
  } else {
    throw err;
  }
}
```

Run the tests from `sdk/typescript`. `npm test` builds the package, then runs
`node --test` against a local stub server.

```sh
npm install
npm test
```

Per-endpoint and per-model reference pages are in `sdk/typescript/docs/`.

## Regenerating

Change `openapi/v1.yaml`, then run the generator from the repository root. It
needs Node and a Java 11+ runtime.

```sh
sdk/generate.sh
```

The script runs openapi-generator 7.24.0 (through
`@openapitools/openapi-generator-cli` 2.40.1, pinned in `openapitools.json`)
with the `python` and `typescript-fetch` generators, then applies
`sdk/patch_generated.py`. The patch sets the environment-variable base URL
default, an SDK `User-Agent` (`gpuai-sdk-python/<version>`,
`gpuai-sdk-ts/<version>`) and the README install instructions. Generated files
are committed. Hand-written tests live in `sdk/python/test/test_oss_client.py`
and `sdk/typescript/test/`, which the generator does not touch.
