param(
  [string]$ExtensionId = "gocnpbhnbikcpnlafmodflddgnmfcjoc",
  [string[]]$ServerOrigin = @("http://10.9.8.162:6111", "http://etech.stcetech.ad01.sec.com"),
  # 更新源固定指向门户域名，由该域名上的 nginx 把 /cloudfile-updates/ 反代到实际静态服务。
  # 静态服务换 IP/端口时只改 nginx，已装机机器无需重跑本脚本。
  [string]$UpdateSource = "http://etech.stcetech.ad01.sec.com/cloudfile-updates/update.json",
  [string]$ExtensionUpdateSource = "http://etech.stcetech.ad01.sec.com/cloudfile-updates/extension-update.json",
  [string]$BinaryPath = "",
  # 安装根目录。留空则自动选：D:\CloudFileLocal → C:\CloudFileLocal →
  # %LocalAppData%\CloudFileLocal。install.cmd 会显式传入自己选好的目录，
  # 保证「脚本、下载的 exe/zip、扩展目录、native host manifest」全在同一处
  # （Agent 的 internal/home 用同一套规则，否则自动升级会写到 Chrome 不读的地方）。
  [string]$InstallDir = "",
  # 无人值守安装（SCCM/GPO/远程会话）用：不写剪贴板，只打印路径。
  [switch]$Silent
)

$ErrorActionPreference = "Stop"

# 关掉 Invoke-WebRequest / Expand-Archive 的进度条：一个 6.7MB 的下载每 10KB 就打一行
# 「正在写入 Web 请求」，几百行刷满控制台，既盖住真正的输出也明显拖慢安装。
# 脚本自己会在关键节点打印「下载 …」「SHA256 校验通过」，不会让人看不到进展。
$ProgressPreference = "SilentlyContinue"

function Write-Step([string]$msg) {
  Write-Host ("==> " + $msg) -ForegroundColor Cyan
}

# 目录能不能真正写：建目录 + 写一个探针文件。只读挂载、满盘、无权限只有真写过才知道。
function Test-WritableDir([string]$dir) {
  try {
    New-Item -ItemType Directory -Force -Path $dir -ErrorAction Stop | Out-Null
    $probe = Join-Path $dir (".write-probe-" + [guid]::NewGuid().ToString("N"))
    Set-Content -LiteralPath $probe -Value "ok" -Encoding ASCII -ErrorAction Stop
    Remove-Item -LiteralPath $probe -Force -ErrorAction SilentlyContinue
    return $true
  } catch {
    return $false
  }
}

# 解析安装根目录：显式参数 > CLOUDFILE_HOME > D:\CloudFileLocal > C:\CloudFileLocal
# > %LocalAppData%\CloudFileLocal。
function Resolve-InstallDir([string]$explicit) {
  $wanted = ""
  if ($explicit) { $wanted = $explicit.Trim() }
  elseif ($env:CLOUDFILE_HOME) { $wanted = $env:CLOUDFILE_HOME.Trim() }
  if ($wanted) {
    New-Item -ItemType Directory -Force -Path $wanted | Out-Null
    return (Resolve-Path -LiteralPath $wanted).Path
  }
  foreach ($candidate in @("D:\CloudFileLocal", "C:\CloudFileLocal")) {
    if (Test-WritableDir $candidate) { return $candidate }
  }
  $fallback = Join-Path $env:LOCALAPPDATA "CloudFileLocal"
  New-Item -ItemType Directory -Force -Path $fallback | Out-Null
  return $fallback
}

$root = Resolve-InstallDir $InstallDir
$extensionDir = Join-Path $root "extension"
$manifest = Join-Path $root "com.cloudfile.local_agent.json"
$legacyRoot = Join-Path $env:LOCALAPPDATA "CloudFileLocal"

function Resolve-Url([string]$baseUrl, [string]$ref) {
  if ($ref -match '^https?://') { return $ref }
  return (New-Object System.Uri((New-Object System.Uri($baseUrl)), $ref)).AbsoluteUri
}

