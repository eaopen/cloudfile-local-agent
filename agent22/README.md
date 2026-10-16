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
# 空白机器：先跑引导体（它只做定源 + 验签 + 下载 native host，然后交给下面那条命令）
cloudfile-install --source https://host/dir --source-fallback https://ip/dir --extension-id <id>

cloudfile-native-host --install --source https://host/dir --source-fallback https://ip/dir \
    --public-key <b64> --extension-id <id>                               # 一键安装/修复
cloudfile-native-host --set-update-source https://host/dir --public-key <b64>  # 只配置更新通道
cloudfile-native-host --clear-update-source                              # 清空更新通道
cloudfile-native-host --check-update                                     # 报告可用的已签名版本
cloudfile-native-host --update                                           # 下载、校验、暂存、健康检查、切换
cloudfile-native-host --update-extension                                 # 装最新扩展包，再由扩展自己重载
cloudfile-native-host --rollback                                         # 指回上一个版本目录
cloudfile-native-host --prune                                            # 只保留当前版本 + 一个回退点
cloudfile-native-host --status                                            # 版本、注册、运行时、应用检测、退出风险
cloudfile-native-host --self-test                                        # 本机自检（JSON；有问题时退出码非 0）
cloudfile-desktop --self-test                                            # 同一套检查，从桌面进程那一侧看
```

空白机器上什么都没有，而 `--install` 需要一个**已存在的**二进制来跑，所以另有一个引导体
`cloudfile-install`（对应 Go 侧 `install.cmd` 的角色）：

- 它**不带任何负载**：定源（同样的主备探测）→ 取**已签名**的清单并用**内置的钉住公钥**验签 →
  下载 `cloudfile-native-host`（校验清单里的 sha256）→ 把参数原样转给它 `--install`。
  注册、暂存、健康检查、激活、自启、启动全部仍由 native host 完成 —— 会改机器状态的动作只有一份实现。
- 与 `install.cmd` 的本质区别：`install.cmd` 从通道下载注册脚本并**无签名**地执行（只查 sha256）；
  这里必须先验签才下载任何字节，而且**公钥在打包时注入**（`CLOUDFILE_INSTALL_PUBLIC_KEY`），
  绝不在运行时从通道取 —— 否则「谁签发」就变成「通道说了算」。没注入又没给 `--public-key` 时它**拒绝干活**。
- `--dry-run` 只定源并报告计划、不改机器：现场排查「这台机器能不能连上通道」用。
- 双击启动（无参数 + 终端）时结尾会等回车，自动化里不会挂住；`--no-pause` 可显式关掉。

一键安装（`--install`）等价于 Go 的 `install.cmd`，但目标机器**不需要任何脚本运行时**：

- **主备地址**：`--source` 是主源（域名），`--source-fallback` 是备用源（通常是直连静态服务的 IP）。依次探测，**把实际可用的那个写回配置** —— 不然用户装完、第一次升级时才发现主源在这台机器上根本不通。主源可用时备源一次都不会被探测（省掉一次超时等待）；两个都不通时报错里带着每个地址**各自的原因**，只报一句「安装失败」在现场没有用。空地址会被跳过，主备写成同一个地址只探一次。
- **顺序**：先判安装目录可写（别下完几 MB 才发现装不进去）→ 定源并写回 → 有更新才换二进制 → 注册 Host → 设开机自启 → 启动任务中心。
- **幂等**：已经装好的机器重跑一遍等于**修复** —— 注册与自启会重做，二进制不动（清单不比当前新就不换）。
- 自启失败与启动失败都**不判安装失败**（各只警告一句）：它们和注册是两件事，谁挂了都不该拖累另一个，二进制已经就位、用户也能自己双击。
- 可跳过：`--skip-registration` / `--skip-autostart` / `--skip-launch`（现场分步排查、或只想升级二进制时用）。
- **装到哪**：`CLOUDFILE_AGENT2_HOME` 覆盖 Agent 的整棵树（配置、任务、版本），与 Go 的 `install.cmd` / `internal/home` 一致；引导体的 `--install-dir` 是同一件事的显式写法。相对路径一律拒绝。
- **全新安装与升级走同一条路**：先暂存（健康检查通过才落进版本目录）、再按「有没有旧注册」决定接管还是新建注册。`update::activate` 是「接管既有注册」专用的，所以注册与激活在 `--install` 里是分开的两步 —— 否则空白机器上会因为「没有旧注册可接管」而失败。

更新通道的信任策略：

- 正式通道要求**规范 https origin**（`https://host/dir`），且 Agent 自身的升级必须有钉住的签名公钥；包只能从同一 origin 下载，不跟随重定向。
- 局域网开发通道是**明文 http**，必须用 `--allow-insecure-source`（或 config.json 的 `allow_insecure_update_source`）显式打开。它**只放宽 scheme 一项**：规范 origin、同源、sha256、签名、不跟随重定向一条都不放松，而且开关本身会被记进 config.json —— 升级时读回来的是「当初同意的那套规则」，不会出现「设置时宽松、升级时严格」。写成 http 的源**永远不会**被自动接受。
- 开发通道允许「有源、没有签名公钥」：那种通道只服务**扩展更新**（扩展不签名，信任锚是同源 + sha256），此时 `--check-update` / `--status` 会明说「只有扩展更新可用，Agent 自身不可升级」。正式通道仍然要求两条都在。
- 开没开这个口子是**可审计的**：`--status` / `--check-update` 用 `update_source_insecure_allowed` 报告它，`--self-test` 会写成「已显式放行明文 http」。

