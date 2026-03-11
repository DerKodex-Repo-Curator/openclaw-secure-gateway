#!/usr/bin/env bash
# 测试 GPG clear-sign 功能
set -eu

echo "=== GPG Clear-Sign 测试 ==="
echo ""

# 检查 GPG 是否可用
if ! command -v gpg &> /dev/null; then
    echo "❌ 错误：未找到 gpg 命令"
    exit 1
fi

echo "✓ GPG 命令可用"
echo ""

# 列出可用的密钥
echo "📋 列出可用的 GPG 密钥："
gpg --list-secret-keys --keyid-format=long || {
    echo "⚠️  未找到私钥，请先导入或生成 GPG 密钥"
    exit 1
}
echo ""

# 创建测试文件
TEST_FILE="/tmp/test-message-$(date +%s).txt"
echo "这是一条测试消息，用于验证 GPG clear-sign 功能。" > "$TEST_FILE"
echo "时间戳: $(date)" >> "$TEST_FILE"

echo "✓ 创建测试文件: $TEST_FILE"
cat "$TEST_FILE"
echo ""

# 执行 clear-sign
SIGNED_FILE="${TEST_FILE}.asc"
echo "🔐 执行 GPG clear-sign..."
echo "命令: gpg --clear-sign --output \"$SIGNED_FILE\" \"$TEST_FILE\""
echo ""

if gpg --clear-sign --output "$SIGNED_FILE" "$TEST_FILE"; then
    echo ""
    echo "✅ GPG clear-sign 成功！"
    echo ""
    echo "📄 签名后的文件内容："
    echo "----------------------------------------"
    cat "$SIGNED_FILE"
    echo "----------------------------------------"
    echo ""

    # 验证签名
    echo "🔍 验证签名..."
    if gpg --verify "$SIGNED_FILE"; then
        echo ""
        echo "✅ 签名验证成功！"
    else
        echo ""
        echo "❌ 签名验证失败"
        exit 1
    fi
else
    echo ""
    echo "❌ GPG clear-sign 失败"
    exit 1
fi

echo ""
echo "🧹 清理测试文件..."
rm -f "$TEST_FILE" "$SIGNED_FILE"
echo "✅ 测试完成"
