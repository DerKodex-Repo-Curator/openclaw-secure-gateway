#!/usr/bin/env bash
# 在容器内测试 GPG 和 SSH 审批功能
# 用法：在 docker-compose 项目目录中运行此脚本
set -eu

echo "=========================================="
echo "  容器内 GPG/SSH 审批测试"
echo "=========================================="
echo ""

# 检查是否在 docker-compose 项目目录中
if [ ! -f "docker-compose.yaml" ] && [ ! -f "docker-compose.yml" ]; then
    echo "❌ 错误：未找到 docker-compose.yaml"
    echo "请在 docker-compose 项目目录中运行此脚本"
    exit 1
fi

# 容器名称（用户应根据实际情况修改）
CONTAINER_NAME="${CONTAINER_NAME:-app}"

echo "使用容器: $CONTAINER_NAME"
echo "提示：可通过环境变量 CONTAINER_NAME 指定容器名称"
echo ""

# 测试 1: GPG clear-sign
echo "📝 测试 1: GPG clear-sign"
echo "----------------------------------------"
echo "这将触发 Telegram 审批请求..."
echo ""

TEST_MSG="测试消息 - $(date)"
if echo "$TEST_MSG" | docker compose exec -T "$CONTAINER_NAME" gpg --clear-sign 2>&1; then
    echo ""
    echo "✅ GPG clear-sign 成功"
else
    echo ""
    echo "❌ GPG clear-sign 失败（可能被拒绝或超时）"
fi

echo ""
echo "=========================================="
echo ""

# 测试 2: SSH 连接
echo "🔐 测试 2: SSH 连接"
echo "----------------------------------------"
echo "这将触发 Telegram 审批请求..."
echo ""

# SSH 目标（用户应根据实际情况修改）
SSH_TARGET="${SSH_TARGET:-git@github.com}"
echo "SSH 目标: $SSH_TARGET"
echo ""

if docker compose exec -T "$CONTAINER_NAME" ssh -T -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null "$SSH_TARGET" 2>&1 | tee /tmp/ssh-test.log; then
    :
fi

if grep -q "successfully authenticated" /tmp/ssh-test.log 2>/dev/null; then
    echo ""
    echo "✅ SSH 连接成功"
elif grep -q "Permission denied" /tmp/ssh-test.log 2>/dev/null; then
    echo ""
    echo "❌ SSH 连接失败：权限被拒绝"
elif grep -q "agent refused" /tmp/ssh-test.log 2>/dev/null; then
    echo ""
    echo "⚠️  SSH agent 拒绝操作（可能需要审批或解锁密钥）"
else
    echo ""
    echo "⚠️  SSH 测试结果未知"
fi

rm -f /tmp/ssh-test.log

echo ""
echo "=========================================="
echo "测试完成"
echo ""
echo "💡 提示："
echo "  - 如果看到审批请求，请在 Telegram 中批准"
echo "  - GPG 操作可能需要输入密码"
echo "  - SSH 操作会使用 GPG agent 管理的密钥"
echo "=========================================="
