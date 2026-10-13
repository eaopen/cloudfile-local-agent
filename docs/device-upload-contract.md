# 设备独占上传（开发门控）

Go 客户端、Hub 设备上传入口和 Native Branch 发布校验已经接线。产品能力仍默认
关闭，真实部署与完整桌面交互验收前不能宣称完整可用。

## 上传与恢复

1. 用户手动 Commit/Checkin，捕获不可变快照并先持久化原 Intent。
2. commit-challenge → commit-authorize 完成设备签名 Prepare；不表示保存成功。
3. 校验快照、复制固定字节，再使用新挑战调用 commit-upload-authorize。
4. 返回 60 秒内开始上传的加密凭据，绑定设备、会话、Intent、摘要、大小及操作。
5. 原始字节只发送到同源固定入口
   `/api/v2.1/cloudfile/extensions/local-edit/v1/agent/commit-upload/`；
   精确 Content-Length，Authorization Bearer，Idempotency-Key 为原 Intent。
   无 Cookie、无本地路径、无重定向。空文件使用明确的零长度请求。
6. 原生事务验证当前设备、会话、权限、Checkout、基线和实际内容；原子提交
   Branch、published 回执与会话/guard 基线。Checkin 同事务释放 guard。
7. 上传失败保留原快照和 Intent。commit-status-challenge → commit-status
   用新设备签名查询自己会话的历史回执，支持已经完成 Checkin 的会话。
   只有匹配原 Intent/基线/摘要/动作的 published 能显示成功。

Hub 复用现有 httptemp 和 publish_edit RPC，不将设备权限转换成 OIDC Cookie、
普通 update-api 或块上传权限。设备挑战 nonce 单次消费；加密上传凭据允许
原 Intent 幂等重试，原生状态防止二次发布。查询仅返回回执元数据，无文件内容。

## 本地执行入口

`cloudfile-agent2 --edit-upload-stdin` 从标准输入读取受限 JSON，供本地控制器使用。
要求当前用户已有配对、可信 origin 和
`CloudFileAgent2/editing/<session_id>/journal.json` 中的既有 Checkout 状态。
不会由浏览器消息创建状态或指定上传路径，也不会覆盖已有 Journal。
测试环境须显式设置 `CLOUDFILE_AGENT2_EXCLUSIVE_ENABLED=1`。

输入字段：instance、device_id、session_id、action（commit/checkin/recover）、
user_confirmed=true。Commit/Checkin 还需绝对 working_copy；recover 仅传原
intent_id。凭据与路径不放命令行参数。失败输出 pending Intent，成功输出原生
published 回执。Windows 使用同安装目录的 CNG Bridge；macOS 使用已确认的
当前用户 Keychain 配对；都不要求管理员权限。

此入口不代替尚待接线的完整桌面 Checkout、下载和创建 Journal 交互。
不要手工编辑 Journal 来绕过会话建立，不要将该入口直接暴露给网页。

## 验证边界

Go TLS 契约测试覆盖快照、真实磁盘恢复与丢失响应；Hub 测试覆盖真实加密凭据、
流式字节接收和 RPC 回执恢复。隔离 MariaDB 用例执行从生产 C 代码提取的原生
SQL，覆盖撤销、到期、冲突、重复签名、Commit 基线推进及 Checkin 后查询。
Linux 原生服务完成全量编译。这些证据仍不等同于运行中完整服务的桌面端到端验收。
