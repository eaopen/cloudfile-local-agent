# CloudFile Agent 2.2 · MVP

开发分支 `agent-rust`，全新 Rust + Tauri v2 实现。
Chrome Native Messaging 与原有 CloudFile Hub/Server 保留为协议边界。
历史参考基线：`e33d380f7e7ae65efdf39bcb0dec091e6d13b755`；不依赖 Go/.NET/Swift 运行时。

## 当前 MVP 实现

- Tauri 任务中心、托盘、可拖动悬浮入口、非抢焦点轻提示、服务配置、设备连接、本地打开与修改状态。
- **任务中心三态展示**：待处理 / 进行中 / 已完成。分组与计数由桌面进程派生，界面只渲染，不自行判断任务是否结清。任务详情按编辑模式给对应操作，普通编辑与独占编辑共用一套界面。
- **可拖动悬浮入口**：拖动交给系统原生手势（跨显示器与缩放比例都不会偏移），位置按显示器分别保存；无任务时按用户设置隐藏；有任务待处理时显示计数徽标。拖动落点由桌面进程在窗口停止移动后写入设置。
- **退出保护**：托盘“退出”与窗口关闭都不会在存在未确认 Intent 或有效独占 Checkout 时静默结束进程。先给出具体风险清单，用户确认后才退出；退出只停止后台续租，不释放 Checkout、不删除本地工作副本与待确认 Intent。
- Chrome `open_uri` 设备签名会话：认领、下载普通编辑工作副本。
- **本地保存后自动上传**（普通本地编辑）：文件内容稳定至少 3 秒后，Agent 捕获快照、签名授权并全量上传；无需手动选文件或网页上传。支持按任务暂停自动上传，也可点击“保存云端”立即提交。
- **独占编辑（Checkout 租约）与手动提交**：网页发起独占会话后，Agent 用当前配对设备签名领取并确认短时租约（`lease-*`），可续租；修改后由用户在任务中心点击“提交”保留 Checkout，或“签入并释放”结束本次独占。提交走独立的签名 `commit-*` 意图 + 原生条件发布，不复用普通上传接口。
- 发布确认后推进基线，可继续编辑再次保存。响应丢失先查询原 Intent；重试发送原快照，不替换成后来修改的内容。
- **大文件 CDC 分块上传（可选，默认关闭）**：≥16 MiB 的快照先按 Seafile 原生 CDC（Rabin，6/8/10 MiB）切分，用服务端已存在的原生块做签名 missing-block 计划；只有确实节省 ≥4 MiB 且复用 ≥25% 时才改走 `*-block-plan` / `*-block-stage` / `*-block-publish`，否则仍用已验证的全量传输。独占与普通本地编辑两条路径共用同一套计划与回退逻辑。
- **本机安装、升级与回退均由自带程序完成**：`cloudfile-native-host` 同时是 CLI，注册 Host、登录项、下载校验、版本目录切换、回退与清理都不需要目标机器安装 Python 或 Rust 工具链。
- 密钥存储在 macOS Keychain / Windows Credential Manager；采用 Rust P-256 签名，不依赖旧 Agent。当前是系统保护的可读密钥，不宣称 CNG/硬件不可导出密钥。
- 独立用户目录 `CloudFileAgent22`、开发 Host `com.cloudfile.agent22.dev`。

本地应用 Ctrl/Cmd+S 保存到磁盘后，普通编辑由 Agent 自动观察工作副本并上传；独占编辑不会自动上传，必须由用户在任务中心显式提交。发生网络中断、冲突或未确认回执时暂停或保持待确认状态，保留工作副本与原上传快照；不会盲目自动重发。
当前单文件上限与现有服务端一致，为 128 MiB。
旧 `cloudfile-local/v2` 会话仍可本地打开，但不具备新设备上传授权；应从网页重新发起普通本地编辑。

### 安装、升级与回退

注册、登录项、升级、回退与清理统一走 `cloudfile-native-host` 的命令行，不使用任何脚本运行时：

```sh
cloudfile-native-host --register-host --extension-id YOUR_EXTENSION_ID   # 注册当前用户 Host
cloudfile-native-host --host-state                                       # 查看当前注册与所属
cloudfile-native-host --rollback-host                                    # 还原上一次被替换的注册
cloudfile-native-host --unregister-host                                  # 只移除本 Agent 写入的注册
cloudfile-native-host --enable-autostart / --disable-autostart           # 可选登录启动
cloudfile-native-host --check-update                                     # 报告可用的已签名版本
cloudfile-native-host --update                                           # 下载、校验、暂存、健康检查、切换
cloudfile-native-host --rollback                                         # 指回上一个版本目录
cloudfile-native-host --prune                                            # 只保留当前版本 + 一个回退点
cloudfile-native-host --status                                            # 版本、注册、运行时、应用检测、退出风险
```

