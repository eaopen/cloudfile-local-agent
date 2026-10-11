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

### 下载目录（workspace_root）

镜像按用途分成两个互不重叠的子树：

```
{root}/view/{repo_id}/{库内路径}     本地查看：可随时重新下载的只读副本（下载后置只读）
{root}/edit/{repo_id}/{库内路径}     本地编辑：用户的工作副本，只有显式选择才会被覆盖
{root}/.cloudfile-meta/...           每个文件的下载指纹，供冲突判定使用
```

查看与编辑刻意分开放，这样「本地查看」永远不会覆盖「本地编辑」正在改的那份文件；
`repo_id` 仍固定在用途之下一层，所以改库名/移动库不会改变本地路径，每个子树也能整体
清理或整体迁移。

`root` 按以下顺序解析：

1. 配置里的 `workspace_root`（显式指定时始终优先，可用于逐机固定位置）；
2. Windows：`D:\CloudFileLocal\sessions`；
3. 回落：系统盘用户缓存目录，Windows 为 `%LocalAppData%\CloudFileLocal\sessions`。

Windows 上第 2 步要求 `D:` 是**固定本地磁盘**（光驱、可移动盘、映射网络盘一律不算），
且**实际可创建目录并写入文件**。任一条件不满足就静默回落到第 3 步，因此办公机上常见的
「D: 是 DVD 光驱」「D: 只读或已写满」都会自动走系统盘，不会中途下载失败。判定每次调用
都会重做，所以 D: 后来变得不可用时会降级到系统盘，而不是报错。

解析结果变化时，旧根上的整棵镜像（两个子树与指纹一起）会**整体移动**到新根，只在目标根
尚无镜像内容时执行，且中途失败不删除源目录。若跑的是更早版本留下的「扁平」布局
（`{root}/{repo_id}/{路径}`，无用途层），首次运行会按文件的只读位一次性分派：只读的归入
`view/`，可写的归入 `edit/`，判不准的一律归入 `edit/`（少一次重新下载，好过丢掉本地改动）。
显式配置了 `workspace_root` 时不搬迁，镜像就固定在那里。macOS/Linux 不做盘符偏好，始终
使用用户缓存目录。

### 本地编辑的冲突判定

「本地编辑」遇到本地已有副本时，Agent 会同时给出两个相互独立的事实：

- **网盘是否变过** —— 比对 `file_id`（Seafile obj_id，内容寻址，内容一变必变）；
- **本地是否被改过** —— 现场重算内容 SHA-256，与下载时记下的 `baseline_digest` 比对。

据此自动化解多数情况：网盘没变就直接复用本地；网盘变了但本地一字未动就直接换成网盘版本
（没有本地改动可丢）。只有「网盘变了**且**本地改过」，或缺指纹、无法确认本地是否改过时，
才询问用户「覆盖本地」还是「使用本地」。

指纹落在 `{root}/.cloudfile-meta/` 而不是浏览器存储里，所以清浏览器缓存、换浏览器、把镜像
移到别的盘，都不会让判定失效。

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

## v0.7 块级增量上传底座（2026-10-11 开发中，未接线/未发布）

`internal/blockdelta/` 新增 **纯 Go Seafile 兼容 CDC**（6–10 MiB、平均 8 MiB，48 字节 Rabin 窗口）：生成有序块 ID、块大小/偏移、Seafile v1 file ID 和 SHA-256；只读取已冻结单文件快照。与独立 Seafile C 参考数据的指纹、切块边界、块 SHA-1、元数据 File ID 和内容 SHA-256 已逐项对齐。

`StageSnapshotOnly` 根据可信服务端返回的缺失块列表，仅上传缺失 raw-blocks；拒绝未知/重复 ID、源内容变化及上传失败。HTTPS adapter 要求已配对同源的短时 `upload-raw-blks-api` URL、TLS 验证和严格响应，不允许重定向；在测试 TLS 服务中验证了分块发送。**本模块只暂存原始块，绝不会发布云端版本、取消独占或绕过 Server Barrier。**

**尚未完成**：CloudFile 设备 Broker 对接真实 scoped 缺失块授权、原生受控 `CommitBlocks`（块完整性、guard、intent、base version 与同一原生事务回执）、大文件 DEV 端到端实测以及 Windows 真实绿色包测试。本模块未由旧 CLI/Native Host/普通保存行为触发，不能宣称增量上传现已面向用户可用。

**明确不包含文件增量下载**：Agent/Agent2 本地查看与本地打开继续受控完整下载，不做下载块缓存、版本块重建或下载差异比较。大文件增量上传也只能由用户明确选择提交，不能后台静默提交。

独立检查：`go test -count=1 ./internal/blockdelta`；全部 Go 用例：`go test ./...`。

## v0.7 本地独占文件人工提交（2026-10-11 开发中）

`internal/editing/manual.go` 提供 `ManualSubmitter`：只接受当前用户**明确确认**的一次独占编辑提交；读取已经领取的 Checkout/guard 状态，创建不可变快照（当前完整文件模式上限 128 MiB），沿用已有 `Workflow.CommitCaptured` 把 `intent_id` 先持久化，再走 `Transport.Publish`。普通 Commit 继续保留占用；明确选择完成时可提交并 Checkin。结果未知时保留唯一工作副本/快照及原 intent，必须明确选择恢复；不使用普通 Seafile `update-api` 降级。

**为未来自动上传预留**：业务提交执行层与触发来源分离，未来可以在重新确认授权模型后，由可选保存检测触发同一 Workflow；本阶段只允许 `SubmitManual` 且 `UserConfirmed=true`。不能把“本地检测到保存”当作当前许可；不自动提交或自动 Checkin。

