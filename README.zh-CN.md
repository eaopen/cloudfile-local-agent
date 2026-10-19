# CloudFile Local Agent

> [!CAUTION]
> **Rust Agent2 已迁移，本分支 `agent-rust` 自 2026-10-20 起停止开发。**
> **请勿继续在此分支或 `agent22/` 目录修改、提交、发版，也不要把开发 PR 指向这里。**
> Rust Agent2 唯一后续开发仓库为 **[eaopen/cloudfile-local-agent2](https://github.com/eaopen/cloudfile-local-agent2)**，以其 `dev` 为主线，使用新仓 feature 分支和 PR。
> 此处 `agent22/` 仅供历史查阅，不代表现行 V2 实现。迁移中的变更见 [新仓 PR #7](https://github.com/eaopen/cloudfile-local-agent2/pull/7)（是否已合入需核对 PR 状态）。
> AI Coding 请先阅读本分支的 [AGENTS.md](AGENTS.md)。

[English](README.md) | [简体中文](README.zh-CN.md)

[产品中心](https://github.com/eaopen/cloudfile) · [产品能力与路线](https://github.com/eaopen/cloudfile/blob/main/docs/zh-CN/README.md)

本地应用助手，通过受控会话和 Native Messaging 连接 CloudFile 与已安装的专业软件。文件的实际访问、提交与版本发布仍受服务端授权约束；平台和发行包支持范围分别核对。

## 技术文档

| 入口 | 内容 |
| --- | --- |
| [技术参考](docs/technical-reference.md) | 构建、配置、应用选择、安全边界、Host 路由、版本及开发阶段说明 |
| [macOS](docs/macos.md) | 安装、Keychain、更新及平台边界 |
| [设备上传契约](docs/device-upload-contract.md) | 设备授权、提交回执、恢复与尚未开放范围 |

## 从源码验证

```bash
go test ./...
go build -trimpath -ldflags="-s -w" -o dist/cloudfile-local-agent ./cmd/cloudfile-local-agent
```

这些命令验证和构建公开 Go Core，不代表已生成所有平台的完整发行包。Windows 当前完整 .NET＋Go 包仍依赖私有构建资产；通用源码的公开独立构建是后续迁移目标，当前公开仓库不能单独复现该完整包。macOS 使用本仓平台说明和脚本。

本地保存不等于服务端新版本，须依据当前包的能力和提交回执判断完成；尚未通过整体验收的编辑路径不作为正式支持声明。

## 许可与维护

通用 Agent 按完整开源产品方向维护，客户专属业务适配独立管理。自有代码采用 [Apache 2.0](LICENSE)，依赖保留各自许可，见 [NOTICE](NOTICE)。产品计划在产品中心维护，具体协议与支持边界随本仓代码更新。
