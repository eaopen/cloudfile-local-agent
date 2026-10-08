param(
  [string]$ExtensionId = "gocnpbhnbikcpnlafmodflddgnmfcjoc",
  [string[]]$ServerOrigin = @("http://10.9.8.162:6111", "http://etech.stcetech.ad01.sec.com"),
  # 更新源固定指向门户域名，由该域名上的 nginx 把 /cloudfile-updates/ 反代到实际静态服务。
  # 静态服务换 IP/端口时只改 nginx，已装机机器无需重跑本脚本。
  [string]$UpdateSource = "http://etech.stcetech.ad01.sec.com/cloudfile-updates/update.json",
  [string]$ExtensionUpdateSource = "http://etech.stcetech.ad01.sec.com/cloudfile-updates/extension-update.json",
  [string]$BinaryPath = ""
)

$ErrorActionPreference = "Stop"

# 固定目录：Agent 二进制、扩展解压目录、native host manifest 均放 %LocalAppData%\CloudFileLocal。
$root = Join-Path $env:LOCALAPPDATA "CloudFileLocal"
$extensionDir = Join-Path $root "extension"
$manifest = Join-Path $root "com.cloudfile.local_agent.json"

function Write-Step([string]$msg) {
  Write-Host ("==> " + $msg) -ForegroundColor Cyan
}

function Resolve-Url([string]$baseUrl, [string]$ref) {
  if ($ref -match '^https?://') { return $ref }
  return (New-Object System.Uri((New-Object System.Uri($baseUrl)), $ref)).AbsoluteUri
}

# 下载单个文件并做 SHA256 校验（期望值小写）。
function Get-File-Checked([string]$url, [string]$dest, [string]$wantSha256) {
  Write-Host ("  下载 " + $url)
  Invoke-WebRequest -Uri $url -OutFile $dest -TimeoutSec 300
  $actual = (Get-FileHash -LiteralPath $dest -Algorithm SHA256).Hash.ToLowerInvariant()
  if ($wantSha256 -and ($actual -ne $wantSha256.ToLowerInvariant())) {
    Remove-Item -LiteralPath $dest -Force -ErrorAction SilentlyContinue
    throw ("SHA256 校验失败：期望 " + $wantSha256 + "，实际 " + $actual)
  }
  Write-Host ("  SHA256 校验通过 " + $actual) -ForegroundColor Green
}

# 查找脚本所在目录（$PSScriptRoot）与上一层目录下的版本号化 Agent exe。
function Find-AgentBinary([string]$dir) {
  if (-not $dir) { return $null }
  $versioned = Get-ChildItem -LiteralPath $dir -Filter "cloudfile-local-agent-*.exe" -File -ErrorAction SilentlyContinue |
    Sort-Object Name -Descending | Select-Object -First 1
  if ($versioned) { return (Resolve-Path -LiteralPath $versioned.FullName).Path }
  return $null
}

# 从 update.json 下载 Agent exe（含 SHA256 校验），返回本地路径。
function Download-Agent([string]$updateSource, [string]$destDir) {
  $json = Invoke-RestMethod -Uri $updateSource -TimeoutSec 30
  if (-not $json.download_url -or -not $json.sha256) {
    throw "更新源缺少 download_url 或 sha256 字段：$updateSource"
  }
  $downloadUrl = Resolve-Url $updateSource $json.download_url
  $fileName = Split-Path $downloadUrl -Leaf
  $dest = Join-Path $destDir $fileName
  Get-File-Checked $downloadUrl $dest $json.sha256
  return (Resolve-Path -LiteralPath $dest).Path
}

# 下载扩展 zip 并解压到固定目录，返回扩展目录（含 manifest.json）。
function Install-Extension([string]$extUpdateSource, [string]$destDir) {
  $json = Invoke-RestMethod -Uri $extUpdateSource -TimeoutSec 30
  if (-not $json.zip_url) {
    throw "扩展更新源缺少 zip_url 字段：$extUpdateSource"
  }
  $zipUrl = Resolve-Url $extUpdateSource $json.zip_url
  $zipFile = Join-Path $destDir (Split-Path $zipUrl -Leaf)
  $zipSha = $json.sha256  # 可能为空（旧 JSON 无此字段），为空则跳过校验
  Get-File-Checked $zipUrl $zipFile $zipSha

  Write-Step "解压扩展"
  if (Test-Path -LiteralPath $destDir) {
    Remove-Item -LiteralPath $destDir -Recurse -Force -ErrorAction SilentlyContinue
  }
  New-Item -ItemType Directory -Force -Path $destDir | Out-Null
  Expand-Archive -LiteralPath $zipFile -DestinationPath $destDir -Force
  if (-not (Test-Path -LiteralPath (Join-Path $destDir "manifest.json"))) {
    throw "解压后的目录缺少 manifest.json，扩展包可能不完整。"
  }
  return (Resolve-Path -LiteralPath $destDir).Path
}

