# macOS Agent2

macOS 使用 Go 主控和 `com.cloudfile.agent2`，不安装 .NET、CNG Bridge 或 Windows Host。设备私钥由 Security.framework 在当前用户 Keychain 中生成，标记为不可导出；Keychain 锁定或钥匙不存在时拒绝签名，不创建替代钥匙。

## 安装与回退

在 macOS 上执行 `scripts/package-macos.sh`，分别生成 Apple Silicon 和 Intel ZIP。解压后运行 `Install.command`。安装无需管理员权限；程序采用按内容哈希区分的不可变路径，当前用户 Chrome 清单原子切换，原清单保留为回退点。回退执行包内 `register-agent2-macos.sh --rollback`，配置、工作副本和钥匙不删除。

开发包为 ad-hoc 签名，不能当作已公证的正式发行包。正式构建设置 `CLOUDFILE_CODESIGN_IDENTITY` 为 Developer ID；发行前另需 Apple 公证及受隔离下载的启动验收。安装程序不会移除 quarantine。

## 本地打开与配对

原有会话模式先执行 `cloudfile-agent2 --allow-origin https://YOUR-CLOUDFILE`。此设置只信任已有会话入口，不等于设备配对。

设备配对提供本地显式 CLI 握手：

1. `cloudfile-agent2 --pairing start --pairing-instance https://YOUR-CLOUDFILE`。返回本地 `key_alias` 和用于浏览器认证注册的 `enrollment.public_key`。
2. 浏览器完成服务端注册，返回 pending challenge；以 JSON 经 stdin 传给 `cloudfile-agent2 --pairing sign --pairing-key KEY_ALIAS`。返回 device_id、nonce、signature，用于浏览器确认接口。
3. 浏览器确认成功并取得 active 状态后，以 JSON 经 stdin 传给 `cloudfile-agent2 --pairing confirm --pairing-key KEY_ALIAS`。本地保存公有配对信息，不保存浏览器 Cookie、会话票据或私钥。

取消或清理过期的本地待配对记录可执行 `--pairing cancel --pairing-key KEY_ALIAS`；这不撤销服务端设备，服务端撤销仍在浏览器完成。

握手十分钟过期，签名挑战需在服务端的有效期内。实例已有配对时拒绝覆盖；确认成功仅表示完成本地登记，不代表服务端持续授权。每次设备操作仍须服务端验证当前权限及设备签名。

## 更新

macOS 拒绝无签名旧版更新清单。用 `--set-update-source https://.../release.json` 设置源，`--set-update-key /absolute/release-public.pem` 固定 P-256 公钥。密钥仅由本地配置指定，不能从网络清单获取。

发布格式：外层 `payload` 和 `signature` 均为标准 Base64；payload 是原始 UTF-8 JSON，signature 为 ECDSA P-256/SHA-256 的 64 字节 P1363 签名。payload 字段为 version、platform（darwin-arm64/darwin-amd64）、sha256、download_url、notes、expires_at（Unix 秒）、ipc_version=1、journal_version=1；可选 extension 包含 version、zip_url、sha256、notes。下载地址必须与更新源同 HTTPS authority。

更新保留旧程序，在 SHA-256、自检通过后原子切换真正的 Chrome 清单，不替换指向它的符号链接。后台任务使用独立 Unix session，浏览器退出不依赖 Native Messaging 管道持续存在。

## 实现边界

v0.7 2.0.1 RC 已新增 macOS **当前用户 Go Native Host 接收新版 open_uri**，严格识别 HTTPS 实例/规范化 URI、当前本机 Keychain 配对，原始 claim/read 通过独立的一次性 P-256 签名挑战；实际文件仅通过固定 /seafhttp/cloudfile/read 的 Header Bearer、无 Cookie/redirect 的受控下载得到。宿主机只将 URI 通过固定 --open-uri-stdin 私有管道传递，绝不把票据置于命令行、日志或 AppleScript 中。

用户明确同意打开后，macOS 系统原生弹框提供普通编辑“保存云端继续／保存云端完成／保留本地关闭／恢复原 Intent”，以及独占“Commit 继续／Commit 并 Checkin／Abandon／关闭保留 Checkout”。独占设备授权继续使用原有 Checkout Journal/Lease 交付，每四分钟尝试续租，操作前要求当前续租可确认；放弃必须服务端签名释放。所有结果不确定都保留本地工作副本和原始 Intent，不自动重发文件；用户关闭窗口只停止续租，不宣称隐式释放锁。

**当前是 RC 源码、跨架构编译、单测和静态 AppleScript 校验通过，不是已完成的正式发布。** 配对 GUI 仍为 CLI 明确握手；真实 Chrome→Keychain 当前设备→Hub/Seafile→Office/CAD→回传与异常恢复尚未验收。Developer ID 签名、Apple 公证及 Intel 实机测试也是外部验收门槛。

## 已执行验证

- macOS 原生 Go 全量测试与 vet；签名清单错误公钥、过期、架构/协议不匹配、越域下载拒绝。
- 更新保留浏览器清单符号链接，新程序具有执行权限；坏摘要、自检失败均不切换入口。
- 真实 Keychain 临时钥匙创建、重新打开、公钥验证签名及清理；合成浏览器响应驱动完整本地配对握手，错误 key thumbprint 被拒绝。此项不是服务端真实配对验收。
- 隔离 HOME 下运行安装后的原生程序，通过 Native Messaging 帧读取状态，验证重装/回退保留本地文件及外来注册拒绝。
- 后台进程在启动父进程退出后完成输入处理，处于独立进程组；票据未放入命令行。

真实浏览器/服务器端到端、Intel 实机、Developer ID 与 Apple 公证仍待外部环境验收。

## 离线分发与绿色运行约束

Windows 与 macOS 都只交付完整离线 ZIP；安装/注册脚本不得联网下载依赖，不使用管理员提权，不安装系统服务。Windows 的 .NET 已自包含，程序从解压目录运行，Chrome 注册仅在 HKCU。macOS 常规安装仅写当前用户目录，也可以运行包内 `Register-Portable.command`，将程序及版本目录留在解压位置；`Unregister-Portable.command` 只移除属于该目录的浏览器注册。

绿色运行不等于零本机状态：浏览器必须有当前用户 Native Host 注册，设备钥匙必须留在本机 Keychain/CNG，不能跟着 U 盘导出；配对、恢复 Journal 和工作副本留在当前用户私有目录。移动绿色目录后需要重新注册。不需要管理员权限，也不要求另一代 Agent 或系统运行库预先安装。
