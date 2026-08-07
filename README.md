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

## 本地查看与编辑规则

规则只保存于当前用户的 Agent 配置文件：Windows 为
`%AppData%\\CloudFileLocal\\config.json`，macOS/Linux 使用系统用户配置目录下的
`CloudFileLocal/config.json`。先用安装脚本或 `--allow-origin` 创建信任 origin，再按需要
加入 `open_rules`；`--validate-config` 可在不启动会话时检查配置。

```json
{
  "allowed_origins": ["https://cloudfile.example"],
  "workspace_root": "D:\\CloudFileLocal\\sessions",
  "open_rules": [
    {
      "id": "cad-viewer",
      "modes": ["local-view", "local-edit"],
      "extensions": ["dwg", "dxf", "step"],
      "command": ["C:\\Program Files\\CAD\\cad.exe", "{file}"]
    },
    {
      "id": "view-fallback",
      "modes": ["local-view"],
      "extensions": ["*"],
      "command": ["C:\\Program Files\\Viewer\\viewer.exe", "{file}"]
    }
  ]
}
```

规则按顺序匹配，具体扩展名应位于通配规则之前。`command[0]` 必须是本机绝对路径，且整个
命令只能有一个 `{file}` 占位符；Agent 使用进程参数直接启动，不经过 shell。没有命中规则
时才回退到系统默认程序。远端 `.cloudfile` 会话从不携带程序路径、命令行或规则。

## 安全模型

- 仅接收 `cloudfile-local/v2` 会话文件；会话文件只包含短期单次领取票据。
- 每个 CloudFile origin 都须由用户/安装脚本显式加入本地信任列表。
- Native Host manifest 精确指定正式扩展 ID；不使用通配 origin 或浏览器 Cookie。
- 每个会话拥有独立工作区；本地编辑使用短时 write-back capability。
- Agent 只接受常规且不超过 1 MiB 的会话文件；下载内容超过 4 GiB 会中止并清理。

Chrome 扩展只把下载完成的 `.cloudfile` 文件路径转交给 Native Host；真正的内容能力由 Agent 直接向 CloudFile Hub 领取。
