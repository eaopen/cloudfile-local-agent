<#
  CloudFile Agent2 —— Windows 每用户注册脚本（纯 Go，不依赖 .NET）

  做什么：把 Go Agent2 装进每用户目录、写 native messaging manifest、在 HKCU 注册
  com.cloudfile.agent2，让 Chrome 扩展直接连它。Host 名与 macOS 完全一致，两边同一份代码。
  顺手把扩展包取下来、把任务中心（托盘 + 任务列表）装好并设开机自启 —— 托盘是用户唯一
  的常驻入口，装完立刻启动，这样右下角就有东西可点。

  为什么不用 .NET：Windows 的 .NET 统一包（GUI/托盘 + CNG 设备签名 + 配对）当前没有交付
  需求，而 Go 单文件既轻又能和 macOS 共用。托盘/任务中心也是 Go 写的（纯 Win32 syscall）。

  用法（都不需要管理员）：
    # 安装 / 升级（不传 -UpdateSource 时默认指向 cloudfile-updates-dev 测试通道）
    .\register-agent2-windows.ps1 .\dist\cloudfile-local-agent-windows-amd64.exe
    # 不给 exe：从更新源现下（校验 sha256 后安装）—— 双击 install.cmd 走的就是这条
    .\register-agent2-windows.ps1
    # 安装 + 指定更新源
    .\register-agent2-windows.ps1 .\dist\agent.exe -UpdateSource http://etech.stcetech.ad01.sec.com/cloudfile-updates/update.json
    # 只装 Agent 与扩展，不要托盘（右下角不会有图标）
    .\register-agent2-windows.ps1 -SkipTaskCenter
    # 回滚到上一个已注册版本 / 卸载（只删自己的注册，用户数据与工作副本保留）
    .\register-agent2-windows.ps1 -Rollback
    .\register-agent2-windows.ps1 -Unregister

  一键安装：测试通道里同时发布了一个 install.cmd（与用户下载目录无关，
  双击即可）。它只做两件事：从通道下载本脚本、再调用本脚本不带 exe。
  所以「下载 Agent + 校验 + 注册 + 装扩展」这套逻辑只有本脚本一份实现。

  更新源：现阶段默认发布的是**测试版**，不传 -UpdateSource 就指向测试通道
  http://etech.stcetech.ad01.sec.com/cloudfile-updates-dev/update.json
  —— 与正式通道同一个 host，只把路径里的 cloudfile-updates 换成 cloudfile-updates-dev，
  生产与测试靠这一节区分，正式通道的产物一个字节都不动。
  -UpdateSource 也可以给「基址」（以 / 结尾），脚本会自己补 update.json。

  产物来源：cloudfile-release\tools\dev-release\make-dev-release.ps1（编译 + 清单 +
  扩展包，输出到 cloudfile-release\cloudfile-updates-dev\），把这个目录整个上传到
  服务器的 cloudfile-updates-dev\ 即可；本机离线自测才需要 serve-dev.ps1
  （它把同一目录挂在 http://127.0.0.1:80/cloudfile-updates-dev/ 下，URL 形状与线上一致）。

  正式发版时把下面 $DevUpdateSource 那一行改回 .../cloudfile-updates/update.json，
  其余逻辑一行都不用动。

  安装布局（与 Agent 的 internal/home 约定一致：exe 在 <root>\bin 下时，
  home.Dir() 会往上找到 <root>\com.cloudfile.agent2.json，从而定位到同一个安装根目录）：
    <root>\bin\cloudfile-agent2-<版本>-<sha256前16>.exe   内容寻址，多版本可共存
    <root>\com.cloudfile.agent2.json                      native messaging manifest
    <root>\previous-native-host.json                      回滚用
    <root>\extension\                                     扩展解压目录（由 Agent 自己更新）