规则：

- 被其他程序注册的 Host **不会**被静默替换。需要显式加 `--takeover`，且原 Manifest 一定先备份为 `<host>.previous.json`，可用 `--rollback-host` 还原。任务中心里对应的接管需要用户确认。
- 升级只信任签名清单：校验签名、SHA-256、平台与协议版本后才暂存到新的版本目录，不覆盖正在运行的程序；暂存程序必须能启动、`--version` 与 `--component` 都通过才允许切换。健康检查失败会删除暂存目录，当前版本保持不变。
- 升级前会检查活动任务。存在有效 Checkout 或未确认 Intent 时默认拒绝升级，因为升级会替换承担续租与发布的进程；用户可在任务中心看到风险清单后明确选择继续。失败时保留用户数据，不会把未确认任务当作已完成。
- Windows 需要 WebView2 运行时，`--status` 会报告 `runtime.webview2`；macOS 使用系统 WebKit（该字段为 `null`，表示不适用而非缺失）。不捆绑离线 WebView2，以保持包体积。
- 当前替代目标是 **A 类：功能完全替代** —— 首次切换允许重新配对设备，但不丢失任何工作副本。原设备身份与原任务的无感原位迁移（B 类）尚未实现。

### 发版（Rust 通道）

Rust 与 Go 共用一台静态服务、同一个通道目录，但**产物与清单都不通用**：Go 的更新只校验 sha256，Rust 必须验签（`update::verify` 是硬策略，不会降级接受）。所以两条链路各自发各自的：

| | 清单 | 产物 | 信任锚 |
|---|---|---|---|
| Go Agent2 | `update.json` | `cloudfile-local-agent-<v>-dev.exe` | sha256 |
| **Rust Agent22** | **`cloudfile-agent2-update.json`** | **`cloudfile-native-host-<v>.exe` / `cloudfile-desktop-<v>.exe`** | **钉住的 P-256 公钥** |
| 扩展（两边共用） | `extension-update.json` | `cloudfile-local-session-receiver-<v>-dev.zip` | 同源 + sha256 |

版本号**在编译期注入**，不改仓库里任何文件（等价于 Go 的 `-ldflags -X`）：

```sh
CLOUDFILE_AGENT2_VERSION=2.2.4 cargo build -p cloudfile-native-host --release --locked
```

`cloudfile_core::VERSION` 优先取这个环境变量，没设才回落到工作区 `Cargo.toml` 的版本（所以开发时手跑 `cargo build` 得到的仍是开发版本）。之所以不采用「发版脚本改 Cargo.toml」：升级链路上有三处把这个字符串当事实比较（清单的 `version`、`update::probe` 拿到的 `--version`、`stage_release` 里「报告版本必须等于清单版本」），三者必须同源，而改文件的方式一旦中途失败就会留下「构建出来的版本 ≠ 签发的版本」的中间态。

