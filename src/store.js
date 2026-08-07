import { mkdir, readFile, rename, writeFile } from 'node:fs/promises';
import path from 'node:path';
import { randomUUID } from 'node:crypto';

const emptyState = () => ({ version: 1, projects: [], tasks: [] });

export class Store {
  constructor(statePath) {
    this.statePath = statePath;
    this.state = emptyState();
  }

  async load() {
    try {
      this.state = JSON.parse(await readFile(this.statePath, 'utf8'));
    } catch (error) {
      if (error.code !== 'ENOENT') throw error;
      await this.save();
    }
  }

  async save() {
    await mkdir(path.dirname(this.statePath), { recursive: true });
    const pending = `${this.statePath}.${process.pid}.tmp`;
    await writeFile(pending, `${JSON.stringify(this.state, null, 2)}\n`, { mode: 0o600 });
    await rename(pending, this.statePath);
  }

  listProjects() {
    return [...this.state.projects].sort((a, b) => b.updatedAt.localeCompare(a.updatedAt));
  }

  async createProject({ name, rootPath }) {
    const now = new Date().toISOString();
    const project = { id: randomUUID(), name, rootPath, createdAt: now, updatedAt: now };
    this.state.projects.push(project);
    await this.save();
    return project;
  }

  async createTask({ projectId, prompt }) {
    const project = this.state.projects.find((entry) => entry.id === projectId);
    if (!project) return null;
    const task = {
      id: randomUUID(),
      projectId,
      prompt,
      status: 'queued',
      createdAt: new Date().toISOString(),
    };
    this.state.tasks.push(task);
    project.updatedAt = task.createdAt;
    await this.save();
    return task;
  }
}
