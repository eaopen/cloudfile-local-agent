import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { mkdtemp, mkdir, rm } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { createAgentServer } from '../src/server.js';
import { Store } from '../src/store.js';

let root;
let server;
let base;
const config = { token: 'a-safe-development-token', sharedRoot: '', statePath: '' };

before(async () => {
  root = await mkdtemp(path.join(os.tmpdir(), 'cloudfile-agent-'));
  await mkdir(path.join(root, 'demo'));
  config.sharedRoot = root;
  config.statePath = path.join(root, 'state.json');
  const store = new Store(config.statePath);
  await store.load();
  server = createAgentServer(config, store);
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  base = `http://127.0.0.1:${server.address().port}`;
});

after(async () => { await new Promise((resolve) => server.close(resolve)); await rm(root, { recursive: true }); });

const call = (route, options = {}) => fetch(`${base}${route}`, {
  ...options,
  headers: { authorization: `Bearer ${config.token}`, ...(options.headers ?? {}) },
});

test('requires the pairing token', async () => {
  const response = await fetch(`${base}/v1/health`);
  assert.equal(response.status, 401);
});

test('creates a project only within the shared root and queues a task', async () => {
  const created = await call('/v1/projects', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ name: 'Demo', rootPath: path.join(root, 'demo') }) });
  assert.equal(created.status, 201);
  const { project } = await created.json();
  const task = await call('/v1/tasks', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ projectId: project.id, prompt: 'Index this folder' }) });
  assert.equal(task.status, 201);
  const createdTask = (await task.json()).task;
  assert.equal(createdTask.status, 'queued');
  const working = await call(`/v1/tasks/${createdTask.id}`, { method: 'PATCH', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ status: 'working' }) });
  assert.equal(working.status, 200);
  const done = await call(`/v1/tasks/${createdTask.id}`, { method: 'PATCH', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ status: 'done' }) });
  assert.equal(done.status, 200);
  const immutable = await call(`/v1/tasks/${createdTask.id}`, { method: 'PATCH', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ status: 'working' }) });
  assert.equal(immutable.status, 409);
  const rejected = await call('/v1/projects', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ name: 'Elsewhere', rootPath: os.tmpdir() }) });
  assert.equal(rejected.status, 400);
});