# 取更新源同级目录里的另一个静态文件（update.json → 同目录的 help.html / install.cmd）。
function Resolve-SiblingUrl([string]$updateSource, [string]$fileName) {
  if (-not $updateSource) { return "" }
  try {
    return (New-Object System.Uri((New-Object System.Uri($updateSource)), $fileName)).AbsoluteUri
  } catch {
    return ""
  }
}

# 下载单个文件并做 SHA256 校验（期望值小写）。
function Get-File-Checked([string]$url, [string]$dest, [string]$wantSha256) {
  Write-Host ("  下载 " + $url)
  # Invoke-WebRequest 只会创建文件，不会创建父目录；全新机器上目标目录还不存在时
  # 会直接报「未能找到路径 … 的一部分」。这里先把父目录建出来。
  $parent = Split-Path -Parent $dest
  if ($parent) { New-Item -ItemType Directory -Force -Path $parent | Out-Null }
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
  $zipSha = $json.sha256  # 可能为空（旧 JSON 无此字段），为空则跳过校验

  # zip 必须下到目标目录之外：下面要把 $destDir 整个删掉再解压，
  # 以前 zip 就放在 $destDir 里，于是「删目录」顺手把 zip 也删了，
  # 紧接着 Expand-Archive 必然报「未能找到路径 … 的一部分」。
  $scratch = Join-Path ([System.IO.Path]::GetTempPath()) ("cloudfile-ext-" + [guid]::NewGuid().ToString("N"))
  New-Item -ItemType Directory -Force -Path $scratch | Out-Null
  try {
    $zipFile = Join-Path $scratch (Split-Path $zipUrl -Leaf)
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
  } finally {
    # 临时 zip 用完即删，不留残件在磁盘上。
    Remove-Item -LiteralPath $scratch -Recurse -Force -ErrorAction SilentlyContinue
  }
}

Write-Step "开始安装 CloudFile 本地组件"

New-Item -ItemType Directory -Force -Path $root | Out-Null

# 1. 定位或下载 Agent exe
Write-Step "准备本地 Agent"
if ($BinaryPath -eq "") {
  # 有更新源就优先取最新的：这个脚本最常见的用途就是「安装 / 升级」，
  # 而安装根目录里往往还躺着上一次下载的旧 exe —— 先找本地会让人以为重跑
  # 脚本升级过了，其实版本没变。取不到才退回本地已有的 exe（绿色包离线安装）。
  if ($UpdateSource -ne "") {
    try {
      $BinaryPath = Download-Agent $UpdateSource $root
    } catch {
      Write-Host ("  在线获取 Agent 失败： " + $_.Exception.Message) -ForegroundColor Yellow
    }
  }
  if (-not $BinaryPath) { $BinaryPath = Find-AgentBinary $root }
  if (-not $BinaryPath) { $BinaryPath = Find-AgentBinary $PSScriptRoot }
  if (-not $BinaryPath) { $BinaryPath = Find-AgentBinary (Join-Path $PSScriptRoot "..") }
  if (-not $BinaryPath) {
    Write-Error "未找到 cloudfile-local-agent-<version>.exe（安装目录、脚本目录与上一层目录均无，且未能从更新源下载），请用 -BinaryPath 指定。"
    exit 1
  }
} elseif (-not (Test-Path -LiteralPath $BinaryPath)) {
  Write-Error "指定的 -BinaryPath 不存在：$BinaryPath"
  exit 1
}
Write-Host ("  Agent: " + $BinaryPath) -ForegroundColor Green
Write-Host ("  安装目录: " + $root) -ForegroundColor Green

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

