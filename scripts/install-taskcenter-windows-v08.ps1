# CloudFile v0.8 per-user TaskCenter development-preview installer.
# Never replaces the Chrome Native Host or v0.7 RC.
[CmdletBinding()]
param(
    [ValidateSet('Install','Status','Uninstall')][string]$Action = 'Status',
    [string]$PackageDirectory = '',
    [switch]$StartNow
)
$ErrorActionPreference = 'Stop'
$installDir = Join-Path $env:LOCALAPPDATA 'CloudFile\TaskCenterPreview\v0.8'
$runKey = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run'
$runName = 'CloudFileTaskCenterV08Preview'
$uiExe = Join-Path $installDir 'CloudFile.TaskCenter.Preview.exe'
$goExe = Join-Path $installDir 'cloudfile-taskcenter.exe'
$runCommand = '"' + $uiExe + '" --taskcenter-bin "' + $goExe + '"'

if ($Action -eq 'Install') {
    if ([string]::IsNullOrWhiteSpace($PackageDirectory)) {
        throw 'Explicit development preview package directory required.'
    }
    $source = [System.IO.Path]::GetFullPath($PackageDirectory)
    $sourceUi = Join-Path $source 'CloudFile.TaskCenter.Preview.exe'
    $sourceGo = Join-Path $source 'cloudfile-taskcenter.exe'
    if (-not (Test-Path -LiteralPath $sourceUi -PathType Leaf) -or
        -not (Test-Path -LiteralPath $sourceGo -PathType Leaf)) {
        throw 'Complete locally built v0.8 preview binaries required.'
    }
    $previous = (Get-ItemProperty -Path $runKey -Name $runName -ErrorAction SilentlyContinue).$runName
    if ($previous -and $previous -ne $runCommand) {
        throw 'Existing TaskCenter preview startup registration differs. Do not overwrite it.'
    }
    New-Item -ItemType Directory -Path $installDir -Force | Out-Null
    Copy-Item -LiteralPath $sourceUi -Destination $uiExe -Force
    Copy-Item -LiteralPath $sourceGo -Destination $goExe -Force
    New-Item -Path $runKey -Force | Out-Null
    New-ItemProperty -Path $runKey -Name $runName -PropertyType String -Value $runCommand -Force | Out-Null
    if ($StartNow) {
        Start-Process -FilePath $uiExe -ArgumentList @('--taskcenter-bin', ('"' + $goExe + '"'))
    }
    Write-Output 'v0.8 current-user preview startup installed; no Host/Checkout changes.'
}
elseif ($Action -eq 'Uninstall') {
    Remove-ItemProperty -Path $runKey -Name $runName -ErrorAction SilentlyContinue
    # Never delete CloudFile task-index, user edited copies or v0.7 journals.
    Remove-Item -LiteralPath $uiExe -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $goExe -Force -ErrorAction SilentlyContinue
    Write-Output 'Only preview executables and startup registration removed. User data retained.'
}
else {
    $registered = (Get-ItemProperty -Path $runKey -Name $runName -ErrorAction SilentlyContinue).$runName
    Write-Output ('Per-user startup: ' + [bool]($registered -eq $runCommand))
    Write-Output ('Preview binaries: ' + [bool]((Test-Path -LiteralPath $uiExe -PathType Leaf) -and
                 (Test-Path -LiteralPath $goExe -PathType Leaf)))
}
