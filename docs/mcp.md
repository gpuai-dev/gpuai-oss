# MCP server

`mcp/` is a [Model Context Protocol](https://modelcontextprotocol.io) server
that lets an MCP client (Claude Code, Claude Desktop, or any other) look up GPU
types and prices, launch and terminate instances, and manage SSH keys on a
GPU.ai Open Source server.

It is a local stdio server. The client starts it as a subprocess and talks
JSON-RPC over stdin and stdout. It calls your gpuctl server directly with the
TypeScript SDK in `sdk/typescript`, and nothing else is contacted.

## Build

Requires Node 22 or newer.

```sh
cd mcp
npm ci
npm run build      # builds sdk/typescript first, then mcp/dist/
```

The entry point is `mcp/dist/index.js`. The package is not published, so
clients run it from the checkout.

## Configuration

| Variable | Required | Meaning |
| --- | --- | --- |
| `GPUAI_API_KEY` | yes | API key, created on the server with `gpuctl apikey create --name <name>`. The server refuses to start without it. |
| `GPUAI_API_BASE` | no | Base URL of the gpuctl API, including `/v1`. Defaults to `http://localhost:8080/v1`. Must be an absolute http or https URL. |

These are the same variables the `gpu` CLI reads. Pass them only through the
client's `env` block, never as command-line arguments, which show up in `ps`
output and shell history.

Startup makes no network call. A wrong key or an unreachable server shows up as
an error on the first tool call. The API key is never included in a tool
result or error message, and per-instance web-console passwords are redacted
from results.

### Claude Code

Add a `.mcp.json` to your project (or use `claude mcp add`).

```json
{
  "mcpServers": {
    "gpuai": {
      "type": "stdio",
      "command": "node",
      "args": ["/path/to/gpuai-oss/mcp/dist/index.js"],
      "env": {
        "GPUAI_API_KEY": "${GPUAI_API_KEY}",
        "GPUAI_API_BASE": "http://localhost:8080/v1"
      }
    }
  }
}
```

Claude Code expands `${GPUAI_API_KEY}` from your shell when it starts the
server, so the key stays out of the file. The equivalent command is:

```sh
claude mcp add gpuai --env GPUAI_API_KEY="$GPUAI_API_KEY" \
  --env GPUAI_API_BASE=http://localhost:8080/v1 \
  -- node /path/to/gpuai-oss/mcp/dist/index.js
```

### Claude Desktop

Edit `claude_desktop_config.json`
(macOS: `~/Library/Application Support/Claude/claude_desktop_config.json`,
Windows: `%APPDATA%\Claude\claude_desktop_config.json`). Claude Desktop does
not expand shell variables, so the key is written literally. Restart the app
after editing.

```json
{
  "mcpServers": {
    "gpuai": {
      "command": "node",
      "args": ["/path/to/gpuai-oss/mcp/dist/index.js"],
      "env": {
        "GPUAI_API_KEY": "<your API key>",
        "GPUAI_API_BASE": "http://localhost:8080/v1"
      }
    }
  }
}
```

Then ask the agent to run `gpu_auth_status` to confirm the key and URL work.

## Tools

There are 15 tools in three tiers. The tier decides what happens before a tool
runs, and the server enforces it.

### Read-only (no confirmation)

| Tool | Endpoint |
| --- | --- |
| `gpu_auth_status` | `GET /v1/instances?limit=5` (proves the key works, reports the API base) |
| `gpu_health_get` | `GET /v1/health` |
| `gpu_types_list` | `GET /v1/gpu-types` |
| `gpu_pricing_list` | `GET /v1/pricing` |
| `gpu_environments_list` | `GET /v1/environments` |
| `gpu_instances_list` | `GET /v1/instances` |
| `gpu_instances_get` | `GET /v1/instances/{id}` |
| `gpu_operations_get` | `GET /v1/operations/{id}` |
| `gpu_ssh_keys_list` | `GET /v1/ssh-keys` |
| `gpu_ssh_keys_get` | `GET /v1/ssh-keys/{id}` |

### Writes (one confirmation)

| Tool | Endpoint |
| --- | --- |
| `gpu_ssh_keys_create` | `POST /v1/ssh-keys` |
| `gpu_ssh_keys_delete` | `DELETE /v1/ssh-keys/{id}` |
| `gpu_instances_update` | `PATCH /v1/instances/{id}` (rename only) |

### Guarded (quote, then confirm)

| Tool | Endpoint |
| --- | --- |
| `gpu_instances_create` | `GET /v1/ssh-keys` and `GET /v1/pricing` to quote, then `POST /v1/instances` |
| `gpu_instances_terminate` | `GET /v1/instances/{id}` to describe, then `DELETE /v1/instances/{id}` |

### How confirmation works

A write or guarded tool does nothing on its first call. It returns a summary
(for `gpu_instances_create`, the hourly price of the cheapest matching offer
and a 24-hour estimate; for terminate, what will be destroyed) and a single-use
`confirmation_token`. Only a second call with that token and the same
parameters executes. The token:

- is bound to the tool and to the exact parameters the summary described,
- expires after five minutes,
- is spent before the action runs, so a failed or retried call cannot reuse it.

When the client supports MCP elicitation, the server asks the user directly
with a confirm dialog instead, through the same token path.

`gpu_instances_create` also requires `gpu_type`, `gpu_count` (1 to 8) and
`tier` explicitly. If `max_price_per_hour` is not given, the quoted price
becomes the ceiling, so the server refuses to place the instance above the
number the user approved. The quote is refused when nothing can be priced or
when the account has no SSH key to attach.

Create and terminate are asynchronous. Both return an operation id; poll it
with `gpu_operations_get` on a backoff (2 seconds, growing by 1 second up to
10).

## Tests

```sh
cd mcp
npm test           # vitest, offline; builds sdk/typescript first
npm run typecheck  # tsc over src and test
```