通道侧的签名器与发版脚本在另一个仓库（`cloudfile-release/tools/dev-release/`）：

```sh
python sign-agent22-release.py keygen dev-keys/agent22-dev.pem dev-keys/agent22-dev.pub.json
python sign-agent22-release.py pubkey dev-keys/agent22-dev.pem          # 导出钉住的公钥
powershell -ExecutionPolicy Bypass -File make-agent22-release.ps1 \
    -Version 2.2.4 -ReleaseKey dev-keys/agent22-dev.pem
```

`make-agent22-release.ps1` 会构建两个产物（内置版本号）、逐个跑 `--version` 核对版本、写清单、签名、回验，并做**端到端自检**：起本地通道，用一个版本更低的探针二进制跑真实的 `--check-update`，确认签名被真实 Rust 验签器接受 —— 这是唯一能证明「Python 签名器与 `update.rs` 没漂移」的检验。

跨界面的契约只有一条：`signing_payload` 的字节形状。`update.rs` 里有一条 `the_signing_payload_is_a_fixed_byte_string` 逐字节钉住它；**Rust 侧改了字段顺序或分隔符，那条测试会红**，届时必须同步改签名器，否则通道上签出的清单会让所有已装 Agent 报「签名与钉住的公钥不符」。

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

- `cloudfile-core`：会话、用户配置、任务记录、设备签名、提交快照与全量上传；`exclusive` 模块负责独占 Checkout 租约与 Commit/Checkin，`cdc` 模块负责大文件分块切分与计划/暂存/发布链路，`guard` 模块是“能否安全退出 / 升级”的唯一判定来源，`settings` 负责本机偏好与悬浮位置，`workspace` 负责工作副本根目录策略与一次性搬迁，`apps` 负责应用检测与自定义打开规则，`hostreg` / `update` / `autostart` 负责注册、版本目录与登录项。
- `cloudfile-core` 的三个 Windows 底层保障模块：`winreg`（进程内读写注册表）、`childproc`（子进程不弹控制台窗口）、`shortcut`（「启动」文件夹里的 `.lnk`）。注册、登录项、WebView2 探测、应用探测**一律走它们**，不再 spawn `reg.exe`——否则父进程是 GUI（Tauri 桌面）时每次调用都会闪一个黑窗口，而且在禁用 `reg.exe` 的机器上会直接失败而不是降级。只有「打开文档的应用」与「必须脱离宿主存活的升级进程」例外，前者需要窗口、后者用 `DETACHED_PROCESS` 自带无控制台。
- 读前加固只有一处判据：`fsaccess`（对应 Go 的 `internal/fsaccess`）。任务目录、任务记录、偏好文件在**读**之前都要求「私有的普通文件/目录」，语义是 **fail closed**（与 Go 的 store 层一致）：一份权限被改松的状态文件不能被当成可信输入去授权退出、上传或续租。列表类接口不会把它报成「没有任务」——那会让人以为工作副本丢了。**Windows 上这条判据是刻意的 no-op**：`Metadata` 只反映只读属性，套 POSIX 位会把每一个状态路径都判死；真实访问由 NTFS ACL 管，而 ACL 不通过 `Metadata` 暴露。所以 Windows 上的加固是「绝对路径 + 非重解析点 + 普通文件 + 体积上限」，不含权限私有。
- Windows 的登录项有**两条途径**，装的时候依次尝试：先写当前用户的 `Run` 键，被组策略或安全软件锁住时（企业机器上常见）退到「启动」文件夹里的 `CloudFileAgent22.lnk`。两条都用 `IShellLinkW`/`IPersistFile` 由 shell 自己维护格式，不手写二进制。`--status` 与设置界面会报出实际生效的是哪一条（`run-key` / `startup-folder`），因为这两种情况在任务管理器里长得完全不一样。两条途径都带上 `--autostart`：登录启动必须进托盘而不是抢焦点。
- `cloudfile-native-host`：有界 stdio 消息、本机请求转交，以及安装/升级/回退用的命令行。
- **模式（mode）是子树，不是字符串**：本地副本只有三个子树 —— 查看（`local-view`，只读）、可编辑、独占可编辑；`local-edit` 与 `optimistic-edit` 是**可编辑子树的两个名字**。浏览器请求与任务都在这层比较，所以「缺省/`local-edit`」永远不会命中只读的查看副本（Go 的 `Request.MirrorMode()` 同义，原文理由：编辑子树是唯一不会被「查看」打开覆盖的那个）。反过来才是会伤人的：查看副本是只读的，被当成编辑副本交给用户，用户改完存不进去。可上传/可提交另有一道**模式白名单**（`journal::capture` 的 `editable task required`、`upload`/`exclusive` 各自校验），所以查看副本拿不到 Intent。
- 本机自检（`selftest` 模块）：把「这台机器上装的东西到底能不能工作」变成一次可脚本化的问答（`--self-test`，两个入口共用同一套检查）。十二项各自独立跑、独立报，输出固定形状的 JSON，有项确实不工作时退出码非 0。**刻意不创建窗口、不发网络请求**：建窗口依赖窗口站/桌面会话，在服务、SSH、CI 上必然不稳，自检本身变成「时好时坏」就没有价值；网络请求要等超时，会把「10 秒出结论」拖成「30 秒也不一定」。更新源只报「配没配、够不够用」，内容对不对由 `--check-update` 回答。
- 浏览器扩展的自更新（`extension` 模块）：与 Agent 清单**同级、同一个源**的 `extension-update.json`（`version` / `zip_url` / `sha256` / `notes`），下载后校验 sha256、解包到临时目录、核对包内 `manifest.json` 的版本与清单一致，再整体改名换过去。归档按不可信输入处理：绝对路径、`..`、反斜杠（Windows 上的 zip slip）、符号链接、非普通文件、超过 256 MiB 一律拒绝，且只写新建文件。替换前上一版会改名到 `<dir>.prior`，换不过去就放回原位；`<dir>.prior` 已存在时拒绝动作，等人工看一眼。装完由扩展自己 `chrome.runtime.reload()` 生效——Chrome 不允许外部进程替换已加载的扩展，所以这一步结束时是「文件已就位」，不是「已生效」。
- `cloudfile-desktop`：唯一任务执行进程与轻量静态界面（`ui/` 下四组静态资源：任务中心、悬浮入口、轻提示、样式），无 Node 运行时。
- `cloudfile-install`：引导体，只给空白机器用（定源 → 验签 → 下载 native host → 转交 `--install`）。它自己不带负载，见上一节。

