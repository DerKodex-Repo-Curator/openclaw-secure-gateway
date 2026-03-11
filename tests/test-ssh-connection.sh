#!/usr/bin/env bash
# 测试 SSH 连接（使用 GPG agent 管理的密钥）
set -eu

echo "=== SSH 连接测试 ==="
echo ""

# 检查 SSH 是否可用
if ! command -v ssh &> /dev/null; then
    echo "❌ 错误：未找到 ssh 命令"
    exit 1
fi

echo "✓ SSH 命令可用"
echo ""

# 检查 SSH agent
if [ -n "${SSH_AUTH_SOCK:-}" ]; then
    echo "✓ SSH_AUTH_SOCK 已设置: $SSH_AUTH_SOCK"

    # 列出已加载的密钥
    echo ""
    echo "📋 SSH Agent 中的密钥："
    if ssh-add -l 2>/dev/null; then
        echo "✓ 找到已加载的密钥"
    else
        echo "⚠️  SSH Agent 中没有密钥，可能需要先添加"
    fi
else
    echo "⚠️  SSH_AUTH_SOCK 未设置，SSH agent 可能未运行"
fi

echo ""
echo "🔐 测试 SSH 连接到远程服务器..."
echo "提示：请将 'git@github.com' 替换为你的实际 SSH 目标"
echo ""

# 示例：测试 GitHub SSH 连接
# 用户应根据实际情况修改目标地址
SSH_TARGET="${SSH_TARGET:-git@github.com}"
echo "目标: $SSH_TARGET"
echo ""

# 执行 SSH 测试
if ssh -T "$SSH_TARGET" 2>&1 | tee /tmp/ssh-test-output.txt; then
    :
fi

# 检查输出内容判断是否成功
if grep -q "successfully authenticated" /tmp/ssh-test-output.txt; then
    echo ""
    echo "✅ SSH 连接成功！服务器已认证你的身份"
    rm -f /tmp/ssh-test-output.txt
    exit 0
elif grep -q "Permission denied" /tmp/ssh-test-output.txt; then
    echo ""
    echo "❌ SSH 连接失败：权限被拒绝"
    echo "可能原因："
    echo "  - SSH 密钥未添加到远程服务器"
    echo "  - SSH agent 中没有正确的密钥"
    rm -f /tmp/ssh-test-output.txt
    exit 1
else
    echo ""
    echo "⚠️  SSH 连接结果未知，请检查上面的输出"
    rm -f /tmp/ssh-test-output.txt
    exit 1
fi
