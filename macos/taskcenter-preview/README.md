# CloudFile v0.8 本地 TaskCenter Preview（不依赖 v0.7 上传）

2026-10-12。**开发测试工具，不是发行安装包，也不是第二个 Chrome Native Host**。

## 已有独立能力

- `internal/taskcenter`：私有本地任务索引，按实例/仓库/远端路径/设备去重，恢复任务元数据；损坏记录不能静默丢失。不保存令牌或上传 Intent。
- `Monitor`：对已登记工作副本低频检查；本地文件稳定至少 3 秒后 SHA256 去重，返回 `local-saved` / `needs-review` 事件；不自动上传。
- `Preferences`：默认 `light`，预留 `badge`、`expand` 与浮标坐标。Go 索引采用私有文件，后续正式 UI 使用统一设置接口。
- `cmd/cloudfile-taskcenter`：`--list` JSON 摘要、`--watch` JSON Lines 变化提示。**仅本机命令行**；无 HTTP、无后台自动启动、无任何编辑/续租/提交 API。
- `macos/taskcenter-preview/CloudFileTaskCenterPreview.swift`：原生菜单栏入口、单一可拖动 48px 浮标、独立多文件任务窗口。窗口仅从本机 `--list` 读取状态，空任务不显示浮标；用户浮标位置由 Go 私有 Preferences 统一保存。已通过独立 Go --watch 只读事件连接保存轻提示（约 3.5 秒，不抢焦点），并通过当前用户预览锁避免重复实例；**已与 Go 的浮窗位置配置接口对接，提醒模式仍以轻提示为默认**。

## 开发环境验证

```bash
go test ./...
go vet ./...
go test -race ./internal/taskcenter ./cmd/cloudfile-taskcenter
GOOS=windows GOARCH=amd64 go build -o /tmp/taskcenter-win.exe ./cmd/cloudfile-taskcenter
GOOS=darwin GOARCH=arm64 go build -o /tmp/taskcenter-macos ./cmd/cloudfile-taskcenter

# 可选：仅在 macOS 开发机上编译只读 UI 预览
swiftc -swift-version 5 macos/taskcenter-preview/CloudFileTaskCenterPreview.swift -o /tmp/CloudFileTaskCenterPreview
# 启动需先编译本机 Go CLI，且任务索引中已有可信登记的测试任务
/tmp/CloudFileTaskCenterPreview --taskcenter-bin /absolute/path/to/cloudfile-taskcenter
```

UI 不会替用户创建/领取 Session，也不会修改云端文件。仅 UI 编译验收通过，未对已安装 Agent 作后台启动或系统通知实机回归。**不要将预览 UI 自动安装或宣称 v0.8 功能上线**；目前尚未做真实点击/浮窗跨屏幕手工验收。

## 后续依赖与集成点

1. v0.7 的**可信本地会话/工作副本**最终接入 `Store.Open`；不能允许任意浏览器扩展消息直接登记本地路径。
2. 常驻单主控从正式 Edit Journal 检索真正 `pending_intent` 与 Lease 状态；扫描结果仅辅助提示，不能充当服务器授权或发布依据。
3. macOS UI 与 Windows UI 统一使用 Go TaskCenter 摘要、持久提醒设置与轻提示事件；正式接入时收敛预览版 UserDefaults，不保留两套配置。
4. 独占续租/退出保护、Chrome P0 定位及真实操作依赖 v0.7 受控流程；独立开发的 UI 不能自行提交、重试或释放 Checkout。

**用户确认边界**：Windows 1.2.3、macOS 2.0.3；单悬浮入口+独立任务窗口；轻提示；Chrome P0 基础联动、P1 体验优化。

## v0.8 RC2 本机常驻预览

在当前用户的 macOS 图形会话下，运行 `scripts/install-taskcenter-macos-v08.sh install` 从源码构建 Go CLI 与 AppKit 预览并注册专用用户级 LaunchAgent。支持 `status`、`uninstall`。**不会修改 v0.7 RC2、Chrome Native Host、用户配对或上传事务**。安装前需要 Go/Swift 构建环境；此为开发预览，不是 Developer ID 签名/公证后的正式发行程序。

## macOS 普通用户绿色运行、无 Developer ID 安装包（v0.8 P0）

本版优先交付 **解压即运行的便携 .app**，不要求管理员账户、管理员密码、安装到 `/Applications`、`sudo`、`.pkg`、MDM、Developer ID 证书、Apple 公证或目标机器安装 Xcode/Swift/Go。开发机运行 `scripts/package-taskcenter-macos-v08.sh`，分别生成 Apple Silicon ARM64 与 Intel AMD64 两份 ZIP；包内 `CloudFileTaskCenter.app` 自带 AppKit GUI 和 Go TaskCenter CLI，并提供 `start-taskcenter.command`。

**默认操作**：把 ZIP 解压到自己有写权限的目录（如 Downloads/用户文件夹），双击 `CloudFileTaskCenter.app`。如果 Dock/Finder 无法正常唤起，可从 Terminal 在解压目录使用 `bash ./start-taskcenter.command`。菜单栏是主要入口，Dock 和独立窗口是后备入口；均不需要管理员权限。无需运行 `install-taskcenter-macos-v08.sh install`，那个命令用于开发机从源码构建和可选用户级 LaunchAgent，不能当作绿色运行前提。

**系统安全边界**：本绿色预览包没有 Developer ID 发行签名或公证；Apple Silicon 编译器可能添加本地 ad-hoc 签名，它不等于开发者身份签名。如果包是通过浏览器下载的，macOS Gatekeeper 可能阻止首次打开或要求用户在 Finder／“系统设置 → 隐私与安全性”中明确授权可信来源；这是操作系统行为，不能保证所有企业管控的设备都会允许未签名应用。不自动清除隔离属性、不全局关闭 Gatekeeper、不绕过 MDM 或软件白名单；拒绝后只报告限制，其他已获许可的 CloudFile 使用不应被阻断。

任务中心绿色包不包含 v0.7 的设备配对、原生 Chrome Host 和上传引擎；需要这些能力的使用者仍须沿用允许的 v0.7 Agent 环境。当前 macOS 绿色包仅完成源码编译和 ZIP 检查，**尚未以非管理员账户在真实目标系统验收首开安全提示、菜单栏、Dock、轻提示和持续续租**。
