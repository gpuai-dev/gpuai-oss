// Tests for the generated client against a local stub server. Run with
// `npm test`, which builds dist/ first.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { createRequire } from 'node:module';

const require = createRequire(import.meta.url);

function startStub(handler) {
  return new Promise((resolve) => {
    const server = http.createServer(handler);
    server.listen(0, '127.0.0.1', () => resolve(server));
  });
}

test('default base path is the local gpuctl server', () => {
  delete process.env.GPUAI_API_BASE;
  delete require.cache[require.resolve('../dist/runtime.js')];
  const { BASE_PATH } = require('../dist/runtime.js');
  assert.equal(BASE_PATH, 'http://localhost:8080/v1');
});

test('GPUAI_API_BASE overrides the default base path', () => {
  process.env.GPUAI_API_BASE = 'http://gpu.example.test:9000/v1/';
  delete require.cache[require.resolve('../dist/runtime.js')];
  const { BASE_PATH } = require('../dist/runtime.js');
  assert.equal(BASE_PATH, 'http://gpu.example.test:9000/v1');
  delete process.env.GPUAI_API_BASE;
});

test('listInstances sends the bearer key and SDK user agent', async () => {
  let seen;
  const server = await startStub((req, res) => {
    seen = { method: req.method, url: req.url, auth: req.headers.authorization, ua: req.headers['user-agent'] };
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({
      data: [{
        id: 'inst_1', status: 'running', gpu_type: 'rtx_4090', gpu_count: 1,
        region: 'us-east', tier: 'on_demand', price_per_hour: 0.5,
        created_at: '2026-01-01T00:00:00Z',
      }],
      next_cursor: null,
    }));
  });
  try {
    const { Configuration, InstancesApi } = require('../dist/index.js');
    const { port } = server.address();
    const api = new InstancesApi(new Configuration({
      basePath: `http://127.0.0.1:${port}/v1`,
      accessToken: 'gpuai_test_key',
    }));
    const page = await api.listInstances({ status: 'running' });
    assert.equal(seen.method, 'GET');
    assert.equal(seen.url, '/v1/instances?status=running');
    assert.equal(seen.auth, 'Bearer gpuai_test_key');
    assert.match(seen.ua, /^gpuai-sdk-ts\//);
    assert.equal(page.data.length, 1);
    assert.equal(page.data[0].gpuType, 'rtx_4090');
    assert.equal(page.nextCursor, null);
  } finally {
    server.close();
  }
});

test('createInstance posts the request body and returns the operation', async () => {
  let body;
  const server = await startStub((req, res) => {
    let raw = '';
    req.on('data', (c) => { raw += c; });
    req.on('end', () => {
      body = JSON.parse(raw);
      res.statusCode = 202;
      res.setHeader('Content-Type', 'application/json');
      res.setHeader('Operation-Id', '7b0c1f4e-0000-4000-8000-000000000001');
      res.end(JSON.stringify({
        operation_id: '7b0c1f4e-0000-4000-8000-000000000001',
        kind: 'instance.create', state: 'pending', created_at: '2026-01-01T00:00:00Z',
      }));
    });
  });
  try {
    const { Configuration, InstancesApi } = require('../dist/index.js');
    const { port } = server.address();
    const api = new InstancesApi(new Configuration({ basePath: `http://127.0.0.1:${port}/v1`, accessToken: 'k' }));
    const op = await api.createInstance({
      createInstanceRequest: { gpuType: 'rtx_4090', gpuCount: 1, tier: 'on_demand', sshKeyIds: ['key_1'] },
    });
    assert.deepEqual(body, { gpu_type: 'rtx_4090', gpu_count: 1, tier: 'on_demand', ssh_key_ids: ['key_1'] });
    assert.equal(op.kind, 'instance.create');
    assert.equal(op.state, 'pending');
  } finally {
    server.close();
  }
});
