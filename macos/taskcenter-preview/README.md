# CloudFile v0.8 本地 TaskCenter Preview（不依赖 v0.7 上传）

2026-10-12。**开发测试工具，不是发行安装包，也不是第二个 Chrome Native Host**。

## 已有独立能力

- `internal/taskcenter`：私有本地任务索引，按实例/仓库/远端路径/设备去重，恢复任务元数据；损坏记录不能静默丢失。不保存令牌或上传 Intent。
- `Monitor`：对已登记工作副本低频检查；本地文件稳定至少 3 秒后 SHA256 去重，返回 `local-saved` / `needs-review` 事件；不自动上传。
- `Preferences`：默认 `light`，预留 `badge`、`expand` 与浮标坐标。Go 索引采用私有文件，后续正式 UI 使用统一设置接口。
- `cmd/cloudfile-taskcenter`：`--list` JSON 摘要、`--watch` JSON Lines 变化提示。**仅本机命令行**；无 HTTP、无后台自动启动、无任何编辑/续租/提交 API。
- `macos/taskcenter-preview/CloudFileTaskCenterPreview.swift`：原生菜单栏入口、单一可拖动 48px 浮标、独立多文件任务窗口。窗口仅从本机 `--list` 读取状态，空任务不显示浮标；用户坐标暂由预览 UI 自己存放于 UserDefaults。已通过独立 Go --watch 只读事件连接保存轻提示（约 3.5 秒，不抢焦点），并通过当前用户预览锁避免重复实例；**已与 Go 的浮窗位置配置接口对接，提醒模式仍以轻提示为默认**。

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