**未完成的产品接线**：CloudFile Hub `editing/v1/commit-file` 仍要求浏览器 OIDC/CSRF 会话，设备签名 Broker 只支持当前本地领取/读取操作，尚不提供受信写入票据。Windows .NET Native Host / Go CLI 尚未把人工提交按钮及已验证设备授权绑定到 `ManualSubmitter`。**不要把浏览器 Cookie 交给 Agent，不要把此库测试声称为真实远端产品上传成功。** 独占编辑仍默认关闭。原生受控块清单 `CommitBlocks` 也尚未接线。

修复 1.0.1 最新 `dev` 源码缺失的 `internal/home`、`internal/workspace`、`internal/launch`、更新模块 API 及 `config` 平台根目录适配，使整个 Go CLI 恢复编译；这些修复需继续在受控 Windows 真实用户环境验证安装/扩展更新和版本切换。

## Agent2 2.0.1 双平台开发线与 Host 隔离（2026-10-11）

- **共用 `dev`**：Agent2 Go 2.0.1 同时构建 macOS arm64 与 Windows amd64；平台差异通过 Go build tags/平台适配处理，不再拆成 Windows/Mac 两条开发分支。
- **macOS 主程序**：Go Agent2 2.0.1 Native Host `com.cloudfile.agent2`。使用 `scripts/register-agent2-macos.sh` 在当前用户 Chrome Profile 注册，Go 直接负责现有本地查看/编辑等可用能力。独占编辑还需设备写授权和用户操作入口，不能提前声明。
- **Windows 过渡**：现有 .NET Agent1 1.x（Host `com.cloudfile.current_agent`）仍为主 UI/本地打开/用户密钥能力；Go Agent2 2.0.1 作为**并存的独立能力组件**（`com.cloudfile.agent2`）逐步承载 Checkout/Commit 等新增业务。当前只有 Go 内部提交执行器、暂存模块，浏览器**不发布未接线的独占能力**。日后 Go 主控、.NET 仅作薄原生能力适配，无需新开发分支。
- **旧 Go 1.x**：`com.cloudfile.local_agent` 仅供兼容老会话。本版 Go2 的配置存放 `CloudFileAgent2` 用户目录，与旧 Go 1.x 的 `CloudFileLocal` 分离，避免工作副本和升级源互相覆盖。
- **浏览器插件 1.0.1**：同一插件依次探测 Agent2 和兼容 Host，**以能力而不是版本号或平台猜测**决定路由：Agent2 已正式声明的能力优先；`open_uri` 在 Go2 尚未实现时仍交给 Windows Agent1；`open_session` 和本地工作区查询可回退原 Go 1.x。Agent2 的未知/失败请求**绝不再自动切换旧 Host 重试**，避免重复消费票据。未发布 Checkout/Commit 不会伪装成受支持的 Native 消息。
- **发行**：Go2 和 Agent1 可并装，但只通过各自 Host ID 启动；Windows 现有 Agent1 的 HKCU/注册表和安装文件不被 Agent2 安装脚本覆盖。Mac/Windows Go2 的新增注册脚本无需 admin；真实受控 Windows 策略、公司签名、升级/回退与 Go+.NET 独占写回 E2E 仍需验证。**Go2 2.0.1 为 DEV 源码版本，不代表正式部署。**

## Windows 1.2.1 / macOS 2.0.1：Device Commit Broker 接线进展

对外发布口径：原 Windows Agent1 `agent1` 分支维护 **1.0.0–1.1.x**；新 Windows `dev` 从 **1.2.1** 起（.NET UI/CNG + **内部 Go Core 2.0.1**）；macOS Go `dev` 从 **2.0.1** 起，未来 Windows Go 主控后再升 2.x 对齐。当前 Go 可执行文件均为 Go Core 2.0.1，不等于 Windows 整体对外版本为 2.x。

`internal/editing/device_commit_proof.go` 复用 CloudFile 服务端 Canonical P-256 Proof；`device_bridge_windows.go` 调用 Windows Agent `.NET Bridge 1.2.1` 既有 CNG 密钥；`device_commit_broker.go` 经同源 HTTPS 发起 `commit-challenge`、签名并执行 `commit-authorize`，只接收持久 `prepared` 意图；`device_bound_transport.go` 通过统一 `Workflow` 把 Broker PREPARE 与未来 Native Publisher 分离，**缺 Native Publisher 则拒绝创建完整提交 transport**。已有测试覆盖 HTTP 提交、签名绑定、错误挑战、未知回包、禁止无权限上报。

**待开发**：设备证明被 Server Native Branch 认可后的原子 `commit-file-device` 发布与 `published` 持久回执，以及真实 Agent2/Windows 1.2.1 UI 与 OS 设备授权完整接线；macOS P-256 Keychain 真实保存/配对尚未做。当前绝不由 Go 自动 fallback 到普通 Seafile `update-api`，不启用自动上传和文件增量下载。Hub `CLOUDFILE_AGENT2_EXCLUSIVE_ENABLED=false`，暂不对用户宣传受控提交已可用。

### Device Broker 测试补强（2026-10-11）

`internal/editing/device_manual_recovery_test.go` 从真实 TLS mock challenge/authorize 一直覆盖到本地人工快照、Journal、模拟原生发布后丢回包和查询原 `intent_id` 恢复；不重复发布、不动用户后来修改的工作副本。Go `device_commit_proof_test.go` 使用 Hub/Python 独立得出的 SHA256 golden，和 Windows .NET 的 `DeviceProofTests` 校验完全相同的 Challenge bytes。三方签名格式一致，不代表服务器已提供设备 Native Publisher；上线前必须真实 E2E。
