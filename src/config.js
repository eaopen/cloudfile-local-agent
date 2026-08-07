import path from 'node:path';
import process from 'node:process';

const defaultPort = 4317;

export function loadConfig(env = process.env) {
  const port = Number(env.CLOUDFILE_AGENT_PORT ?? defaultPort);
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    throw new Error('CLOUDFILE_AGENT_PORT must be a valid TCP port');
  }

  const token = env.CLOUDFILE_AGENT_TOKEN;
  if (!token || token.length < 16) {
    throw new Error('Set CLOUDFILE_AGENT_TOKEN to a value of at least 16 characters');
  }

  return {
    host: env.CLOUDFILE_AGENT_HOST ?? '127.0.0.1',
    port,
    token,
    sharedRoot: path.resolve(env.CLOUDFILE_AGENT_ROOT ?? process.cwd()),
    statePath: path.resolve(env.CLOUDFILE_AGENT_STATE ?? path.join(process.cwd(), 'data', 'state.json')),
  };
}
