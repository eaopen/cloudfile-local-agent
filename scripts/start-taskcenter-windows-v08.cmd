@echo off
setlocal
REM CloudFile v0.8 portable Windows TaskCenter. Requires no admin,
REM installer, PowerShell execution policy, HKCU registration or autostart.
set "HERE=%~dp0"
set "UI=%HERE%CloudFile.TaskCenter.Preview.exe"
set "TASKCORE=%HERE%cloudfile-taskcenter.exe"
if not exist "%UI%" (
  echo CloudFile TaskCenter UI missing from this portable folder.
  exit /b 2
)
if not exist "%TASKCORE%" (
  echo CloudFile TaskCenter core missing from this portable folder.
  exit /b 2
)
REM UI has its own per-user singleton; no second Chrome Native Host.
set "SIGNEDCORE=%HERE%CloudFile.Agent2.Go.exe"
if exist "%SIGNEDCORE%" (
    start "" "%UI%" --taskcenter-bin "%TASKCORE%" --go-core-bin "%SIGNEDCORE%" %*
) else (
    start "" "%UI%" --taskcenter-bin "%TASKCORE%" %*
)
if errorlevel 1 exit /b 1
exit /b 0
