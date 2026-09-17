param(
  [Parameter(Mandatory = $true)][string]$ExtensionId,
  [Parameter(Mandatory = $true)][string]$ServerOrigin,
  [string]$BinaryPath = ""
)

# 优先查找脚本所在目录（$PSScriptRoot）下的 cloudfile-local-agent.exe，
# 找不到再找上一层目录（$PSScriptRoot\..）下的。
if ($BinaryPath -eq "") {
  $local = Join-Path $PSScriptRoot "cloudfile-local-agent.exe"
  $parent = Join-Path $PSScriptRoot "..\cloudfile-local-agent.exe"
  if (Test-Path -LiteralPath $local) {
    $BinaryPath = (Resolve-Path -LiteralPath $local).Path
  } elseif (Test-Path -LiteralPath $parent) {
    $BinaryPath = (Resolve-Path -LiteralPath $parent).Path
  } else {
    Write-Error "未找到 cloudfile-local-agent.exe：脚本目录($PSScriptRoot)与上一层目录均不存在该文件，请用 -BinaryPath 指定。"
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
