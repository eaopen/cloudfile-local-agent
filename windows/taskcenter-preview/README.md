# CloudFile v0.8 Windows TaskCenter preview

**独立研发预览，不是正式 Windows Agent 安装包。**

## 当前实现：Go（2026-10-14 起）

同目录的 `Program.cs` / `.csproj` 是**最初的 WinForms 参考实现**，保留下来对照行为；
**正式投递的是 Go 版**：`cmd/cloudfile-taskcenter-ui`（纯标准库 + 直接 syscall 调
user32/shell32/gdi32/comctl32，零第三方依赖）。下面的 C# WinForms 版只作为参考实现保留。

**怎么被装上、怎么起来的**（2026-10-14 起）：`scripts/register-agent2-windows.ps1` 会从同一个
测试通道取 `taskcenter-update.json`，把 `cloudfile-taskcenter.exe` 与
`cloudfile-taskcenter-ui.exe` 装到 `<安装根>\bin\`，写一条 HKCU Run 自启并立刻启动 UI。
**启动即进托盘、不弹窗口**；首次运行弹一条「CloudFile 已在托盘运行」说明气泡（标记文件
`%AppData%\CloudFileAgent2\ui-notify-shown`，删掉会再弹一次）。不想装托盘用 `-SkipTaskCenter`。

本地打开文件的提示：Agent 往 `%AppData%\CloudFileAgent2\ui-notify.jsonl` 追加事件，
UI 每 3 秒读增量并弹气泡。UI 启动时**跳过历史**，所以攒了一晚的记录不会在开机时炸成一串。

它和 WinForms 版行为对齐：

| 能力 | 说明 |
| --- | --- |
| 系统托盘 | `Shell_NotifyIcon` + 右键菜单（打开任务中心 / 刷新任务 / 退出预览）、左键单击打开窗口 |
| 任务窗口 | 660×380（最小 490×300）居中、任务栏可见；`SysListView32` 四列（文件 / 本地状态 / 模式 / 库·路径）；底部状态栏 +「管理选中文件」按钮 |
| 可拖动浮标 | 48×48 置顶无边框；无任务时隐藏；拖动吸到当前显示器工作区左右边；位置经 Go `--get-preferences` / `--set-position` 读写，与 macOS 共用一份设置 |
| 3 秒刷新 | 轮询 Go `cloudfile-taskcenter.exe --list` |
| 保存轻提示 | 常驻 `--watch` 子进程，`local-saved` / `needs-review` / `checkout-*` 触发气泡；独占异常在列表里标 `⚠ 独占续租未确认` |
| focus 心跳 | 每 3 秒 `--poll-focus`，同时也是「有没有桌面监听者」的心跳 |
| 单实例 | 每个登录会话一个（互斥体 `Local\CloudFileTaskCenterPreviewV08`） |
| 退出确认 | 退出前重新 `--list`；仍有独占/待查任务时先确认（不释放 Checkout、不删本地文件） |

**它与 WinForms 版一样，把 Go CLI 当作任务状态的唯一权威**（子进程拿 JSON），
所以它不是第二个 Native Host，也不注册任何东西。

### 构建

```powershell
# 需要同目录的 cloudfile-taskcenter.exe（Go CLI）
go build -o dist\cloudfile-taskcenter.exe .\cmd\cloudfile-taskcenter
go build -ldflags "-s -w -H=windowsgui" -o dist\cloudfile-taskcenter-ui.exe .\cmd\cloudfile-taskcenter-ui
# UI 默认取「自己旁边」的 cloudfile-taskcenter.exe，也可以显式指定
dist\cloudfile-taskcenter-ui.exe --taskcenter-bin C:\Tools\cloudfile-taskcenter.exe
```

`-H=windowsgui` 只是为了不弹控制台窗口；不加也能跑（会带一个控制台）。

### 自动化自检（`--self-test`）

GUI 没法在 CI 里用眼睛验，所以留了一个冒烟入口：建窗口/托盘 → 往列表控件写两行再回读
→ 真跑一次 `--list` 并解析 JSON → 报错或用退出码说明结果 → 自己关掉。

```powershell
dist\cloudfile-taskcenter-ui.exe --self-test
# self-test 列表写入      OK
# self-test CLI --list    OK（本地任务 0 项，活动独占 0）
# self-test 窗口         OK
# ... 浮标 OK / 托盘 OK → exit 0
```

退出码：`0` 正常；`2` 启动前置失败（找不到 CLI / 注册窗口类失败）；`3` 自检项失败。
**托盘创建失败不算失败**（企业策略可能禁止），只在状态栏提示「系统托盘不可用；可继续使用
任务栏和普通窗口。」，窗口照常显示。

**仍未验收**：真机上的拖动/吸边手感、跨屏与高 DPI、托盘在受限策略下的实际表现、
以及浅色/深色主题下的观感 —— 这些只能人工看。`--self-test` 只能证明「建得起来、数据通得过」。

---

以下为最初的 WinForms 参考实现的说明，行为契约仍然有效。


本项目只有 WinForms UI：单个用户会话内互斥锁、系统托盘、可拖动吸边浮标、独立多文件任务窗口，以及调用 Go 本机只读任务入口的 3 秒刷新和保存轻提示。浮标在无任务时隐藏，浏览器关闭时仍可保留任务显示。预览窗口不注册/取代 `com.cloudfile.current_agent`，没有用户授权、Checkout、上传、Checkin 或续租功能。

## 构建

在 Windows 安装 .NET 8 SDK 后：

```powershell
dotnet build CloudFile.TaskCenter.Preview.csproj -c Release
# 使用同平台编译的 cloudfile-taskcenter.exe 本机只读 CLI
dotnet run --project CloudFile.TaskCenter.Preview.csproj -- --taskcenter-bin C:\Tools\cloudfile-taskcenter.exe
```

在具有 Docker 的 macOS/Linux 开发机可使用 `mcr.microsoft.com/dotnet/sdk:8.0` 交叉编译，只验证 .NET 语法与 Windows targeting，不替代 Windows GUI 实机测试。

## 已实现与限制

- 使用 Go `--list` 的有限摘要显示文件、状态和库/路径；`--watch` 事件只触发本地气泡提示；无 Web 访问和文件上传。
- 单个 WinForms 预览实例（per-session mutex），同一任务只展示一次；多个文件独立排列。
- 浮标位置现已统一通过 Go `--get-preferences` / `--set-position` 读取和保存；不再维护单独的 GUI 浮窗配置文件。
- 通知默认轻提示，不抢焦点，异常留在任务列表；系统气泡具体可见行为受 Windows 通知设置影响。
- 尚未做 Windows 真机拖动、托盘、焦点、跨屏 DPI、退出/重启、打包安装或升级验收；**不能标记 Windows 1.2.3 已交付**。
- Go TaskCenter 还未从真实 v0.7 已验证编辑会话自动登记任务。运行预览需要已有可信任务索引；不会从浏览器随意接收文件路径。

研发目标：先验证桌面任务体验，不影响 Windows v0.7 的单 Native Host 与原版 1.2.1 用户流程。

## 当前用户级常驻预览

在 Windows 使用 `scripts/install-taskcenter-windows-v08.ps1 -Action Install -PackageDirectory <包含两个 exe 的目录>` 注册 HKCU Run 启动项；`-Action Status` 查看状态，`-Action Uninstall` 删除预览注册和预览二进制，不删除任何本地编辑副本、任务索引或 v0.7 Journal。需要先将 WinForms 项目发布为无需 SDK 的 Windows 可执行文件，并准备同架构 Go CLI。此脚本仅供开发预览，不替代正式签名安装包。


## 无管理员/无安装权限（v0.8 P0 硬约束）

解压绿色包后，直接双击同目录 `start-taskcenter.cmd`，或者运行 `CloudFile.TaskCenter.Preview.exe --taskcenter-bin <同目录 cloudfile-taskcenter.exe>`。无需 UAC、MSI、PowerShell、系统服务、HKLM、系统目录写入、Windows 启动项，也无需目标机安装 .NET Runtime/SDK（绿色包已自包含）。

- 系统托盘 `NotifyIcon` 默认启用、是主要入口；创建成功后启动窗口默认最小化，**任务栏图标仍存在作为第二入口**。若托盘/通知被企业策略禁止或初始化失败，直接显示普通 WinForms 窗口。关闭普通窗口只最小化到任务栏，不让用户失去所有入口。
- 开机自启默认为**关闭**。可选 `optional-user-autostart.ps1` 的 `-EnableAutoStart` 尝试 HKCU Run；若被权限/策略拒绝，仅警告，不阻断本地运行或复制到当前用户可写目录。
- 如果 PowerShell 执行策略阻止脚本，直接运行 `start-taskcenter.cmd` 即可。使用可写目录（如 Downloads/用户文件夹），不用安装向导。
- “无需安装”不等于能绕过企业 AppLocker/WDAC、执行白名单、杀毒、Chrome 扩展或 Native Host 的组织策略：这些策略若禁止程序/扩展运行，不能保证浏览器一键打开；应报告明确限制，不能自动要求管理员提权。
- 绿色任务中心只管理当前用户的本地已验证任务。云端编辑、上传仍需相应已有的受信 Agent/设备身份及 v0.7 正式业务链；不能把解压任务中心等同于已安装完整 CloudFile Agent。
- 任何自启、托盘、通知、更新、Native Host 注册失败都不得删除工作副本或 Intent，也不得虚报独占续租成功。

**Windows 实机验收仍待完成**：标准用户、无安装权限、HKCU Run 被拒绝、托盘不可用、PowerShell 脚本禁止、离线双击绿色包、手动启动/退出、保存提醒/冲突及数据恢复。