#>
param(
  [Parameter(Position = 0)][string] $SourceBinary = '',
  [string] $ExtensionId = 'gocnpbhnbikcpnlafmodflddgnmfcjoc',
  [string[]] $TrustedOrigin = @(
    'http://10.9.8.162:6111',
    'http://10.12.1.157:8092',
    'http://etech.stcetech.ad01.sec.com',
    'https://etech.stcetech.ad01.sec.com'
  ),
  [string] $UpdateSource = '',   # 留空 = 用下面的 $DevUpdateSource（测试通道）
  [string] $InstallDir = '',
  [switch] $Rollback,
  [switch] $Unregister,
  [switch] $SkipTaskCenter,
  [switch] $Silent
)

$ErrorActionPreference = 'Stop'
# 下载 7MB 的 Agent 时 Invoke-WebRequest 默认会往控制台狂刷「正在写入 Web 请求...」，
# 在安装窗口里看就是刷屏（用户会以为卡住了）。这里全局关掉进度条。
$ProgressPreference = 'SilentlyContinue'

$HostName = 'com.cloudfile.agent2'
$RegistryPath = "HKCU:\Software\Google\Chrome\NativeMessagingHosts\$HostName"
$VersionPattern = '^2\.\d+\.\d+$'
# 托盘的每用户自启项（不需要管理员）。
$RunKey = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run'
$RunValue = 'CloudFileTaskCenter'
# Run 项写不进去时的备选自启（同样是每用户、不需要管理员）。
$StartupLink = 'CloudFileTaskCenter.lnk'
# TaskCenter 只认这两个文件名，清单里出现别的名字一律拒绝。
$TaskCenterFiles = @('cloudfile-taskcenter.exe', 'cloudfile-taskcenter-ui.exe')
# 未给本地 exe 时从更新源现下，落在下面这个临时目录里；任何失败路径都要清掉。
$script:DownloadTemp = ''
$script:DownloadedVersion = ''
# 托盘自启最后落在哪条途径（HKCU Run / Startup / 空串=没设上），收尾提示用。
$script:Autostart = ''

function Write-Step([string] $message) { Write-Host ('==> ' + $message) -ForegroundColor Cyan }
function Write-Note([string] $message) { Write-Host ('    ' + $message) -ForegroundColor Green }
function Write-Warn([string] $message) { Write-Host ('[警告] ' + $message) -ForegroundColor Yellow }

# 建注册表键（存在就什么都不做，只建链上缺的）。
#
# **绝不要直接写 `New-Item -Path <注册表路径> -Force`**：PowerShell 5.1 在注册表上把
# -Force 实现成「先删掉再重建」，而 Windows 禁止删除
# HKCU\Software\Microsoft\Windows\CurrentVersion 及其下的 Run 这类受保护的键 ——
# 于是报 UnauthorizedAccessException「尝试执行未经授权的操作」。在
# $ErrorActionPreference='Stop' 下它就是终止错误，会把整个注册判成失败（用户看到
# 「注册脚本返回失败」，但 Agent 其实早就装好了）。
# 键不存在时仍用 -Force：注册表 provider 只有靠它才会把中间层一起建出来。
function New-RegistryKey([string] $path) {
  if (-not (Test-Path -LiteralPath $path)) { New-Item -Path $path -Force | Out-Null }
}

# 删掉本次下载的临时目录，别在 %TEMP% 里留一个 7MB 的 exe。
# 刚落地的 exe 常被杀软/延迟句柄占住几秒（实测本机三次安装都留下了一个删不掉的目录），
# 一次删不掉就退避重试；仍然删不掉就认了（%TEMP% 有系统清理，留一行警告）。
function Remove-DownloadTemp {
  if (-not $script:DownloadTemp) { return }
  for ($attempt = 0; $attempt -lt 5; $attempt++) {
    if (-not (Test-Path -LiteralPath $script:DownloadTemp)) { break }
    Remove-Item -LiteralPath $script:DownloadTemp -Recurse -Force -ErrorAction SilentlyContinue
    if (-not (Test-Path -LiteralPath $script:DownloadTemp)) { break }
    Start-Sleep -Milliseconds (300 * ($attempt + 1))
  }
  if (Test-Path -LiteralPath $script:DownloadTemp) {
    Write-Warn ('临时目录暂时删不掉（可能被杀软占住），系统清理时会收掉：' + $script:DownloadTemp)
  }
  $script:DownloadTemp = ''
}