# 老版本装在 %LocalAppData%\CloudFileLocal 下，扩展也是从那里加载的。本次已迁到
# 新安装目录，旧的扩展目录会一直留在 Chrome 里并继续跑旧代码，必须提示移除。
$legacyInstall = (Test-Path -LiteralPath (Join-Path $legacyRoot "com.cloudfile.local_agent.json")) -and ($manifest -notlike ($legacyRoot + "*"))
if ($legacyInstall) {
  Write-Host ("  提示：检测到旧安装仍在 " + $legacyRoot) -ForegroundColor Yellow
  Write-Host "        请在 chrome://extensions 里先「移除」旧的 CloudFile 扩展，再按下面步骤加载新目录。" -ForegroundColor Yellow
}

# 5. 引导启用扩展：复制目录路径 + 打印「去帮助页点按钮」的提示
# 脚本不打开浏览器：扩展管理页没法由外部程序打开（Chrome 会丢弃任何来自外部进程的
# chrome:// 地址，实测命令行传 chrome://extensions 只得到一个新标签页），而网页里
# 也没法跳过去（链接 / location.href / target=_blank / window.open 全被拦，
# 控制台报 Not allowed to load local resource）。所以交给帮助页上的
# 「打开扩展页面」按钮：点一下复制地址，用户 Ctrl+L、Ctrl+V、回车即可。
Write-Step "引导启用扩展"
$extensionPath = if ($extDir) { $extDir } else { $extensionDir }
$helpUrl = Resolve-SiblingUrl $UpdateSource "help.html"

# 剪贴板是锦上添花，不能成为安装的终点：远程会话、剪贴板被别的程序占用、没有交互
# 桌面时 Set-Clipboard 会抛 ExternalException，而本脚本开着
# $ErrorActionPreference = "Stop"，以前会直接把整个脚本打断在这里，
# 连后面的引导和收尾提示都不会执行。
$clipboardOk = $false
if ($Silent) {
  Write-Host "  已跳过剪贴板（-Silent），扩展目录见下面这行：" -ForegroundColor Yellow
} else {
  try {
    Set-Clipboard -Value $extensionPath -ErrorAction Stop
    $clipboardOk = $true
    if ($extDir) {
      Write-Host "  扩展目录路径已复制到剪贴板：" -ForegroundColor Green
    } else {
      Write-Host "  已把扩展目标目录复制到剪贴板（需先手动放置扩展文件）：" -ForegroundColor Yellow
    }
  } catch {
    Write-Host "  无法写入剪贴板（不影响安装），请手动复制下面这行路径：" -ForegroundColor Yellow
  }
}
Write-Host ("  " + $extensionPath) -ForegroundColor Green

Write-Host ""
Write-Host "======================================================" -ForegroundColor Cyan
Write-Host "  Agent 安装完成，还差最后一步启用扩展（每台机器只需一次）：" -ForegroundColor Cyan
Write-Host "  1. 打开安装帮助页（下面这几步页面上也有）：" -ForegroundColor Cyan
if ($helpUrl) {
  Write-Host ("     " + $helpUrl) -ForegroundColor Cyan
}
Write-Host "  2. 打开扩展管理页：Chrome 右上角的菜单（三个点）→「扩展程序」→「管理扩展程序」" -ForegroundColor Cyan
Write-Host "     （或直接在地址栏输入 chrome://extensions 回车）" -ForegroundColor Cyan
Write-Host "  3. 打开右上角「开发者模式」，点「加载已解压的扩展程序」" -ForegroundColor Cyan
if ($clipboardOk) {
  Write-Host "  4. 在文件夹窗口按 Ctrl+L、Ctrl+V（扩展目录已在剪贴板里，中途别复制别的东西）、" -ForegroundColor Cyan
  Write-Host "     回车，点「选择文件夹」" -ForegroundColor Cyan
} else {
  Write-Host "  4. 在文件夹窗口粘贴下面这行扩展目录、回车，点「选择文件夹」：" -ForegroundColor Cyan
  Write-Host ("     " + $extensionPath) -ForegroundColor Cyan
}
Write-Host ("  扩展目录: " + $extensionPath) -ForegroundColor Cyan
Write-Host "======================================================" -ForegroundColor Cyan
Write-Host ""
Write-Host ("扩展 ID 应为固定值: " + $ExtensionId)
