#!/usr/bin/env bash
# 重新构建 bot 并验证修复
# 用法：在 docker-compose 项目目录中运行此脚本
set -e

echo "=== GPG/SSH 审批系统重建与验证 ==="
echo ""

# 检查是否在 docker-compose 项目目录中
if [ ! -f "docker-compose.yaml" ] && [ ! -f "docker-compose.yml" ]; then
    echo "❌ 错误：未找到 docker-compose.yaml"
    echo "请在 docker-compose 项目目录中运行此脚本"
    exit 1
fi

# 服务名称（用户应根据实际情况修改）
BOT_SERVICE="${BOT_SERVICE:-gpg-approval-bot}"
APP_SERVICE="${APP_SERVICE:-app}"

echo "Bot 服务: $BOT_SERVICE"
echo "应用服务: $APP_SERVICE"
echo "提示：可通过环境变量 BOT_SERVICE 和 APP_SERVICE 指定服务名称"
echo ""

echo "步骤 1: 重新构建 bot 镜像..."
docker compose build "$BOT_SERVICE"

echo ""
echo "步骤 2: 重新创建并启动服务..."
docker compose up -d --force-recreate "$BOT_SERVICE"
docker compose restart "$APP_SERVICE"

echo ""
echo "步骤 3: 等待服务启动..."
sleep 3

echo ""
echo "步骤 4: 检查 bot 日志（应看到清空缓存消息）..."
docker compose logs --tail=20 "$BOT_SERVICE"

echo ""
echo "步骤 5: 测试 GPG 签名（应触发审批）..."
echo "执行: docker compose exec $APP_SERVICE gpg --clear-sign"
echo "请在 Telegram 中审批并输入密码"
echo ""

echo "步骤 6: 测试 SSH 连接（应自动处理 GPG 解锁）..."
echo "执行: docker compose exec $APP_SERVICE ssh -T <target>"
echo "如果 GPG agent 锁定，应自动触发解锁流程"
echo ""

echo "步骤 7: 再次测试 SSH（应命中缓存）..."
echo "执行: docker compose exec $APP_SERVICE ssh -T <target>"
echo "应该无需审批，直接通过"
echo ""

echo "=== 验证完成 ==="
echo ""
echo "关键验证点："
echo "1. bot 启动时日志显示 '清空 GPG agent 缓存'"
echo "2. SSH 操作在 GPG agent 锁定时自动触发解锁审批"
echo "3. 缓存时间内重复操作命中缓存"
echo "4. 容器重启后缓存清空，需要重新审批"
