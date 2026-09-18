param(
  [Parameter(Mandatory = $true)][string]$ExtensionId,
  [Parameter(Mandatory = $true)][string]$ServerOrigin,
  [string]$UpdateSource = "",
  [string]$BinaryPath = ""
)

# 查找脚本所在目录（$PSScriptRoot）与上一层目录（$PSScriptRoot\..）下的
# cloudfile-local-agent 版本号化二进制（cloudfile-local-agent-<x.y.z>.exe）。
function Find-AgentBinary([string]$dir) {
  if (-not $dir) { return $null }
  $versioned = Get-ChildItem -LiteralPath $dir -Filter "cloudfile-local-agent-*.exe" -File -ErrorAction SilentlyContinue |
    Sort-Object Name -Descending | Select-Object -First 1
  if ($versioned) { return (Resolve-Path -LiteralPath $versioned.FullName).Path }
  return $null
}

if ($BinaryPath -eq "") {
  $BinaryPath = Find-AgentBinary $PSScriptRoot
  if (-not $BinaryPath) { $BinaryPath = Find-AgentBinary (Join-Path $PSScriptRoot "..") }
  if (-not $BinaryPath) {
    Write-Error "未找到 cloudfile-local-agent-<version>.exe：脚本目录($PSScriptRoot)与上一层目录均不存在该文件，请用 -BinaryPath 指定。"
    exit 1
  }
} elseif (-not (Test-Path -LiteralPath $BinaryPath)) {
  Write-Error "指定的 -BinaryPath 不存在：$BinaryPath"
  exit 1
}

$root = Join-Path $env:LOCALAPPDATA "CloudFileLocal"
$manifest = Join-Path $root "com.cloudfile.local_agent.json"
New-Item -ItemType Directory -Force -Path $root | Out-Null

& $BinaryPath --allow-origin $ServerOrigin
if ($LASTEXITCODE -ne 0) {
  Write-Error "执行 --allow-origin 失败：$BinaryPath"
  exit 1
}

if ($UpdateSource -ne "") {
  & $BinaryPath --set-update-source $UpdateSource
  if ($LASTEXITCODE -ne 0) {
    Write-Error "执行 --set-update-source 失败：$BinaryPath"
    exit 1
  }
}

@{
  name = "com.cloudfile.local_agent"
  description = "CloudFile Local Agent"
  path = $BinaryPath
  type = "stdio"
  allowed_origins = @("chrome-extension://$ExtensionId/")
} | ConvertTo-Json | Set-Content -Encoding utf8 -NoNewline $manifest

New-Item -Path "HKCU:\Software\Google\Chrome\NativeMessagingHosts\com.cloudfile.local_agent" -Force | Out-Null
Set-ItemProperty -Path "HKCU:\Software\Google\Chrome\NativeMessagingHosts\com.cloudfile.local_agent" -Name '(default)' -Value $manifest
Write-Host "Registered CloudFile Local Agent for Chrome extension $ExtensionId"
Write-Host "Binary: $BinaryPath"
