import { createServer } from 'node:http';
import { stat } from 'node:fs/promises';
import path from 'node:path';
import { timingSafeEqual } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { loadConfig } from './config.js';
import { Store } from './store.js';

const json = (response, status, body) => {
  response.writeHead(status, { 'content-type': 'application/json; charset=utf-8', 'cache-control': 'no-store' });
  response.end(JSON.stringify(body));
};

const readJson = async (request) => {
  let raw = '';
  for await (const chunk of request) {
    raw += chunk;
    if (raw.length > 64 * 1024) throw new Error('Request body is too large');
  }
  try { return JSON.parse(raw || '{}'); } catch { throw new Error('Request body must be JSON'); }
};

const validToken = (actual, expected) => {
  if (typeof actual !== 'string') return false;
  const left = Buffer.from(actual);
  const right = Buffer.from(expected);
  return left.length === right.length && timingSafeEqual(left, right);
};

const inSharedRoot = (target, root) => target === root || target.startsWith(`${root}${path.sep}`);

function validateProject(input, config) {
  if (!input || typeof input !== 'object') throw new Error('Request body must be an object');
  const name = input.name?.trim();
  if (!name || name.length > 80) throw new Error('Project name must contain 1–80 characters');
  if (typeof input.rootPath !== 'string' || !input.rootPath.trim()) throw new Error('Project folder is required');
  const rootPath = path.resolve(input.rootPath);
  if (!inSharedRoot(rootPath, config.sharedRoot)) throw new Error('Project folder must be inside CLOUDFILE_AGENT_ROOT');
  return { name, rootPath };
}

export function createAgentServer(config, store) {
  return createServer(async (request, response) => {
    try {
      if (request.method === 'OPTIONS') {
        response.writeHead(204, { 'access-control-allow-origin': '*', 'access-control-allow-headers': 'authorization, content-type', 'access-control-allow-methods': 'GET, POST, OPTIONS' });
        return response.end();
      }
      if (!validToken(request.headers.authorization?.replace(/^Bearer\s+/i, ''), config.token)) {
        return json(response, 401, { error: 'unauthorized' });
      }
      const url = new URL(request.url, 'http://localhost');
      if (request.method === 'GET' && url.pathname === '/v1/health') {
        return json(response, 200, { status: 'ready', sharedRoot: config.sharedRoot });
      }
      if (request.method === 'GET' && url.pathname === '/v1/projects') {
        return json(response, 200, { projects: store.listProjects() });
      }
      if (request.method === 'POST' && url.pathname === '/v1/projects') {
        const project = validateProject(await readJson(request), config);
        const info = await stat(project.rootPath).catch(() => null);
        if (!info?.isDirectory()) return json(response, 422, { error: 'Project folder does not exist or is not a directory' });
        return json(response, 201, { project: await store.createProject(project) });
      }
      if (request.method === 'POST' && url.pathname === '/v1/tasks') {
        const { projectId, prompt } = await readJson(request);
        if (typeof projectId !== 'string' || typeof prompt !== 'string' || !prompt.trim() || prompt.length > 4000) {
          return json(response, 400, { error: 'projectId and a 1–4000 character prompt are required' });
        }
        const task = await store.createTask({ projectId, prompt: prompt.trim() });
        return task ? json(response, 201, { task }) : json(response, 404, { error: 'Project not found' });
      }
      return json(response, 404, { error: 'not_found' });
    } catch (error) {
      return json(response, 400, { error: error.message });
    }
  });
}

async function main() {
  const config = loadConfig();
  const store = new Store(config.statePath);
  await store.load();
  const server = createAgentServer(config, store);
  server.listen(config.port, config.host, () => console.log(`CloudFile Local Agent listening at http://${config.host}:${config.port}`));
}

if (process.argv[1] === fileURLToPath(import.meta.url)) main();
