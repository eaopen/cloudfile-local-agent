# CloudFile Local Agent

> 用途：说明 CloudFile 本地查看/编辑 Native Messaging Host 的构建、配置和安全边界
> 适用版本：Seafile CE 14 扩展版；Agent 协议 `cloudfile-local/v2`
> 当前状态：验证中；代码与单元测试存在，跨平台签名发布包和全链路验收待完成

一个 Go 绿色 Native Messaging Host，用于领取 CloudFile 的短时会话、在隔离工作区下载文件，并交给本机已安装的软件查看或编辑。

本 Agent 属于 CloudFile 的新应用扩展，不是 Seafile CE 或 Authentik 的组成部分。整体能力
状态、依赖和限制见[扩展能力矩阵](../cloudfile-docker/docs/feature-matrix.md)。

## 安装与启动

需要 Go 1.24 以上才能从源码构建；发布包是无运行时依赖的单文件二进制。

```bash
go test ./...
go build -trimpath -ldflags="-s -w" -o dist/cloudfile-local-agent ./cmd/cloudfile-local-agent
./dist/cloudfile-local-agent --allow-origin https://cloudfile.example
```

Windows 绿色包用 `scripts/register-windows.ps1` 写入当前用户的 Native Messaging Host 注册表项；Agent 是无需安装运行时的单文件程序，不需要管理员权限，也不开放 localhost HTTP 服务。将包放在当前用户可写的固定目录即可，不必写入 Program Files 或注册系统服务。

## 本地查看与编辑规则

**首次使用只需信任 CloudFile origin，不必配置应用路径。** Agent 按以下顺序打开文件：

1. 用户自定义的 `open_rules`；
2. 自动检测到的已安装软件；
3. 操作系统默认文件关联。

自动检测覆盖 Microsoft Word/Excel/PowerPoint/Visio、LibreOffice、AutoCAD、BricsCAD、
DraftSight、Revit、SOLIDWORKS、Creo、NX、CATIA、SketchUp、Rhino 和 FreeCAD 的常见 Office、
CAD/三维格式。Windows 优先检查用户/机器注册的 `App Paths`，再检查常见安装目录与可执行文件；
不读取浏览器数据，也不从服务端接收软件路径。

只有需要指定特定专业软件或覆盖默认顺序时才加入 `open_rules`。规则保存于当前用户的 Agent 配置文件：Windows 为
`%AppData%\\CloudFileLocal\\config.json`，macOS/Linux 使用系统用户配置目录下的
`CloudFileLocal/config.json`。先用安装脚本或 `--allow-origin` 创建信任 origin，再按需要
加入 `open_rules`；`--validate-config` 可在不启动会话时检查配置。

```json
{
  "allowed_origins": ["https://cloudfile.example"],
  "workspace_root": "D:\\CloudFileLocal\\sessions",
  "open_rules": [
    {
      "id": "cad-viewer",
      "modes": ["local-view", "local-edit"],
      "extensions": ["dwg", "dxf", "step"],
      "command": ["C:\\Program Files\\CAD\\cad.exe", "{file}"]
    },
    {
      "id": "view-fallback",
      "modes": ["local-view"],
      "extensions": ["*"],
      "command": ["C:\\Program Files\\Viewer\\viewer.exe", "{file}"]
    }
  ]
}
```

规则按顺序匹配，具体扩展名应位于通配规则之前。`command[0]` 必须是本机绝对路径，且整个
命令只能有一个 `{file}` 占位符；Agent 使用进程参数直接启动，不经过 shell。远端
`.cloudfile` 会话从不携带程序路径、命令行或规则。

## 安全模型

- 仅接收 `cloudfile-local/v2` 会话文件；会话文件只包含短期单次领取票据。
- 每个 CloudFile origin 都须由用户/安装脚本显式加入本地信任列表。
- Native Host manifest 精确指定正式扩展 ID；不使用通配 origin 或浏览器 Cookie。
- 本地编辑只保存到用户工作区，由用户在网页中手动上传；Agent 不接收 write-back capability，不监听文件变更，也不自动上传。`local-edit-exclusive` 暂不支持。
- Agent 只接受常规且不超过 1 MiB 的会话文件；下载内容超过 4 GiB 会中止并清理。

Chrome 扩展只把下载完成的 `.cloudfile` 文件路径转交给 Native Host；真正的内容能力由 Agent 直接向 CloudFile Hub 领取。

## 单文件人工签入内核（尚未开放）

`internal/editing` 提供人工 Commit/无内容 Checkin、带来源身份的稳定快照及持久意图恢复。Commit 保持签出，Checkin 确认回执后关闭并清除本地凭据。结果未知时查询原 intent，不重复提交，也不覆盖或删除唯一工作副本。

`SessionHTTPTransport` 已适配原生 OIDC 的 Checkout、人工上传、状态查询、受控下载、心跳、Resume 和 Abandon；它要求受信已认证的 host client。设备授权 broker 尚未接线，旧 v2 runner 不注册这些能力，Claim 仍拒绝 writeback，不传递浏览器 Cookie。

本地 HTTPS 测试覆盖 multipart、响应断连后的历史查询、不重复提交、无内容 Checkin 和工作副本保留；测试服务的身份/发布为夹具。Seafile 原生服务闭环已另行验收，不能代替浏览器 OIDC → Agent 产品联调。Go 测试、go vet、race 与 Windows amd64 交叉编译通过；Office/DWG/NX 实机保存行为未验收。自动上传、多文件工程及未验收的软件路径均不开放。
