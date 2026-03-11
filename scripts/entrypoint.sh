#!/usr/bin/env sh
set -e

# 清空 GPG agent 缓存，确保容器重启后状态一致
echo "清空 GPG agent 缓存..."
gpg-connect-agent reloadagent /bye 2>/dev/null || true

# 启动 bot
exec /app/gpg-approval-bot
