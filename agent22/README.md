# CloudFile Agent 2.2 · MVP

开发分支 `agent-rust`，全新 Rust + Tauri v2 实现。
Chrome Native Messaging 与原有 CloudFile Hub/Server 保留为协议边界。
历史参考基线：`e33d380f7e7ae65efdf39bcb0dec091e6d13b755`；不依赖 Go/.NET/Swift 运行时。

## 当前 MVP 实现

- Tauri 任务中心、托盘、服务配置、设备连接、本地打开与修改状态。
- Chrome `open_uri` 设备签名会话：认领、下载普通编辑工作副本。
- **本地保存后自动上传**（普通本地编辑）：文件内容稳定至少 3 秒后，Agent 捕获快照、签名授权并全量上传；无需手动选文件或网页上传。支持按任务暂停自动上传，也可点击“保存云端”立即提交。
- **独占编辑（Checkout 租约）与手动提交**：网页发起独占会话后，Agent 用当前配对设备签名领取并确认短时租约（`lease-*`），可续租；修改后由用户在任务中心点击“提交”保留 Checkout，或“签入并释放”结束本次独占。提交走独立的签名 `commit-*` 意图 + 原生条件发布，不复用普通上传接口。
- 发布确认后推进基线，可继续编辑再次保存。响应丢失先查询原 Intent；重试发送原快照，不替换成后来修改的内容。
- 密钥存储在 macOS Keychain / Windows Credential Manager；采用 Rust P-256 签名，不依赖旧 Agent。当前是系统保护的可读密钥，不宣称 CNG/硬件不可导出密钥。
- 独立用户目录 `CloudFileAgent22`、开发 Host `com.cloudfile.agent22.dev`。

本地应用 Ctrl/Cmd+S 保存到磁盘后，普通编辑由 Agent 自动观察工作副本并上传；独占编辑不会自动上传，必须由用户在任务中心显式提交。发生网络中断、冲突或未确认回执时暂停或保持待确认状态，保留工作副本与原上传快照；不会盲目自动重发。
当前单文件上限与现有服务端一致，为 128 MiB。
旧 `cloudfile-local/v2` 会话仍可本地打开，但不具备新设备上传授权；应从网页重新发起普通本地编辑。
CDC/大文件分块上传与自动更新后置。

服务端边界：`optimistic-edit` 需要 `CLOUDFILE_AGENT2_OPTIMISTIC_UPLOAD_ENABLED`，`exclusive-edit` 需要部署显式开启 Agent2 独占能力（`CLOUDFILE_EDITING_ENABLED` + Agent2 exclusive 开关）。接口未启用会显示失败并保留副本，不会降级为普通网页上传。

验证：macOS 编译与协议集成测试（含独占编辑租约、提交、签入、丢失回执恢复与冲突保护）；尚未完成运行中 Hub/Server + Chrome + Office 的端到端验收，也未实机验证 Windows。

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
4. Agent 在文件内容稳定后自动上传，也可在任务中心点击“保存云端”。确认发布后可继续编辑。
5. 独占编辑：从网页发起独占会话（服务端需开启 Agent2 独占能力），Agent 领取并确认 Checkout 租约后在任务中心显示“独占编辑”任务；点击“提交”保留 Checkout，点击“签入并释放”结束本次独占。提交结果未确认时先点“查询提交结果”，不会自动重发。

服务端必须已启用设备会话。普通编辑需要 `CLOUDFILE_AGENT2_OPTIMISTIC_UPLOAD_ENABLED` 并部署原生发布接口；独占编辑还需要部署显式开启 Agent2 独占能力（`CLOUDFILE_EDITING_ENABLED` 与 Agent2 exclusive 开关，默认关闭）。接口未启用会显示失败并保留副本，不会降级为普通网页上传。
原上传待确认时，“查询上传结果”只查询；“重试原上传”先查询，再在未发布时重发同一 Intent 的原快照。后来修改仍留在工作副本，下次保存再上传。独占编辑对应“查询提交结果”，语义相同，但只在新独立提交/签入时可再次发起，且签入成功后本地会话关闭，必须从网页重新发起。

```sh
python3 scripts/package.py
```

构建本机架构 ZIP，两个可执行文件必须放在同一目录。
打包脚本需要 Python 3.11 或更高版本。macOS 包含可双击的开发 `.app`，
注册 Host 时 `--bin-dir` 指向该应用的 `Contents/MacOS`；Windows 指向 ZIP 解压目录。
GitHub Actions 对 macOS / Windows 分别执行 Rust 与配对脚本测试并生成开发 ZIP；
构建通过不代替 Windows 实机和真实服务端验收。

## 代码边界

- `cloudfile-core`：会话、用户配置、任务记录、设备签名、提交快照与全量上传；`exclusive` 模块负责独占 Checkout 租约与 Commit/Checkin。
- `cloudfile-native-host`：有界 stdio 消息和本机请求转交。
- `cloudfile-desktop`：唯一任务执行进程与轻量静态界面，无 Node 运行时。

本机 IPC 使用用户数据目录下的短期文件邮箱及进程锁，无 localhost 服务。
请求成功处理后移除票据；进程中断后不会自动重放一次性票据。
本地副本不因重复打开或浏览器 overwrite 字段而覆盖。只有服务器确认发布才可显示云端保存成功。
独占编辑的 Checkout 令牌只写入所有者可读的任务 journal；签入成功或释放后立即删除，服务端在 Agent 确认 journal 落盘后才会擦除其密文副本。

参考：[Tauri v2](https://v2.tauri.app/)、[Chrome Native Messaging](https://developer.chrome.com/docs/extensions/develop/concepts/native-messaging)。
