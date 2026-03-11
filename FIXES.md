# GPG/SSH 审批系统修复说明

## 修复内容

### 1. 统一缓存 TTL
- GPG agent SSH 缓存从 86400 秒降至 1800 秒（30 分钟）
- 与 bot 审批缓存保持一致

### 2. SSH 自动触发 GPG 解锁
- ssh-ask wrapper 在审批通过后检测 GPG agent 状态
- 如果 agent 锁定，自动创建 gpg_unlock_for_ssh 解锁请求
- 用户一次审批完成整个 SSH 操作

### 3. Bot 支持 GPG 解锁类型
- 新增 gpg_unlock_for_ssh 请求类型
- 复用现有密码解锁流程
- Telegram 消息显示为 "🔓 GPG 解锁（SSH 需要）"

### 4. 容器启动清空缓存
- entrypoint.sh 启动时执行 gpg-connect-agent reloadagent
- 确保重启后 bot 缓存与 GPG agent 缓存同步

## 验证状态

✅ GPG agent SSH 缓存 TTL: 1800 秒
✅ Bot 容器启动时清空缓存
✅ ssh-ask 支持自动 GPG 解锁
✅ Bot 支持 gpg_unlock_for_ssh 类型

## 测试方法

```bash
# 1. 测试 SSH（首次需要审批）
docker compose exec openclaw-gateway ssh -T git@github.com

# 2. 再次测试（应命中缓存）
docker compose exec openclaw-gateway ssh -T git@github.com

# 3. 重启容器验证缓存清空
docker compose restart gpg-approval-bot openclaw-gateway
docker compose logs --tail=5 gpg-approval-bot  # 应看到 "清空 GPG agent 缓存"

# 4. 重启后再次测试（应重新审批）
docker compose exec openclaw-gateway ssh -T git@github.com
```

## 关键改动文件

- `/home/ooxx/.gnupg/gpg-agent.conf` - SSH 缓存 TTL 配置
- `gpg-approval-bot/scripts/ssh-ask` - 自动 GPG 解锁逻辑
- `gpg-approval-bot/main.go` - 支持 gpg_unlock_for_ssh 类型
- `gpg-approval-bot/scripts/entrypoint.sh` - 启动时清空缓存
- `gpg-approval-bot/Dockerfile` - 使用新 entrypoint