本机 IPC 使用用户数据目录下的短期文件邮箱及进程锁，无 localhost 服务。
请求成功处理后移除票据；进程中断后不会自动重放一次性票据。
本地副本不因重复打开或浏览器 overwrite 字段而覆盖。只有服务器确认发布才可显示云端保存成功。
独占编辑的 Checkout 令牌只写入所有者可读的任务 journal；签入成功或释放后立即删除，服务端在 Agent 确认 journal 落盘后才会擦除其密文副本。
分块上传的原始块字节来自本任务冻结快照的同一内存镜像，切分与暂存不存在时间差；暂存只写内容寻址的块，不改变文件路径或 Checkout 状态。
`guard::assess` 是退出、托盘状态、浏览器退出查询与升级前置检查共用的唯一判定；任何一处都不允许自行放宽“可以安全退出”的定义。

## 验证状态

已覆盖：`cargo test --workspace --locked`（**156 项**在本机 Windows 11 实测全绿，含 2 项需要外部条件的 `#[ignore]`：一条往系统凭据库写临时凭据，一条往真实 shell 写一个临时 `.lnk`；另有 4 项 `#[cfg(unix)]` 用例只在 CI 的 macOS 上跑——权限判据在 Windows 上是刻意的 no-op，所以它的 POSIX 行为只能在那里验）、`cargo clippy --workspace --all-targets --locked -- -D warnings`、`cargo fmt --all --check`。
引导体与 `--install` 各有确定性用例：引导体那边起回环 HTTP 服务验「主源可用则备源一次不探、`--dry-run` 不建目录也不下载、缺公钥或公钥非法时**停在下载之前**、裸开关连同取值一起转发」；`--install` 那边验「主备选择、失败时列出每个地址的原因、可用的地址写回配置」。`CLOUDFILE_AGENT2_HOME` 的覆盖（含相对路径被拒）也有用例。