# 自愈：以前几次安装没删掉的下载临时目录（被杀软占住过），超过一天的一并清掉。
# 只认自己这个前缀、且只清一天前的 —— 同机上正在跑的另一次安装用的是全新目录，不会被误伤。
function Remove-StaleDownloadTemp {
  $cutoff = (Get-Date).AddDays(-1)
  Get-ChildItem -LiteralPath ([IO.Path]::GetTempPath()) -Directory -Filter 'cf-agent2-install-*' -ErrorAction SilentlyContinue |
    Where-Object { $_.LastWriteTime -lt $cutoff } |
    ForEach-Object { Remove-Item -LiteralPath $_.FullName -Recurse -Force -ErrorAction SilentlyContinue }
}

function Fail([string] $message) {
  Write-Host ('[错误] ' + $message) -ForegroundColor Red
  Remove-DownloadTemp
  exit 1
}

# ── 安装根目录 ────────────────────────────────────────────────────────
$root = if ($InstallDir) { $InstallDir }
elseif ($env:CLOUDFILE_AGENT2_HOME) { $env:CLOUDFILE_AGENT2_HOME }
else { Join-Path $env:LOCALAPPDATA 'CloudFileAgent2' }
$root = [IO.Path]::GetFullPath($root)
$binDir = Join-Path $root 'bin'
$manifestPath = Join-Path $root ($HostName + '.json')
$previousPath = Join-Path $root 'previous-native-host.json'

function Get-RegisteredManifest {
  if (-not (Test-Path -LiteralPath $RegistryPath)) { return '' }
  return [string](Get-ItemProperty -LiteralPath $RegistryPath -ErrorAction SilentlyContinue).'(default)'
}

# 只认自己写的 manifest：名字、类型对，且 path 落在本安装的 bin 目录下。
# 身份不符一律拒绝操作，避免误删/误覆盖别人的注册。
function Test-OwnedManifest([string] $path) {
  if (-not $path -or -not (Test-Path -LiteralPath $path -PathType Leaf)) { return $false }
  try { $json = Get-Content -LiteralPath $path -Raw -Encoding UTF8 | ConvertFrom-Json } catch { return $false }
  if ($json.name -ne $HostName -or $json.type -ne 'stdio') { return $false }
  try { $target = [IO.Path]::GetFullPath([string]$json.path) } catch { return $false }
  return $target.StartsWith($binDir + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)
}

# 用 --component 跑一次能力查询：确认是可用的 Windows Agent2，且 IPC/Journal 版本对得上。
function Assert-Healthy([string] $exe) {
  $payload = '{"ipc_version":1,"request":{"type":"capabilities"}}'
  $raw = @($payload | & $exe --component 2>$null) -join ''
  if (-not $raw) { Fail ('健康自检无输出（不是可用的 Agent2 二进制）：' + $exe) }
  try { $response = $raw | ConvertFrom-Json } catch { Fail ('健康自检返回的不是 JSON：' + $raw) }
  if (-not $response.ok) { Fail ('健康自检未返回 ok：' + $raw) }
  if ($response.result.platform -ne 'windows') { Fail ('不是 Windows 平台构建：' + $response.result.platform) }
  if ($response.result.ipc_version -ne 1 -or $response.result.journal_version -ne 1) { Fail ('IPC/Journal 版本不兼容：' + $raw) }
  return $response.result.core_version
}

function Write-Manifest([string] $targetPath) {
  $json = [ordered]@{
    name            = $HostName
    description     = 'CloudFile Agent2 Windows Native Host'
    type            = 'stdio'
    path            = $targetPath
    allowed_origins = @("chrome-extension://$ExtensionId/")
  } | ConvertTo-Json -Depth 4
  $temporary = $manifestPath + '.' + [Guid]::NewGuid().ToString('N') + '.part'
  try {
    [IO.File]::WriteAllText($temporary, $json, (New-Object Text.UTF8Encoding($false)))
    Move-Item -LiteralPath $temporary -Destination $manifestPath -Force
  } finally {
    if (Test-Path -LiteralPath $temporary) { Remove-Item -LiteralPath $temporary -Force -ErrorAction SilentlyContinue }
  }
}

