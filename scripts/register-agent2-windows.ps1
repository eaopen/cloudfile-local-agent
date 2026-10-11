# Agent2 2.0.1 Windows Go capability component, installed per-user.
# Does NOT replace com.cloudfile.current_agent (Windows Agent1/.NET 1.x).
param(
  [string]$BinaryPath = '',
  [string]$ExtensionId = 'gocnpbhnbikcpnlafmodflddgnmfcjoc',
  [string]$InstallDir = '',
  [switch]$Unregister
)
$ErrorActionPreference = 'Stop'
$hostName = 'com.cloudfile.agent2'
if ($ExtensionId -cnotmatch '^[a-p]{32}$') { throw 'Invalid Chrome extension ID' }
$root = if ($InstallDir) { $InstallDir } else { Join-Path $env:LOCALAPPDATA 'CloudFileAgent2' }
$root = [System.IO.Path]::GetFullPath($root)
$manifest = Join-Path $root "$hostName.json"
$registry = "HKCU:\Software\Google\Chrome\NativeMessagingHosts\$hostName"
if ($Unregister) {
  if (Test-Path $registry) {
    $registered = (Get-Item $registry).GetValue('')
    if ($registered -ne $manifest) { throw 'A different Agent2 host is registered; refusing removal' }
    Remove-Item -LiteralPath $registry -Force
  }
  if (Test-Path -LiteralPath $manifest) { Remove-Item -LiteralPath $manifest -Force }
  return
}
if (-not $BinaryPath -or -not (Test-Path -LiteralPath $BinaryPath -PathType Leaf)) { throw 'Agent2 executable not found' }
$binary = (Resolve-Path -LiteralPath $BinaryPath).Path
$version = (& $binary --version | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or $version -ne '2.0.1') {
  throw "Expected Agent2 Go 2.0.1, got '$version'"
}
$binDir = Join-Path $root 'bin'
New-Item -ItemType Directory -Path $binDir -Force | Out-Null
# A 2.0.1 development build may change without bumping the public Windows
# 1.2.1 version. Keep hash-addressed binaries immutable so live processes are
# never overwritten and the previous registered version can be restored.
$sourceHash = (Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash.ToLowerInvariant()
$target = Join-Path $binDir ('cloudfile-agent2-2.0.1-' + $sourceHash.Substring(0, 12) + '.exe')
if ($binary -ne $target) {
  if (Test-Path -LiteralPath $target) {
    $destHash = (Get-FileHash -LiteralPath $target -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($sourceHash -ne $destHash) { throw 'An existing immutable Agent2 binary differs; refusing overwrite' }
  } else {
    Copy-Item -LiteralPath $binary -Destination $target -ErrorAction Stop
  }
}
$nativeManifest = @{
  name = $hostName
  description = 'CloudFile Agent2 Go capability component (Windows)'
  path = $target
  type = 'stdio'
  allowed_origins = @("chrome-extension://$ExtensionId/")
}
$tmp = Join-Path $root ('.native-host-' + [Guid]::NewGuid().ToString('N') + '.json')
try {
  $json = $nativeManifest | ConvertTo-Json -Depth 3
  [System.IO.File]::WriteAllText($tmp, $json, [System.Text.UTF8Encoding]::new($false))
  Move-Item -LiteralPath $tmp -Destination $manifest -Force
} finally {
  if (Test-Path -LiteralPath $tmp) { Remove-Item -LiteralPath $tmp -Force }
}
New-Item -Path $registry -Force | Out-Null
(Get-Item $registry).SetValue('', $manifest)
Write-Host "Agent2 Windows Go capability host registered for this user: $manifest"
Write-Host 'Agent1 (.NET) Chrome registration was not changed.'
