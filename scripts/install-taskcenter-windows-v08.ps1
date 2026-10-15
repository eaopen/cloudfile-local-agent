# CloudFile v0.8 optional current-user integration.
# GREEN RUN requires neither PowerShell nor this script: double-click the
# portable start-taskcenter-windows-v08.cmd in the unpacked package.
# This script never requests elevation, never touches HKLM, services or the
# Chrome Native Host, and never removes task indexes, edits or v0.7 journals.
[CmdletBinding()]
param(
    [ValidateSet('Install','Start','Status','Uninstall')][string]$Action = 'Status',
    [string]$PackageDirectory = '',
    [string]$GoCoreBinary = '',
    [switch]$EnableAutoStart,
    [switch]$StartNow
)
$ErrorActionPreference = 'Stop'
$installDir = Join-Path $env:LOCALAPPDATA 'CloudFile\TaskCenterPreview\v0.8'
$runKey = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run'
$runName = 'CloudFileTaskCenterV08Preview'
$uiExe = Join-Path $installDir 'CloudFile.TaskCenter.Preview.exe'
$goExe = Join-Path $installDir 'cloudfile-taskcenter.exe'
$runner = Join-Path $installDir 'start-taskcenter-windows-v08.cmd'

function Get-PortablePackage([string]$folder) {
    if ([string]::IsNullOrWhiteSpace($folder)) {
        throw 'Specify -PackageDirectory pointing to the unpacked portable preview.'
    }
    $dir = [IO.Path]::GetFullPath($folder)
    $exe = Join-Path $dir 'CloudFile.TaskCenter.Preview.exe'
    $core = Join-Path $dir 'cloudfile-taskcenter.exe'
    if (-not (Test-Path -LiteralPath $exe -PathType Leaf) -or
        -not (Test-Path -LiteralPath $core -PathType Leaf)) {
        throw 'Portable preview requires both the WinForms UI and Go TaskCenter binaries.'
    }
    return @{ Directory=$dir; UI=$exe; Core=$core }
}

function Get-CoreArgs {
    if (-not $GoCoreBinary) { return @() }
    if (-not [IO.Path]::IsPathFullyQualified($GoCoreBinary) -or
        -not (Test-Path -LiteralPath $GoCoreBinary -PathType Leaf)) {
        throw 'Optional signed renewal needs an existing absolute path to the original Go Core.'
    }
    return @('--go-core-bin', ('"' + $GoCoreBinary + '"'))
}

function Start-Portable([string]$exe, [string]$core) {
    $args = @('--taskcenter-bin', ('"' + $core + '"'))
    $args += @(Get-CoreArgs)
    # Ordinary user-launched WinForms process, not an installer/service.
    Start-Process -FilePath $exe -ArgumentList $args -WorkingDirectory (Split-Path -Parent $exe)
}

function Get-OwnedAutoStart {
    try {
        $value = (Get-ItemProperty -Path $runKey -Name $runName -ErrorAction Stop).$runName
        return [string]$value
    }
    catch [System.Management.Automation.ItemNotFoundException] { return '' }
    catch [System.Management.Automation.PSArgumentException] { return '' }
    catch {
        Write-Warning 'Cannot read current-user startup settings. Portable run still works.'
        return ''
    }
}

$runBase = '"' + $uiExe + '" --taskcenter-bin "' + $goExe + '"'
switch ($Action) {
    'Start' {
        $portable = Get-PortablePackage $PackageDirectory
        Start-Portable $portable.UI $portable.Core
        Write-Output 'Started CloudFile TaskCenter directly from its portable folder.'
        break
    }
    'Install' {
        $portable = Get-PortablePackage $PackageDirectory
        $args = @(Get-CoreArgs) # validate before modifying any current-user files
        try {
            New-Item -ItemType Directory -Path $installDir -Force -ErrorAction Stop | Out-Null
            foreach ($name in @('CloudFile.TaskCenter.Preview.exe', 'cloudfile-taskcenter.exe',
                                'start-taskcenter-windows-v08.cmd')) {
                $sourceFile = Join-Path $portable.Directory $name
                if (Test-Path -LiteralPath $sourceFile -PathType Leaf) {
                    $destination = Join-Path $installDir $name
                    if (-not [string]::Equals($sourceFile,$destination,
                                              [StringComparison]::OrdinalIgnoreCase)) {
                        Copy-Item -LiteralPath $sourceFile -Destination $destination -Force -ErrorAction Stop
                    }
                }
            }
        } catch {
            Write-Warning 'Current-user file deployment was blocked. No admin install is needed; run start-taskcenter-windows-v08.cmd directly from the portable folder.'
            if ($StartNow) { Start-Portable $portable.UI $portable.Core }
            break
        }

        if ($EnableAutoStart) {
            $runCommand = $runBase
            if ($GoCoreBinary) { $runCommand += ' --go-core-bin "' + $GoCoreBinary + '"' }
            try {
                $previous = Get-OwnedAutoStart
                if ($previous -and $previous -ne $runCommand) {
                    Write-Warning 'Different current-user startup entry exists; leaving it unchanged.'
                } else {
                    # 不要用 New-Item -Force：PS 5.1 在注册表上把 -Force 当「删掉重建」，
                    # 而 CurrentVersion\Run 是 Windows 保护的键（禁止删除），会报
                    # UnauthorizedAccessException。键在就只写值。
                    if (-not (Test-Path -LiteralPath $runKey)) { New-Item -Path $runKey -Force -ErrorAction Stop | Out-Null }
                    New-ItemProperty -Path $runKey -Name $runName -PropertyType String -Value $runCommand -Force -ErrorAction Stop | Out-Null
                    Write-Output 'Optional current-user autostart enabled.'
                }
            } catch {
                Write-Warning 'Autostart blocked by policy; TaskCenter remains installed and can start manually.'
            }
        } else {
            Write-Output 'Autostart not requested. No registry write was attempted.'
        }
        if ($StartNow) { Start-Portable $uiExe $goExe }
        Write-Output 'CloudFile current-user preview available; elevation and OS installation are not required.'
        break
    }
    'Uninstall' {
        $existing = Get-OwnedAutoStart
        if ($existing) {
            if ($existing.StartsWith($runBase,[StringComparison]::OrdinalIgnoreCase)) {
                try { Remove-ItemProperty -Path $runKey -Name $runName -ErrorAction Stop }
                catch { Write-Warning 'Startup removal blocked; no admin elevation attempted.' }
            } else {
                Write-Warning 'Unknown startup entry left unchanged.'
            }
        }
        foreach ($item in @($uiExe, $goExe, $runner)) {
            try { Remove-Item -LiteralPath $item -Force -ErrorAction SilentlyContinue }
            catch { Write-Warning 'Preview file is in use or protected; remove it manually if needed.' }
        }
        Write-Output 'Only optional integration removed. User edits, task indexes and journals retained.'
        break
    }
    'Status' {
        $existing = Get-OwnedAutoStart
        Write-Output ('Portable mode available without installer or admin: yes')
        Write-Output ('Current-user copy: ' + [bool]((Test-Path $uiExe -PathType Leaf) -and
                                                     (Test-Path $goExe -PathType Leaf)))
        Write-Output ('Optional autostart: ' + [bool]($existing -and
                      $existing.StartsWith($runBase,[StringComparison]::OrdinalIgnoreCase)))
        break
    }
}