Write-Step "开始安装 CloudFile 本地组件"

New-Item -ItemType Directory -Force -Path $root | Out-Null

# 1. 定位或下载 Agent exe
Write-Step "准备本地 Agent"
if ($BinaryPath -eq "") {
  $BinaryPath = Find-AgentBinary $PSScriptRoot
  if (-not $BinaryPath) { $BinaryPath = Find-AgentBinary (Join-Path $PSScriptRoot "..") }
  if (-not $BinaryPath -and $UpdateSource -ne "") {
    $BinaryPath = Download-Agent $UpdateSource $PSScriptRoot
  }
  if (-not $BinaryPath) {
    Write-Error "未找到 cloudfile-local-agent-<version>.exe（脚本目录与上一层目录均无，且未配置 -UpdateSource 下载），请用 -BinaryPath 指定。"
    exit 1
  }
} elseif (-not (Test-Path -LiteralPath $BinaryPath)) {
  Write-Error "指定的 -BinaryPath 不存在：$BinaryPath"
  exit 1
}
Write-Host ("  Agent: " + $BinaryPath) -ForegroundColor Green

# 2. 下载并解压扩展
$extDir = $null
if ($ExtensionUpdateSource -ne "") {
  Write-Step "准备 Chrome 扩展"
  try {
    $extDir = Install-Extension $ExtensionUpdateSource $extensionDir
    Write-Host ("  扩展目录: " + $extDir) -ForegroundColor Green
  } catch {
    Write-Host ("  扩展下载/解压失败：`n  " + $_.Exception.Message) -ForegroundColor Yellow
    Write-Host "  可稍后手动下载扩展 zip 并解压后，在 chrome://extensions 加载。" -ForegroundColor Yellow
    $extDir = $null
  }
}

# 3. 信任 origin
Write-Step "配置信任站点与升级源"
foreach ($origin in $ServerOrigin) {
  & $BinaryPath --allow-origin $origin
  if ($LASTEXITCODE -ne 0) {
    Write-Error "执行 --allow-origin $origin 失败：$BinaryPath"
    exit 1
  }
}
if ($UpdateSource -ne "") {
  & $BinaryPath --set-update-source $UpdateSource
  if ($LASTEXITCODE -ne 0) {
    Write-Error "执行 --set-update-source 失败：$BinaryPath"
    exit 1
  }
}

# 4. 写 native host manifest + 注册表
Write-Step "注册 Native Messaging Host"
@{
  name = "com.cloudfile.local_agent"
  description = "CloudFile Local Agent"
  path = $BinaryPath
  type = "stdio"
  allowed_origins = @("chrome-extension://$ExtensionId/")
} | ConvertTo-Json | Set-Content -Encoding utf8 -NoNewline $manifest

New-Item -Path "HKCU:\Software\Google\Chrome\NativeMessagingHosts\com.cloudfile.local_agent" -Force | Out-Null
Set-ItemProperty -Path "HKCU:\Software\Google\Chrome\NativeMessagingHosts\com.cloudfile.local_agent" -Name '(default)' -Value $manifest
Write-Host ("  已注册，扩展 ID: " + $ExtensionId) -ForegroundColor Green

# 5. 引导启用扩展：打开 chrome://extensions + 复制目录路径 + 提示
Write-Step "引导启用扩展"
if ($extDir) {
  Set-Clipboard -Value $extDir
  Write-Host "  扩展目录路径已复制到剪贴板：" -ForegroundColor Green
  Write-Host ("  " + $extDir) -ForegroundColor Green
} else {
  Set-Clipboard -Value $extensionDir
  Write-Host "  已把扩展目标目录复制到剪贴板（需先手动放置扩展文件）：" -ForegroundColor Yellow
  Write-Host ("  " + $extensionDir) -ForegroundColor Yellow
}

Start-Process "chrome.exe" "chrome://extensions"

Write-Host ""
Write-Host "======================================================" -ForegroundColor Cyan
Write-Host "  Agent 安装完成，还差最后一步启用扩展：" -ForegroundColor Cyan
Write-Host "  1. 在已打开的 chrome://extensions 页右上角打开「开发者模式」" -ForegroundColor Cyan
Write-Host "  2. 点「加载已解压的扩展程序」" -ForegroundColor Cyan
Write-Host "  3. 在文件夹选择框地址栏 Ctrl+V 粘贴路径（已复制到剪贴板）回车" -ForegroundColor Cyan
Write-Host "  4. 选择文件夹即可完成" -ForegroundColor Cyan
Write-Host "======================================================" -ForegroundColor Cyan
Write-Host ""
Write-Host "扩展 ID 应为固定值: " + $ExtensionId