if ($Unregister -and $Rollback) { Fail '不能同时指定 -Unregister 与 -Rollback。' }

# ── 更新源：默认测试通道（cloudfile-updates-dev）──────────────────────
# 与正式通道同 host，只把路径里的 cloudfile-updates 换成 cloudfile-updates-dev，
# 生产 / 测试靠这一节区分。正式发版时只改这一行（改回 .../cloudfile-updates/update.json）。
# 个别机器解析不了门户域名时，可显式传 IP 备源（内容与域名完全一致）：
#   -UpdateSource http://10.9.8.162:6111/cloudfile-updates-dev/update.json
$DevUpdateSource = 'http://etech.stcetech.ad01.sec.com/cloudfile-updates-dev/update.json'
$usingDevDefaults = -not $UpdateSource
if (-not $UpdateSource) { $UpdateSource = $DevUpdateSource }
# 允许传「基址」（以 / 结尾），省得调用方自己拼文件名。
if ($UpdateSource.EndsWith('/')) { $UpdateSource = $UpdateSource + 'update.json' }

# 未给本地 exe 时：从更新源把 Agent2 现下来。规则与 Agent 自己升级时一致 ——
# 相对 download_url 按清单 URL 解析、强制同源、校验 sha256。双击 install.cmd 走的就是这条。
#
# 下面这几个函数选择 throw 而不是直接 Fail：好让「托盘/任务中心装不上」这种非关键
# 步骤可以只警告、不把整个注册判成失败。
function Get-UpdateManifest([string] $source) {
  $ProgressPreference = 'SilentlyContinue'
  try {
    return Invoke-RestMethod -Uri $source -TimeoutSec 30
  } catch {
    throw ('读取清单失败：' + $source + ' —— ' + $_.Exception.Message)
  }
}

# 把清单里的文件名解析成**同源**绝对地址。跨站一律拒绝：清单被替换时不能把
# 安装指向别的站点。
function Resolve-SameOrigin([string] $source, [string] $name) {
  $base = [Uri] $source
  $target = [Uri]::new($base, $name)
  if ($target.Scheme -ne $base.Scheme -or $target.Host -ne $base.Host) {
    throw ('清单指向了别的站点，拒绝下载：' + $target.AbsoluteUri)
  }
  return $target
}

# 下载一个文件并核对 sha256。
function Get-VerifiedFile([string] $source, [string] $name, [string] $sha256, [string] $destination) {
  $expected = ([string]$sha256).ToLowerInvariant()
  if ($expected -notmatch '^[0-9a-f]{64}$') { throw ('清单里的 sha256 不合法：' + $sha256) }
  $target = Resolve-SameOrigin $source $name
  Write-Note ('下载 ' + $target.AbsoluteUri)
  try {
    Invoke-WebRequest -UseBasicParsing -Uri $target.AbsoluteUri -OutFile $destination -TimeoutSec 300
  } catch {
    throw ('下载失败：' + $target.AbsoluteUri + ' —— ' + $_.Exception.Message)
  }
  $actual = (Get-FileHash -LiteralPath $destination -Algorithm SHA256).Hash.ToLowerInvariant()
  if ($actual -ne $expected) { throw ('下载内容的 sha256 与清单不一致，拒绝安装：' + $actual) }
  return $destination
}

