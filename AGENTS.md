# STOP: Rust Agent2 was migrated (2026-10-20)

This instruction is for the **legacy `agent-rust` branch** of `eaopen/cloudfile-local-agent`. It is an archival reference, **not the development branch** for Rust CloudFile Agent2.

## Required behavior for any AI coding agent

1. **Do not modify, fix, refactor, release, or commit any Rust Agent2 code in this branch**, including `agent22/`, its Tauri UI, tests, installers, release scripts, and CI package pipeline.
2. For all new Rust Agent2 work, **switch to `https://github.com/eaopen/cloudfile-local-agent2`**. Base feature branches on that repository's `dev`, and open PRs there, never to `eaopen/cloudfile-local-agent@agent-rust`.
3. Existing newer V2 Guard editing, local view / normal edit / exclusive edit, session state, and workspace contracts are maintained in the **new repository**. Do not blindly copy the old Rust directory across or overwrite the new code.
4. [Migration PR #7](https://github.com/eaopen/cloudfile-local-agent2/pull/7) contains the work porting later old-branch changes; **inspect its current merge status**. A PR is not equivalent to merged `dev`.
5. This old branch may still receive narrowly scoped **archival notices and documentation fixes**. This exception does not permit new Rust behavior changes. Do not attempt to disable the migration guard workflow.

**If a task explicitly names the old `agent-rust` source path, treat it as background reference and apply the requested Rust code changes in the new repo only.**

---

# 旧仓 Rust Agent2 停止开发说明

- **禁止在 `eaopen/cloudfile-local-agent` 的 `agent-rust` 分支继续开发、修复、提交或发布 Rust Agent2。**
- **唯一后续开发仓库：[https://github.com/eaopen/cloudfile-local-agent2](https://github.com/eaopen/cloudfile-local-agent2)，以 `dev` 为集成主线。**
- 旧目录 `agent22/` 只作历史参考；从旧仓查阅资料不等于在旧仓提交修改。
- 旧分支改动移植情况见 [新仓 PR #7](https://github.com/eaopen/cloudfile-local-agent2/pull/7)；未核对 PR 状态时不得声称已合入 `dev`。
- 只允许维护明显的迁移标识/历史文档，不允许借此继续添加 Rust 功能。
