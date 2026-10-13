@echo off
REM 跨平台启动脚本 - Windows
REM 双击运行或在 cmd/PowerShell 中执行

setlocal enabledelayedexpansion

echo === Pilot 开发环境启动脚本 ===
echo.

REM 获取脚本所在目录的父目录（项目根目录）
set "SCRIPT_DIR=%~dp0"
set "PROJECT_ROOT=%SCRIPT_DIR%.."

REM 切换到项目根目录
cd /d "%PROJECT_ROOT%"

REM 检查 Docker 是否运行
docker info >nul 2>&1
if errorlevel 1 (
    echo 错误: Docker 未运行，请先启动 Docker Desktop
    pause
    exit /b 1
)

REM 检查 Go 是否安装
where go >nul 2>&1
if errorlevel 1 (
    echo 错误: Go 未安装，请先安装 Go 1.24+
    echo 下载地址: https://go.dev/dl/
    pause
    exit /b 1
)

if not defined PILOT_CONFIG set "PILOT_CONFIG=%cd%\manifest\config\config.yaml"

echo 步骤 1/3: 启动 Docker 依赖服务...
if not exist "%cd%\.env" (
    echo 错误: 缺少 .env，请先 copy .env.example .env 并设置 MYSQL_ROOT_PASSWORD
    pause
    exit /b 1
)
docker compose --env-file "%cd%\.env" -f manifest/docker/docker-compose.yml up -d
if errorlevel 1 (
    echo 错误: Docker 依赖服务启动失败
    pause
    exit /b 1
)

echo.
echo 步骤 2/3: 等待真实依赖服务就绪...
set /a ATTEMPT=1
:wait_deps
go run .\cmd\checkdeps\main.go >nul 2>&1
if not errorlevel 1 goto deps_ready
if !ATTEMPT! GEQ 30 (
    echo 错误: 依赖服务未在预期时间内就绪
    go run .\cmd\checkdeps\main.go
    pause
    exit /b 1
)
echo 等待依赖服务 (!ATTEMPT!/30)...
timeout /t 2 /nobreak >nul
set /a ATTEMPT+=1
goto wait_deps

:deps_ready

echo.
echo 步骤 3/3: 启动 Pilot 服务...
go run .\cmd\main.go %*

pause
