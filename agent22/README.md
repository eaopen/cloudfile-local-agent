# CloudFile Agent 2.2 · MVP

开发分支 `agent-rust`，全新 Rust + Tauri v2 实现。
Chrome Native Messaging 与原有 CloudFile Hub/Server 保留为协议边界。
历史参考基线：`e33d380f7e7ae65efdf39bcb0dec091e6d13b755`；不依赖 Go/.NET/Swift 运行时。

## 当前 MVP 实现

- Tauri 任务中心、托盘、服务配置、设备连接、本地打开与修改状态。
- Chrome `open_uri` 设备签名会话：认领、下载普通编辑工作副本。
- **保存云端**：Agent 自动定位工作副本、捕获快照、签名授权并全量上传；无需手动选文件或网页上传。
- 发布确认后推进基线，可继续编辑再次保存。响应丢失先查询原 Intent；重试发送原快照，不替换成后来修改的内容。
- 密钥存储在 macOS Keychain / Windows Credential Manager；采用 Rust P-256 签名，不依赖旧 Agent。当前是系统保护的可读密钥，不宣称 CNG/硬件不可导出密钥。
- 独立用户目录 `CloudFileAgent22`、开发 Host `com.cloudfile.agent22.dev`。

本地应用 Ctrl/Cmd+S 只保存到磁盘；点击任务中心“保存云端”后由 Agent 上传。
当前普通编辑单文件上限与现有服务端一致，为 128 MiB。
旧 `cloudfile-local/v2` 会话仍可本地打开，但不具备新设备上传授权；应从网页重新发起普通本地编辑。
独占编辑、CDC、自动上传、复杂恢复与自动更新后置。

验证：macOS 编译与协议集成测试；尚未完成运行中 Hub/Server + Chrome + Office 的端到端验收，也未实机验证 Windows。

## 运行

在本目录执行（工具链由 `rust-toolchain.toml` 固定）：

```sh
cargo test --locked
cargo build --workspace --locked
cargo run -p cloudfile-desktop --locked
```

先在任务中心添加完整服务地址，例如 `https://files.example.com` 或 `https://files.example.com/seafile/`。部署路径由本机配置保存，设备签名仍绑定根 Origin。连接设备前会检查服务端编辑能力和配对接口；未启用时直接显示原因，不创建配对密钥。
Windows 需已安装 WebView2 Runtime；Chrome 本身不提供此运行时。
macOS 使用系统 WebKit。当前 ZIP 是开发包，不含签名、公证或运行时安装器。

## Chrome 开发联调

```sh
python3 scripts/prepare-extension.py
```

使用独立的 Chrome 配置文件（开发副本沿用现有扩展 ID，不能与正式扩展装在同一配置文件）。在扩展管理页以开发者模式加载 `dist/chrome-extension-agent22`，复制扩展 ID 后执行：

```sh
python3 scripts/register-host.py --extension-id YOUR_EXTENSION_ID --bin-dir target/debug
```

该脚本只注册当前用户的开发 Host，不替换旧 Host。开发扩展是共享扩展的隔离副本；
连接步骤：

1. 在任务中心添加服务并点击“连接设备”。
2. 在开发 Chrome 配置文件登录同一 CloudFile 服务，点击扩展中的“连接 Agent 2.2”。配对窗口有效期 10 分钟，浏览器登录 Cookie 不会交给 Agent。
3. 从网页发起“普通本地编辑”（设备会话 `cloudfile-open://v1/open`），在本地应用保存修改。
4. 在任务中心点击“保存云端”。无需选择文件；确认发布后可继续编辑。

服务端必须已启用设备会话和 `CLOUDFILE_AGENT2_OPTIMISTIC_UPLOAD_ENABLED`，并部署原生发布接口。接口未启用会显示失败并保留副本，不会降级为普通网页上传。
原上传待确认时，“查询上传结果”只查询；“重试原上传”先查询，再在未发布时重发同一 Intent 的原快照。后来修改仍留在工作副本，下次保存再上传。

```sh
python3 scripts/package.py
```

构建本机架构 ZIP，两个可执行文件必须放在同一目录。

## 代码边界

- `cloudfile-core`：会话、用户配置、任务记录、设备签名、提交快照与全量上传。
- `cloudfile-native-host`：有界 stdio 消息和本机请求转交。
- `cloudfile-desktop`：唯一任务执行进程与轻量静态界面，无 Node 运行时。

本机 IPC 使用用户数据目录下的短期文件邮箱及进程锁，无 localhost 服务。
请求成功处理后移除票据；进程中断后不会自动重放一次性票据。
本地副本不因重复打开或浏览器 overwrite 字段而覆盖。只有服务器确认发布才可显示云端保存成功。

参考：[Tauri v2](https://v2.tauri.app/)、[Chrome Native Messaging](https://developer.chrome.com/docs/extensions/develop/concepts/native-messaging)。
