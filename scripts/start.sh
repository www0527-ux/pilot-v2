#!/bin/bash
# 跨平台启动脚本 - macOS / Linux
# Windows 用户请使用 start.bat；Git Bash/WSL 可使用 make start。

set -e

# 获取脚本所在目录的父目录（项目根目录）
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# 切换到项目根目录
cd "$PROJECT_ROOT"

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${GREEN}=== Pilot 开发环境启动脚本 ===${NC}"
echo ""

# 检查 Docker 是否运行
if ! docker info > /dev/null 2>&1; then
    echo -e "${RED}错误: Docker 未运行，请先启动 Docker${NC}"
    exit 1
fi

# 检查 Go 是否安装
if ! command -v go &> /dev/null; then
    echo -e "${RED}错误: Go 未安装，请先安装 Go 1.24+${NC}"
    exit 1
fi

export PILOT_CONFIG="${PILOT_CONFIG:-$PROJECT_ROOT/manifest/config/config.yaml}"

echo -e "${YELLOW}步骤 1/3: 启动 Docker 依赖服务...${NC}"
if [ ! -f "$PROJECT_ROOT/.env" ]; then
    echo -e "${RED}错误: 缺少 $PROJECT_ROOT/.env，请先 cp .env.example .env 并设置 MYSQL_ROOT_PASSWORD${NC}"
    exit 1
fi
docker compose --env-file "$PROJECT_ROOT/.env" -f manifest/docker/docker-compose.yml up -d

echo ""
echo -e "${YELLOW}步骤 2/3: 等待真实依赖服务就绪...${NC}"
attempt=1
max_attempts=30
while [ "$attempt" -le "$max_attempts" ]; do
    if go run ./cmd/checkdeps/main.go > /dev/null 2>&1; then
        break
    fi
    if [ "$attempt" -eq "$max_attempts" ]; then
        echo -e "${RED}错误: 依赖服务未在预期时间内就绪${NC}"
        go run ./cmd/checkdeps/main.go
        exit 1
    fi
    echo -e "${YELLOW}等待依赖服务 ($attempt/$max_attempts)...${NC}"
    sleep 2
    attempt=$((attempt + 1))
done

echo ""
echo -e "${YELLOW}步骤 3/3: 启动 Pilot 服务...${NC}"
exec go run ./cmd/main.go "$@"
