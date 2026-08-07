# CloudFile Local Agent

一个仅在本机运行的 Node.js 服务，供 CloudFile Local Console Chrome 扩展管理受限目录内的项目和任务队列。

## 安装与启动

需要 Node.js 20 或更高版本。

```bash
export CLOUDFILE_AGENT_TOKEN='replace-with-a-long-random-token'
export CLOUDFILE_AGENT_ROOT='/absolute/path/you/want-to-share'
npm test
npm start
```

服务默认监听 `http://127.0.0.1:4317`。通过 `CLOUDFILE_AGENT_PORT` 和 `CLOUDFILE_AGENT_HOST` 可调整监听地址；`CLOUDFILE_AGENT_HOST` 应保持回环地址。

## 安全模型

- 每个 API 请求都必须带有相同的 Bearer 配对令牌。
- 项目目录必须已经存在，并且只能位于 `CLOUDFILE_AGENT_ROOT` 内。
- 状态以原子方式保存到 `data/state.json`，文件权限为 `0600`。

与 Chrome 扩展配对时，在扩展设置页填入 `http://127.0.0.1:4317` 和相同令牌即可。