规则：

- 被其他程序注册的 Host **不会**被静默替换。需要显式加 `--takeover`，且原 Manifest 一定先备份为 `<host>.previous.json`，可用 `--rollback-host` 还原。任务中心里对应的接管需要用户确认。
- 升级只信任签名清单：校验签名、SHA-256、平台与协议版本后才暂存到新的版本目录，不覆盖正在运行的程序；暂存程序必须能启动、`--version` 与 `--component` 都通过才允许切换。健康检查失败会删除暂存目录，当前版本保持不变。
- 升级前会检查活动任务。存在有效 Checkout 或未确认 Intent 时默认拒绝升级，因为升级会替换承担续租与发布的进程；用户可在任务中心看到风险清单后明确选择继续。失败时保留用户数据，不会把未确认任务当作已完成。
- Windows 需要 WebView2 运行时，`--status` 会报告 `runtime.webview2`；macOS 使用系统 WebKit（该字段为 `null`，表示不适用而非缺失）。不捆绑离线 WebView2，以保持包体积。
- 当前替代目标是 **A 类：功能完全替代** —— 首次切换允许重新配对设备，但不丢失任何工作副本。原设备身份与原任务的无感原位迁移（B 类）尚未实现。

### 大文件 CDC 分块上传

客户端开关 `CLOUDFILE_AGENT2_CDC_ENABLED=1`，需与服务端 `CLOUDFILE_AGENT2_CDC_ENABLED` 同时开启。边界与顺序：

- 计划、暂存、发布每一步都单独向 `commit-upload-authorize`（或 `optimistic-upload-authorize`）申请一次性的 60 秒票据；不跨步复用，票据只在 header 里传。
- 暂存原始块不改变任何文件：只有原生 Branch 内的守卫事务才写文件路径，并在其中重新校验账号、设备、租约、基线与回执。
- 回退是安全的：计划请求失败或计划显示不划算时，尚未发布任何内容，因此回落到全量传输仍使用同一个已准备 Intent。一旦发布请求已发出，就不再回退——回执丢失时保留工作副本与原快照，先“查询提交结果”。
- 单文件上限 128 MiB、单块上限 10 MiB、单 Intent 最多 64 块，与 `cf_device_block_plan` 一致。

服务端边界：`optimistic-edit` 需要 `CLOUDFILE_AGENT2_OPTIMISTIC_UPLOAD_ENABLED`，`exclusive-edit` 需要部署显式开启 Agent2 独占能力（`CLOUDFILE_EDITING_ENABLED` + Agent2 exclusive 开关）。分块传输另需两侧同时开启 `CLOUDFILE_AGENT2_CDC_ENABLED`。接口未启用会显示失败并保留副本，不会降级为普通网页上传；分块传输未启用时会回落到全量传输。

验证：macOS 编译、静态检查与协议集成测试。CDC 分块切分与参考 Agent 的字节输出做交叉校验（32 MiB 镜像的块 ID、尺寸、SHA-256 与 File ID 完全一致），并在 mock Hub 上覆盖计划、只上传缺失块、发布、不划算回落、传输未启用回落与暂存被拒保留原 Intent。尚未完成运行中 Hub/Server + Chrome + Office 的端到端验收，也未实机验证 Windows，真实服务端分块上传仍待联调。

## 运行

在本目录执行（工具链由 `rust-toolchain.toml` 固定）：

```sh
cargo test --workspace --locked
cargo build --workspace --locked
cargo run -p cloudfile-desktop --locked
```

桌面程序接受三个自检参数，都不需要窗口或私有目录：`--version`、`--component`（本地组件契约 JSON）、`--help`。
登录项会用 `--autostart` 启动它：有托盘时保持常驻不打开窗口，避免每次登录抢焦点。

先在任务中心添加完整服务地址，例如 `https://files.example.com` 或 `https://files.example.com/seafile/`。部署路径由本机配置保存，设备签名仍绑定根 Origin。连接设备前会检查服务端编辑能力和配对接口；未启用时直接显示原因，不创建配对密钥。
Windows 需已安装 WebView2 Runtime；Chrome 本身不提供此运行时，`cloudfile-native-host --status` 会如实报告是否检测到。macOS 使用系统 WebKit。
当前 ZIP 是开发包，不含签名、公证或运行时安装器。

## Chrome 开发联调

```sh
python3 scripts/prepare-extension.py
```

（`prepare-extension.py` 是构建期工具，只用于生成开发扩展副本；目标机器不需要 Python。）

