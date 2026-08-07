param(
  [Parameter(Mandatory = $true)][string]$ExtensionId,
  [Parameter(Mandatory = $true)][string]$ServerOrigin,
  [string]$BinaryPath = (Join-Path $PSScriptRoot "..\cloudfile-local-agent.exe")
)

$root = Join-Path $env:LOCALAPPDATA "CloudFileLocal"
$manifest = Join-Path $root "com.cloudfile.local_agent.json"
New-Item -ItemType Directory -Force -Path $root | Out-Null

& $BinaryPath --allow-origin $ServerOrigin
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
