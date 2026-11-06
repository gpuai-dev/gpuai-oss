# GPU.ai Open Source MCP server

A local stdio [MCP](https://modelcontextprotocol.io) server for a self-hosted
gpuctl server. It exposes 15 tools: ten reads (GPU types, pricing,
environments, instances, operations, SSH keys, health and auth status), three
confirmed writes (SSH key create and delete, instance rename) and two guarded
tools that quote before they act (instance create and terminate).

```sh
npm ci
npm run build
GPUAI_API_KEY=<key> GPUAI_API_BASE=http://localhost:8080/v1 node dist/index.js
```

Client configuration, the tool list and the confirmation flow are documented
in [docs/mcp.md](../docs/mcp.md).