安装器（`--install`）的主备源行为有确定性用例：测试里起一个回环 HTTP 服务当主源、把一个没人监听的端口当备源，断言「只探测了主源、备源一次没碰、可用的地址被写回 config.json、退出码 0」；另两条覆盖「没给地址就拒绝且不写任何配置」与「两个都不通时报出两个地址」。对真实开发通道跑 `--install` 验证了**失败路径**：主源（域名）被网络拦截、备源返回 404（通道上还没有 Rust 约定的清单），报告如实列出两条原因与各自地址。

扩展自更新**整条链路**已对着真实开发通道实测：`--set-update-source http://10.9.8.162:6111/cloudfile-updates-dev --allow-insecure-source` → `--update-extension` 从明文 http 下载真实发布的包（摘要与清单一致）、解出 10 个条目（含 `icons/` 子目录）、装成 1.0.4；`--self-test` 随后如实报出「已显式放行明文 http」与「1.0.4」，不带开关时同一命令被拒（`must be a canonical https origin`）。
Windows 侧的注册表与登录项测试（`hostreg` / `autostart` / `update` 的注册表步骤、`cli_contract` 的注册与接管）**不再依赖 `reg.exe`**：注册表是进程内读写的，所以这些用例在禁用 `reg.exe` 的环境里也能跑、也会绿。
其中 `cloudfile-native-host/tests/cli_contract.rs` 与 `cloudfile-desktop/tests/cli_contract.rs` 直接运行真实二进制，并把 `HOME`（Windows 另加 `LOCALAPPDATA`）重定向到临时目录，因此注册、接管、回退、卸载、健康检查与协议自检都不触碰开发机上的真实 Chrome 注册。

尚未验收：

- **Windows 实机**。注册表读写、WebView2 探测与登录项的两条途径（`Run` 键写入 → 读回 → 撤销，「启动」文件夹 `.lnk` 写入 → 读回 → 撤销，含 shell 路径与文档化公式一致）已在 Windows 11 上实测；**Tauri 窗口与托盘行为、应用探测的真机回归仍未做**。
- **真实服务端联调**。自动上传、独占提交与 CDC 仍只对着 mock Hub 验证过。
- **浏览器扩展联动的完整链路**。`--status` / `capabilities` 的字段已对齐，但**没有在真实 Chrome 里跑过**：安装、加载、`chrome.runtime.reload()` 之后的实际行为都还没验证。扩展自更新的**文件侧**已用开发通道里真实发布的包验证过（摘要与清单一致、9 个条目含 `icons/` 子目录全部解出、版本核对、重复安装幂等），**下载侧（`--update-extension` 走网络）未验证**：见下面那条关于 http 通道的说明。
- **Rust Agent 自身的更新（一键安装的“换二进制”那一步）**。通道侧必须先提供符合 Rust 约定的清单：源写成 origin（`https://host/dir`），清单固定叫 `cloudfile-agent2-update.json`，并**用 Rust 的私钥签名**。现有开发通道里只有给 Go 侧用的 `update.json`（名字、位置、信任模型都不同），所以 `--install` / `--update` / `cloudfile-install` 走到下载那一步必然 404。这不是 Rust 侧的缺陷，而是**通道侧待补**（扩展更新不受影响，已经实测通过）。补齐后，`--install` 与引导体的端到端才算真的走完。
- **引导体的公钥注入与发布**。`cloudfile-install` 的公钥是**打包时注入**的（`CLOUDFILE_INSTALL_PUBLIC_KEY`），默认没有 —— 这是刻意的：没有钉住的公钥就不该开始安装。发版流程需要：① 用 Rust 的私钥签一份发布清单；② 注入公钥构建 `cloudfile-install`；③ 把两者一起发到通道。`make-dev-release` 目前只处理 Go 侧的产物，Rust 通道的发布脚本还没写。
- **无感原位升级（B 类）**。不迁移旧设备身份与原任务，首次切换需重新配对。
- **Code signing / 公证 / 运行时安装器**。当前 ZIP 是开发包。

参考：[Tauri v2](https://v2.tauri.app/)、[Chrome Native Messaging](https://developer.chrome.com/docs/extensions/develop/concepts/native-messaging)。