function Get-AgentFromUpdateSource([string] $source) {
  Write-Step ('从更新源获取 Agent2：' + $source)
  Remove-StaleDownloadTemp
  try {
    $manifest = Get-UpdateManifest $source
    $version = [string]$manifest.version
    if ($version -notmatch $VersionPattern) { throw ('更新清单里的 version 不是 Agent2 版本号：' + $version) }
    if (-not $manifest.download_url) { throw '更新清单缺少 download_url，拒绝安装。' }
    $script:DownloadTemp = Join-Path ([IO.Path]::GetTempPath()) ('cf-agent2-install-' + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Force -Path $script:DownloadTemp | Out-Null
    $file = Join-Path $script:DownloadTemp ('cloudfile-local-agent-' + $version + '.exe')
    $null = Get-VerifiedFile $source ([string]$manifest.download_url) ([string]$manifest.sha256) $file
  } catch {
    Fail $_.Exception.Message
  }
  $script:DownloadedVersion = $version
  Write-Note ('sha256 校验通过（版本 ' + $version + '）')
  return $file
}

# 设托盘自启。两条途径都不判注册失败：Agent 才是关键件，托盘可以重跑本脚本补。
#   1) HKCU Run 值：不落文件、卸载干净，首选（域策略锁了 Run 的机器上会失败）；
#   2) 「启动」文件夹快捷方式：锁 Run 的机器上通常还能用，退而求其次。
# 三条参数都带默认值，只为让测试能把它指到临时键/临时快捷方式上。
# 返回实际生效的途径名，全失败返回空串（调用方只警告）。
function Set-Autostart([string] $ui, [string] $key = $RunKey, [string] $value = $RunValue,
                       [string] $linkName = $StartupLink) {
  try {
    New-RegistryKey $key
    Set-ItemProperty -LiteralPath $key -Name $value -Value ('"' + $ui + '"') -Type String
    Write-Note ('已设开机自启（HKCU Run）：' + $value)
    return 'HKCU Run'
  } catch {
    Write-Warn ('写 HKCU Run 失败：' + $_.Exception.Message)
  }
  try {
    $startup = [Environment]::GetFolderPath('Startup')
    if (-not $startup) { throw '取不到「启动」文件夹路径' }
    $link = Join-Path $startup $linkName
    $shell = New-Object -ComObject WScript.Shell
    $shortcut = $shell.CreateShortcut($link)
    $shortcut.TargetPath = $ui
    $shortcut.WorkingDirectory = $binDir
    $shortcut.Description = 'CloudFile 任务中心（托盘）'
    $shortcut.Save()
    Write-Note ('已设开机自启（启动文件夹）：' + $link)
    return 'Startup'
  } catch {
    Write-Warn ('写「启动」文件夹失败：' + $_.Exception.Message)
  }
  Write-Warn '两条自启途径都没写成功（机器上可能锁了自启策略）。'
  Write-Warn ('  不影响注册：重启后手动双击任务中心即可，路径 ' + $ui)
  return ''
}

# TaskCenter 就是托盘 + 任务列表：它是用户唯一的常驻入口。
# 装完**立刻启动**（UI 是 GUI 子系统程序，启动不弹控制台；它自己会进托盘），
# 并设一条 HKCU 自启 —— 否则用户重启后右下角又什么都没有。
# 这一步失败**不判注册失败**：Agent 才是关键件，托盘可以重跑本脚本补。
function Install-TaskCenter([string] $manifestUrl) {
  Write-Step ('获取任务中心（托盘）：' + $manifestUrl)
  try {
    $manifest = Get-UpdateManifest $manifestUrl
    if (-not $manifest.files) { throw 'taskcenter-update.json 里没有 files。' }
    $count = 0
    foreach ($entry in $manifest.files) {
      $name = [string]$entry.name
      # 白名单：只认自己那两个文件名，别让清单往 bin 里写任意路径。
      if ($TaskCenterFiles -notcontains $name) { throw ('清单里有不认识的文件名：' + $name) }
      $destination = Join-Path $binDir $name
      $temporary = $destination + '.part'
      $null = Get-VerifiedFile $manifestUrl $name ([string]$entry.sha256) $temporary
      Move-Item -LiteralPath $temporary -Destination $destination -Force
      $count++
    }
    Write-Note ($count.ToString() + ' 个文件已装到 ' + $binDir)
  } catch {
    Write-Warn ('任务中心没装好（不影响 Agent 注册）：' + $_.Exception.Message)
    Write-Warn ' 稍后重跑本脚本即可补上；不想要托盘时加 -SkipTaskCenter。'
    # 半截文件不要留在 bin 里，免得看起来像装成功了。
    Get-ChildItem -LiteralPath $binDir -Filter '*.part' -ErrorAction SilentlyContinue |
      Remove-Item -Force -ErrorAction SilentlyContinue
    return
  }

  # 自启失败只警告：它跟「启动」是两件事，谁挂了都不该拖累另一个。
  $ui = Join-Path $binDir 'cloudfile-taskcenter-ui.exe'
  $script:Autostart = Set-Autostart $ui

  try {
    Start-Process -FilePath $ui | Out-Null
    Write-Note '任务中心已启动（进托盘、不弹窗口）'
  } catch {
    Write-Warn ('任务中心启动失败（可手动双击它）：' + $_.Exception.Message)
  }
}

# ── 卸载：只移除自己的注册项与 manifest，用户数据全部保留 ───────────────
if ($Unregister) {
  $registered = Get-RegisteredManifest
  if ($registered) {
    if (-not (Test-OwnedManifest $registered)) { Fail ('HKCU 上的 ' + $HostName + ' 不是本安装，拒绝删除：' + $registered) }
    Remove-Item -LiteralPath $RegistryPath -Recurse -Force
    Write-Note '已移除 HKCU 注册项。'
  } else {
    Write-Note 'HKCU 上没有本 Host 的注册项。'
  }
  foreach ($file in @($manifestPath, $previousPath)) {
    if (Test-Path -LiteralPath $file) { Remove-Item -LiteralPath $file -Force -ErrorAction SilentlyContinue }
  }
  # 一并撤掉托盘的自启项：否则下次登录还会起一个指向已卸载安装的进程。
  # 只删自己写的那个值，别碰 Run 键里别人的条目。
  if (Get-ItemProperty -LiteralPath $RunKey -Name $RunValue -ErrorAction SilentlyContinue) {
    Remove-ItemProperty -LiteralPath $RunKey -Name $RunValue -Force
    Write-Note ('已移除开机自启项：' + $RunValue)
  }
  # 备选自启（「启动」文件夹快捷方式）同样要撤，两个途径都可能存在。
  try {
    $linkPath = Join-Path ([Environment]::GetFolderPath('Startup')) $StartupLink
    if (Test-Path -LiteralPath $linkPath) {
      Remove-Item -LiteralPath $linkPath -Force
      Write-Note ('已移除开机自启项：' + $linkPath)
    }
  } catch {
    Write-Warn ('检查「启动」文件夹快捷方式时出错：' + $_.Exception.Message)
  }
  Write-Host '程序、config.json、工作副本与已安装的扩展目录都保留；要彻底清理请手工删除 ' -NoNewline
  Write-Host $root
  exit 0
}

# ── 回滚：恢复上一份 manifest 指向的旧二进制 ───────────────────────────
if ($Rollback) {
  $registered = Get-RegisteredManifest
  if ($registered -and -not (Test-OwnedManifest $registered)) { Fail ('当前注册不是本安装，拒绝回滚：' + $registered) }
  if (-not (Test-OwnedManifest $previousPath)) { Fail ('没有可回滚的上一版注册：' + $previousPath) }
  $previousJson = Get-Content -LiteralPath $previousPath -Raw -Encoding UTF8 | ConvertFrom-Json
  $targetPath = [IO.Path]::GetFullPath([string]$previousJson.path)
  if (-not (Test-Path -LiteralPath $targetPath -PathType Leaf)) { Fail ('上一版二进制已不存在：' + $targetPath) }
  Write-Step ('回滚到 ' + $targetPath)
  $null = Assert-Healthy $targetPath
  Copy-Item -LiteralPath $previousPath -Destination $manifestPath -Force
  New-RegistryKey $RegistryPath
  Set-ItemProperty -LiteralPath $RegistryPath -Name '(default)' -Value $manifestPath
  Write-Note '已回滚。工作副本、配对数据与更新源配置都未改动。'
  exit 0
}

# ── 安装 / 升级 ───────────────────────────────────────────────────────
# 先做占用检查再下载：别下完 7MB 才发现这台机器上已经被别的安装占着。
$registered = Get-RegisteredManifest
if ($registered -and -not (Test-OwnedManifest $registered)) {
  Fail ('HKCU 上的 ' + $HostName + ' 已被别的安装占用，拒绝覆盖：' + $registered)
}
if ($ExtensionId -notmatch '^[a-p]{32}$') { Fail ('扩展 ID 非法（应为 32 位 a-p）：' + $ExtensionId) }

# 取要安装的 exe：给了本地路径就用它（离线/绿色包），否则从更新源现下。
if ($SourceBinary) {
  if (-not [IO.Path]::IsPathRooted($SourceBinary)) { Fail ('exe 必须是绝对路径：' + $SourceBinary) }
  $SourceBinary = [IO.Path]::GetFullPath($SourceBinary)
  if (-not (Test-Path -LiteralPath $SourceBinary -PathType Leaf)) { Fail ('找不到 exe：' + $SourceBinary) }
  $sourceItem = Get-Item -LiteralPath $SourceBinary -Force
  if ($sourceItem.Attributes -band [IO.FileAttributes]::ReparsePoint) { Fail 'exe 不能是符号链接或联接点。' }
} else {
  $SourceBinary = Get-AgentFromUpdateSource $UpdateSource
}

Write-Step '校验源二进制'
$version = (& $SourceBinary --version | Out-String).Trim()
if ($version -notmatch $VersionPattern) { Fail ('--version 返回的不是 Agent2 版本号：' + $version) }
if ($script:DownloadedVersion -and $version -ne $script:DownloadedVersion) {
  Fail ('下载到的二进制自报版本与清单不一致：--version = ' + $version + '，清单 = ' + $script:DownloadedVersion)
}
$null = Assert-Healthy $SourceBinary
Write-Note ('版本 ' + $version + '，健康自检通过')

# 内容寻址：同名即同内容，不同构建互不覆盖，回滚只需换 manifest 里的 path。
$hash = (Get-FileHash -LiteralPath $SourceBinary -Algorithm SHA256).Hash.ToLowerInvariant()
$target = Join-Path $binDir ('cloudfile-agent2-' + $version + '-' + $hash.Substring(0, 16) + '.exe')
New-Item -ItemType Directory -Force -Path $binDir | Out-Null
if (Test-Path -LiteralPath $target) {
  if ((Get-FileHash -LiteralPath $target -Algorithm SHA256).Hash.ToLowerInvariant() -ne $hash) {
    Fail ('目标已存在但内容不一致，拒绝覆盖：' + $target)
  }
  Write-Note ('二进制已存在，跳过拷贝：' + $target)
} else {
  Copy-Item -LiteralPath $SourceBinary -Destination $target
  Write-Note ('已安装：' + $target)
}
$null = Assert-Healthy $target

Write-Step '注册 Native Messaging Host'
# manifest 必须先落地：Agent 的 home.Dir() 靠它定位安装根目录，
# 否则下面 --allow-origin / --set-update-source 会把 config.json 写到回退目录去。
if (Test-OwnedManifest $manifestPath) { Copy-Item -LiteralPath $manifestPath -Destination $previousPath -Force }
Write-Manifest $target
New-RegistryKey $RegistryPath
Set-ItemProperty -LiteralPath $RegistryPath -Name '(default)' -Value $manifestPath
Write-Note ('HKCU 已注册：' + $RegistryPath)
Write-Note ('安装根目录：' + $root)

Write-Step '配置信任站点'
foreach ($origin in $TrustedOrigin) {
  if (-not $origin) { continue }
  & $target --allow-origin $origin
  if ($LASTEXITCODE -ne 0) { Fail ('--allow-origin 失败：' + $origin) }
  Write-Note ('已信任 ' + $origin)
}

Write-Step '写入更新源'
if ($usingDevDefaults) { Write-Warn ('未指定 -UpdateSource，改用测试通道：' + $UpdateSource) }
& $target --set-update-source $UpdateSource
if ($LASTEXITCODE -ne 0) { Fail '--set-update-source 失败。' }
Write-Note $UpdateSource

# ── 顺手把扩展也装上 ──────────────────────────────────────────────────
# 必须在 manifest 与 update_source 都写好之后：--update-extension 用 home.Dir()
# 定位 <root>\extension，并读 config.json 里的 update_source 去 sibling 的
# extension-update.json。装不上不算安装失败（离线机器仍可手动解压 zip）。
$extensionDir = Join-Path $root 'extension'
Write-Step '获取扩展包'
& $target --update-extension
if ($LASTEXITCODE -ne 0) {
  Write-Warn ('扩展包没能自动装好（不影响 Agent 注册）。稍后可在扩展里点「立即升级扩展」，' +
              '或手动把扩展 zip 解压到：' + $extensionDir)
}

# ── 任务中心（托盘 + 任务列表）────────────────────────────────────────
# 它跟 Agent/扩展同一个基址，所以直接用 update_source 推出目录来。
$updateBase = $UpdateSource.Substring(0, $UpdateSource.LastIndexOf('/') + 1)
if ($SkipTaskCenter) {
  Write-Warn '按 -SkipTaskCenter 跳过托盘/任务中心：右下角不会有图标。'
} else {
  Install-TaskCenter ($updateBase + 'taskcenter-update.json')
}

# ── 收尾引导：扩展仍然要手动「加载已解压」一次 ────────────────────────
Remove-DownloadTemp
if (-not $Silent) {
  try {
    Set-Clipboard -Value $extensionDir -ErrorAction Stop
    Write-Note ('扩展目录已复制到剪贴板：' + $extensionDir)
  } catch {
    Write-Warn ('无法写入剪贴板，请手动复制这一行：' + $extensionDir)
  }
}
Write-Host ''
Write-Host '======================================================' -ForegroundColor Cyan
Write-Host ' 还差最后一步启用扩展（每台机器只需一次）：' -ForegroundColor Cyan
Write-Host '  1. 打开扩展管理页：Chrome 右上角菜单 -^>「扩展程序」-^>「管理扩展程序」' -ForegroundColor Cyan
Write-Host '     （或直接在地址栏输入 chrome://extensions 回车）' -ForegroundColor Cyan
Write-Host '  2. 打开右上角「开发者模式」' -ForegroundColor Cyan
Write-Host '  3. 点「加载已解压的扩展程序」' -ForegroundColor Cyan
if ($Silent) {
  Write-Host ('  4. 选择这个目录：' + $extensionDir) -ForegroundColor Cyan
} else {
  Write-Host '  4. 在文件夹窗口按 Ctrl+L、Ctrl+V（路径已在剪贴板里），回车，点「选择文件夹」' -ForegroundColor Cyan
}
Write-Host '======================================================' -ForegroundColor Cyan
if (-not $SkipTaskCenter) {
  Write-Host '右下角应已出现 CloudFile 托盘图标：' -NoNewline
  Write-Host (Join-Path $binDir 'cloudfile-taskcenter-ui.exe')
  Write-Host '  双击它打开任务中心；本地查看/编辑时会在那里弹提示。'
  if ($script:Autostart) {
    Write-Host ('  开机自启已设好（' + $script:Autostart + '），重启后托盘会自己起来。')
  } else {
    Write-Warn '开机自启没设上，重启后需要手动双击任务中心。'
  }
}
Write-Host ''
Write-Host ('扩展 ID 应为：' + $ExtensionId)
if (-not (Test-Path -LiteralPath (Join-Path $extensionDir 'manifest.json'))) {
  Write-Warn ('扩展目录里还没有 manifest.json —— 首次安装请先把扩展包解压到 ' + $extensionDir)
}
