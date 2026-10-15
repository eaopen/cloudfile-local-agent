@echo off
setlocal enableextensions
title CloudFile 本地组件安装（测试通道）

rem ---------------------------------------------------------------
rem  双击运行即可。本脚本只做两件事：
rem    1) 从通道下载 register-agent2-windows.ps1；
rem    2) 调用它 —— 不给 exe 参数，表示由它从更新源现下 Agent 并校验 sha256。
rem  下载 Agent、注册 Native Host、获取扩展包都在那个 ps1 里，逻辑只有一份实现。
rem
rem  通道：正式 cloudfile-updates / 测试 cloudfile-updates-dev。
rem  整份文件只有下面 CHANNEL 一行区分生产与测试，正式发版时改它即可。
rem  下载有主备两个地址（内容完全一致）：主源是门户域名，备用源是直连静态
rem  服务 IP。有些机器解析不了、或访问不了门户域名，会自动改用备用地址，
rem  并把「实际可用的那个」传给注册脚本写进 Agent 的 update_source。
rem
rem  安装目录：%CLOUDFILE_AGENT2_HOME% 优先，否则 %LOCALAPPDATA%\CloudFileAgent2
rem  （与注册脚本、Agent 的 internal/home 约定一致）。
rem
rem  注意：echo 的文本里不要出现 > < & |，cmd 会把它们当重定向或管道。
rem ---------------------------------------------------------------

rem ── 通道：换通道只改这一行 ──────────────────────────────────────
set "CHANNEL=cloudfile-updates-dev"

set "HOST_PRIMARY=etech.stcetech.ad01.sec.com"
set "HOST_MIRROR=10.9.8.162:6111"
set "BASE_PRIMARY=http://%HOST_PRIMARY%/%CHANNEL%/"
set "BASE_MIRROR=http://%HOST_MIRROR%/%CHANNEL%/"

set "PS1_NAME=register-agent2-windows.ps1"
set "OKBASE="
set "CUSTOMURL="

set "WORKDIR=%LOCALAPPDATA%\CloudFileAgent2"
if not "%CLOUDFILE_AGENT2_HOME%"=="" set "WORKDIR=%CLOUDFILE_AGENT2_HOME%"

rem 测试或镜像站用：CLOUDFILE_INSTALL_BASE 覆盖上面两个基址（末尾要带斜杠）。
if not "%CLOUDFILE_INSTALL_BASE%"=="" (
  set "BASE_PRIMARY=%CLOUDFILE_INSTALL_BASE%"
  set "BASE_MIRROR=%CLOUDFILE_INSTALL_BASE%"
)

call :usable "%WORKDIR%"
if errorlevel 1 (
  echo.
  echo [错误] 无法创建或写入安装目录：%WORKDIR%
  goto fail
)

echo ============================================================
echo   CloudFile 本地组件安装
echo   通道    : %CHANNEL%
echo   安装目录: %WORKDIR%
echo ============================================================
echo.

echo [1/2] 下载安装脚本 %PS1_NAME%
if not "%CLOUDFILE_PS1_URL%"=="" (
  echo   使用 CLOUDFILE_PS1_URL 指定的地址（不兜底：指错了就该报错）
  set "CUSTOMURL=%CLOUDFILE_PS1_URL%"
  call :fetch1 "%CLOUDFILE_PS1_URL%"
  goto fetched
)

call :fetch1 "%BASE_PRIMARY%%PS1_NAME%"
if not errorlevel 1 (
  set "OKBASE=%BASE_PRIMARY%"
  goto fetched
)

echo   主源不可用，改用备用地址（直连静态服务）
call :fetch1 "%BASE_MIRROR%%PS1_NAME%"
if not errorlevel 1 set "OKBASE=%BASE_MIRROR%"

