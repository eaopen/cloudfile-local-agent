# CloudFile v0.8 Windows TaskCenter preview

**独立研发预览，不是正式 Windows Agent 1.2.3 安装包。**

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
- 浮标坐标暂存 Windows 当前用户 `LocalAppData/CloudFile/taskcenter-preview/`。**正式版本应迁移到 Go 统一 Preferences，不得保留两套配置**。
- 通知默认轻提示，不抢焦点，异常留在任务列表；系统气泡具体可见行为受 Windows 通知设置影响。
- 尚未做 Windows 真机拖动、托盘、焦点、跨屏 DPI、退出/重启、打包安装或升级验收；**不能标记 Windows 1.2.3 已交付**。
- Go TaskCenter 还未从真实 v0.7 已验证编辑会话自动登记任务。运行预览需要已有可信任务索引；不会从浏览器随意接收文件路径。

研发目标：先验证桌面任务体验，不影响 Windows v0.7 的单 Native Host 与原版 1.2.1 用户流程。