使用独立的 Chrome 配置文件（开发副本沿用现有扩展 ID，不能与正式扩展装在同一配置文件）。在扩展管理页以开发者模式加载 `dist/chrome-extension-agent22`，复制扩展 ID 后执行：

```sh
cargo build -p cloudfile-native-host --locked
./target/debug/cloudfile-native-host --register-host --extension-id YOUR_EXTENSION_ID
```

也可以直接在任务中心的“Chrome 开发注册与升级”里填扩展 ID 注册，不必用命令行。
注册只影响当前用户的开发 Host，不替换旧 Host；若该 Host 已被其他程序注册，会先报错，
由用户确认后才备份并接管。

开发扩展是共享扩展的隔离副本；连接步骤：

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

构建本机架构 ZIP，两个可执行文件必须放在同一目录。ZIP 内不再附带任何注册脚本：
注册、登录项、升级与回退都由 `cloudfile-native-host` 自己完成，解压即可运行，
目标机器不需要 Python 或 Rust 工具链。
打包脚本本身仍需 Python 3.11 或更高版本（仅构建期）。macOS 包含可双击的开发 `.app`。
GitHub Actions 对 macOS / Windows 分别执行 Rust 与配对脚本测试并生成开发 ZIP；
构建通过不代替 Windows 实机和真实服务端验收。

## 代码边界

- `cloudfile-core`：会话、用户配置、任务记录、设备签名、提交快照与全量上传；`exclusive` 模块负责独占 Checkout 租约与 Commit/Checkin，`cdc` 模块负责大文件分块切分与计划/暂存/发布链路，`guard` 模块是“能否安全退出 / 升级”的唯一判定来源，`settings` 负责本机偏好与悬浮位置，`apps` 负责应用检测与自定义打开规则，`hostreg` / `update` / `autostart` 负责注册、版本目录与登录项。
- `cloudfile-native-host`：有界 stdio 消息、本机请求转交，以及安装/升级/回退用的命令行。
- `cloudfile-desktop`：唯一任务执行进程与轻量静态界面（`ui/` 下四组静态资源：任务中心、悬浮入口、轻提示、样式），无 Node 运行时。

本机 IPC 使用用户数据目录下的短期文件邮箱及进程锁，无 localhost 服务。
请求成功处理后移除票据；进程中断后不会自动重放一次性票据。
本地副本不因重复打开或浏览器 overwrite 字段而覆盖。只有服务器确认发布才可显示云端保存成功。
独占编辑的 Checkout 令牌只写入所有者可读的任务 journal；签入成功或释放后立即删除，服务端在 Agent 确认 journal 落盘后才会擦除其密文副本。
分块上传的原始块字节来自本任务冻结快照的同一内存镜像，切分与暂存不存在时间差；暂存只写内容寻址的块，不改变文件路径或 Checkout 状态。
`guard::assess` 是退出、托盘状态、浏览器退出查询与升级前置检查共用的唯一判定；任何一处都不允许自行放宽“可以安全退出”的定义。

## 验证状态

已覆盖：`cargo test --workspace --locked`（91 项，含 1 项需要外部条件的 `#[ignore]`）、`cargo clippy --workspace --all-targets --locked -- -D warnings`、`cargo fmt --all --check`。
其中 `cloudfile-native-host/tests/cli_contract.rs` 与 `cloudfile-desktop/tests/cli_contract.rs` 直接运行真实二进制，并把 `HOME`（Windows 另加 `LOCALAPPDATA`）重定向到临时目录，因此注册、接管、回退、卸载、健康检查与协议自检都不触碰开发机上的真实 Chrome 注册。

尚未验收：

- **Windows 实机**。应用检测、注册表读写、WebView2 探测、登录项 `Run` 键与 Tauri 窗口行为都只在本机 macOS 上编译通过，没有 Windows 真机回归。
- **真实服务端联调**。自动上传、独占提交与 CDC 仍只对着 mock Hub 验证过。
- **Chrome 扩展联动**。`--status` / `capabilities` 的字段已对齐，但没有在真实 Chrome 里跑过完整链路。
- **扩展自身的更新**。浏览器的 `update_extension` 请求目前返回明确的“未实现”，不会假装成功；扩展需要手动重新加载开发副本。
- **无感原位升级（B 类）**。不迁移旧设备身份与原任务，首次切换需重新配对。
- **Code signing / 公证 / 运行时安装器**。当前 ZIP 是开发包。

参考：[Tauri v2](https://v2.tauri.app/)、[Chrome Native Messaging](https://developer.chrome.com/docs/extensions/develop/concepts/native-messaging)。