:fetched
call :ps1ok
if errorlevel 1 (
  echo.
  echo [错误] 下载 %PS1_NAME% 失败。
  if defined CUSTOMURL echo        试过：%CUSTOMURL%
  if not defined CUSTOMURL echo        试过：%BASE_PRIMARY%
  if not defined CUSTOMURL echo        试过：%BASE_MIRROR%
  echo        请检查本机能否访问上面的地址（DNS/代理/防火墙），然后重新双击本脚本。
  echo        两个地址都不通时，请把本窗口内容发给管理员。
  goto fail
)

echo.
echo [2/2] 开始安装：下载 Agent、校验 sha256、注册 Host、获取扩展包
echo       请勿关闭本窗口
echo.
rem 把实际可用的基址传下去：注册脚本就不用再猜，而且写进 update_source 的
rem 正是这台机器能访问的那个地址（末尾带斜杠，脚本会自己补 update.json）。
if defined OKBASE (
  powershell -NoProfile -ExecutionPolicy Bypass -File "%WORKDIR%\%PS1_NAME%" -InstallDir "%WORKDIR%" -UpdateSource "%OKBASE%"
) else (
  powershell -NoProfile -ExecutionPolicy Bypass -File "%WORKDIR%\%PS1_NAME%" -InstallDir "%WORKDIR%"
)
if errorlevel 1 (
  echo.
  echo [错误] 注册脚本返回失败，请把上面完整输出发给管理员。
  goto fail
)

if not exist "%WORKDIR%\com.cloudfile.agent2.json" (
  echo.
  echo [警告] 没有找到 %WORKDIR%\com.cloudfile.agent2.json，注册可能没完成。
  goto fail
)

echo.
echo ------------------------------------------------------------
echo  完成。最后一步（每台机器只需一次）按上面提示做：
echo    打开扩展管理页 -^> 打开「开发者模式」-^> 「加载已解压的扩展程序」
echo    选择目录：%WORKDIR%\extension
echo  以后升级点扩展图标里的按钮即可，不用再跑本脚本。
echo ------------------------------------------------------------
goto done

:fail
echo.
pause
exit /b 1

:done
echo.
pause
exit /b 0

rem 下载 %1 到 %WORKDIR%\%PS1_NAME%：先 PowerShell，再 curl。成功 exit /b 0。
rem 每次都先删旧的：否则下载失败时会把上一次留下的旧脚本当成本次结果执行。
:fetch1
set "TRYURL=%~1"
del "%WORKDIR%\%PS1_NAME%" >nul 2>nul
echo   尝试 %TRYURL%
rem 这里刻意吞掉 PowerShell 与 curl 的原始报错：主备两个地址都会试，
rem 把 "Unable to connect to the remote server" 那种红字刷一屏反而吓人，
rem 失败原因在 :fetched 处统一给一句人话。
powershell -NoProfile -ExecutionPolicy Bypass -Command "Invoke-WebRequest -UseBasicParsing -Uri '%TRYURL%' -OutFile '%WORKDIR%\%PS1_NAME%' -TimeoutSec 30" >nul 2>nul
call :ps1ok
if not errorlevel 1 exit /b 0
echo   PowerShell 下载失败，改用 curl 再试
curl.exe -fsSL --connect-timeout 8 --max-time 60 -o "%WORKDIR%\%PS1_NAME%" "%TRYURL%" >nul 2>nul
call :ps1ok
if not errorlevel 1 exit /b 0
exit /b 1

rem 下到的注册脚本是否可用：存在且大于 2KB。
rem 小于 2KB 一定是没下全（断流、或代理返回了错误页），当成成功会去执行坏脚本。
:ps1ok
if not exist "%WORKDIR%\%PS1_NAME%" exit /b 1
for %%A in ("%WORKDIR%\%PS1_NAME%") do if %%~zA LSS 2048 exit /b 1
exit /b 0

rem 目录能否真正写入：建目录 + 写一个探针文件。
:usable
if not exist "%~1" mkdir "%~1" 2>nul
if not exist "%~1" exit /b 1
echo ok> "%~1\.cf-write-test" 2>nul
if errorlevel 1 exit /b 1
del "%~1\.cf-write-test" >nul 2>nul
exit /b 0
