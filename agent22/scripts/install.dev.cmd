@echo off
setlocal enableextensions
title CloudFile Agent 2.2 安装（测试通道）

rem ---------------------------------------------------------------
rem  双击运行即可。本脚本只做一件事：拿到引导体，然后交给它。
rem    1) 同目录有 cloudfile-install.exe 就直接用（推荐：离线也能装）；
rem    2) 没有就从通道下载：主源是门户域名，备用源是直连静态服务 IP。
rem  验签、下载 Agent、注册 Native Host、写自启、启动桌面端全在引导体里，
rem  它内置钉住的发布公钥 —— 通道只能提供文件，不能决定「谁签发」。
rem
rem  正式与测试只差下面 CHANNEL 一行，二进制是同一份：
rem    正式 cloudfile-updates      -> install.cmd
rem    测试 cloudfile-updates-dev  -> install.dev.cmd
rem
rem  安装目录：%CLOUDFILE_AGENT2_HOME% 优先，否则 %LOCALAPPDATA%\CloudFileAgent22。
rem  自动化调用时设 CLOUDFILE_NO_PAUSE=1 可跳过结尾的暂停。
rem  注意：echo 的文本里不要出现 > < & |，cmd 会把它们当重定向或管道。
rem ---------------------------------------------------------------

set "CHANNEL=cloudfile-updates-dev"

set "HOST_PRIMARY=etech.stcetech.ad01.sec.com"
set "HOST_MIRROR=10.9.8.162:6111"
set "BASE_PRIMARY=http://%HOST_PRIMARY%/%CHANNEL%"
set "BASE_MIRROR=http://%HOST_MIRROR%/%CHANNEL%"

set "INSTALLER=cloudfile-install.exe"
set "WORKDIR=%LOCALAPPDATA%\CloudFileAgent22"
if not "%CLOUDFILE_AGENT2_HOME%"=="" set "WORKDIR=%CLOUDFILE_AGENT2_HOME%"
if not exist "%WORKDIR%" mkdir "%WORKDIR%" >nul 2>&1

set "TARGET=%~dp0%INSTALLER%"
if exist "%TARGET%" goto run
set "TARGET=%WORKDIR%\%INSTALLER%"
if exist "%TARGET%" goto run

echo 正在获取安装引导体...
call :fetch "%BASE_PRIMARY%/%INSTALLER%"
if not errorlevel 1 goto run
call :fetch "%BASE_MIRROR%/%INSTALLER%"
if not errorlevel 1 goto run

echo.
echo [失败] 两个地址都没能下载 %INSTALLER%。
echo        请检查本机能否访问下面的地址（DNS / 代理 / 防火墙）：
echo          %BASE_PRIMARY%
echo          %BASE_MIRROR%
echo        也可以把 %INSTALLER% 与本脚本放在同一目录后重试。
set "CODE=1"
goto end

:run
echo 通道：%BASE_PRIMARY%
"%TARGET%" --source "%BASE_PRIMARY%" --source-fallback "%BASE_MIRROR%" %*
set "CODE=%ERRORLEVEL%"
goto end

:fetch
rem 下载到 %TARGET%，并做一次「体积像不像一个 exe」的粗检：
rem 断流、代理返回的错误页都会明显偏小，别把它当真值去执行。
powershell -NoProfile -ExecutionPolicy Bypass -Command "$ProgressPreference='SilentlyContinue'; try { Invoke-WebRequest -UseBasicParsing -TimeoutSec 60 -Uri '%~1' -OutFile '%TARGET%' -ErrorAction Stop } catch { exit 1 }" >nul 2>&1
if errorlevel 1 exit /b 1
if not exist "%TARGET%" exit /b 1
set "SIZE="
for %%F in ("%TARGET%") do set "SIZE=%%~zF"
if not defined SIZE exit /b 1
if %SIZE% LSS 1000000 (
  del /q "%TARGET%" >nul 2>&1
  exit /b 1
)
exit /b 0

:end
if not defined CODE set "CODE=1"
echo.
echo 退出码：%CODE%
if not "%CLOUDFILE_NO_PAUSE%"=="" exit /b %CODE%
pause
exit /b %CODE%
