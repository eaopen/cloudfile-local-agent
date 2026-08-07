# CloudFile Local Agent

一个 Go 绿色 Native Messaging Host，用于领取 CloudFile 的短时会话、在隔离工作区下载文件，并交给本机默认应用查看或编辑。

## 安装与启动

需要 Go 1.24 以上才能从源码构建；发布包是无运行时依赖的单文件二进制。

```bash
go test ./...
go build -trimpath -ldflags="-s -w" -o dist/cloudfile-local-agent ./cmd/cloudfile-local-agent
./dist/cloudfile-local-agent --allow-origin https://cloudfile.example
```

Windows 绿色包用 `scripts/register-windows.ps1` 写入当前用户的 Native Messaging Host 注册表项；不需要管理员权限，也不开放 localhost HTTP 服务。

## 安全模型

- 仅接收 `cloudfile-local/v2` 会话文件；会话文件只包含短期单次领取票据。
- 每个 CloudFile origin 都须由用户/安装脚本显式加入本地信任列表。
- Native Host manifest 精确指定正式扩展 ID；不使用通配 origin 或浏览器 Cookie。
- 每个会话拥有独立工作区；本地编辑使用短时 write-back capability。

Chrome 扩展只把下载完成的 `.cloudfile` 文件路径转交给 Native Host；真正的内容能力由 Agent 直接向 CloudFile Hub 领取。
