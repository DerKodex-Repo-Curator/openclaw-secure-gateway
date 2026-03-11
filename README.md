# gpg-approval-bot

用于 GPG/SSH 操作的 Telegram 审批服务。

## 功能
- 接收审批请求（允许/拒绝按钮）
- 审批通过后进入密码输入阶段（PENDING）
- 用户发送密码后自动删除消息并尝试解锁
- **密码缓存**：解锁成功后使用 `PRESET_PASSPHRASE` 将密码预设到 GPG agent 缓存
- **智能检测**：SSH 操作前检查密钥缓存状态，避免不必要的重复解锁
- 成功后写回 `unlocked`，供 wrapper 继续执行真实命令
- 超时/拒绝时写回失败状态
- **审批缓存**：相同操作在缓存时间内无需重复审批

## 目录
- `main.go`：Bot 主程序
- `Dockerfile`：容器构建文件
- `scripts/gpg-ask`：GPG wrapper
- `scripts/ssh-ask`：SSH wrapper
- `docker-compose.yaml.example`：独立运行示例
- `.env.example`：示例环境变量

## 环境变量
- `GPG_APPROVAL_BOT_TOKEN`：审批 bot token
- `GPG_APPROVAL_CHAT_ID`：允许交互的 chat id
- `APPROVAL_SOCKET`：Unix Socket 路径（默认 `/run/approval-rpc/approval.sock`）
- `GPG_APPROVAL_TIMEOUT`：单次审批超时时间（秒，默认 60）
- `SSH_APPROVAL_CACHE_SECONDS`：SSH 审批缓存秒数（默认 1800，设为 `0` 表示每次都审批）
- `GPG_APPROVAL_CACHE_SECONDS`：GPG 审批缓存秒数（默认 1800，设为 `0` 表示每次都审批）
- `GPG_UNLOCK_KEY_HINT`：可选，解锁 key 提示

## 独立启动
```bash
cp .env.example .env
# 在 .env 里填写 HOST_APPROVAL_DIR / HOST_GNUPG_DIR / HOST_GPG_AGENT_DIR
cp docker-compose.yaml.example docker-compose.yaml
docker compose up -d --build
```

## 其他 Docker 容器如何使用
核心思路：
- 把本目录 `scripts/gpg-ask`、`scripts/ssh-ask` 挂载到目标容器覆盖 `/usr/local/bin/gpg`、`/usr/local/bin/ssh`
- 共享审批目录
- 共享 `.gnupg` 与 gpg-agent sockets

在你的 compose 里可直接加：

```yaml
services:
  your-app:
    environment:
      APPROVAL_DIR: ${BOT_APPROVAL_DIR}
      SSH_AUTH_SOCK: ${BOT_SSH_AUTH_SOCK}
    volumes:
      - ${BOT_SCRIPT_DIR}/gpg-ask:/usr/local/bin/gpg:ro
      - ${BOT_SCRIPT_DIR}/ssh-ask:/usr/local/bin/ssh:ro
      - ${HOST_APPROVAL_DIR}:${BOT_APPROVAL_DIR}
      - ${HOST_GNUPG_DIR}:${BOT_GNUPG_DIR}
      - ${HOST_GPG_AGENT_DIR}/S.gpg-agent:${BOT_GNUPG_DIR}/S.gpg-agent
      - ${HOST_GPG_AGENT_DIR}/S.gpg-agent.extra:${BOT_GNUPG_DIR}/S.gpg-agent.extra
      - ${HOST_GPG_AGENT_DIR}/S.gpg-agent.browser:${BOT_GNUPG_DIR}/S.gpg-agent.browser
      - ${HOST_GPG_AGENT_DIR}/S.gpg-agent.ssh:${BOT_GNUPG_DIR}/S.gpg-agent.ssh
```

## 技术细节

### GPG 密码缓存机制

解锁流程分为两步：

1. **密码验证**：使用 `gpg --pinentry-mode loopback --passphrase <password> --sign` 验证密码正确性
2. **密码预设**：验证成功后，使用 `gpg-connect-agent PRESET_PASSPHRASE` 将密码预设到 GPG agent 缓存

这样做的好处：
- SSH 认证子密钥 `[A]` 和 GPG 签名子密钥 `[S]` 共享主密钥密码，但在 GPG agent 中有独立的缓存状态
- 单纯的 `gpg --sign` 只会缓存签名子密钥的密码，不会缓存认证子密钥的密码
- 使用 `PRESET_PASSPHRASE` 可以一次性预设所有子密钥（包括认证子密钥）的密码到缓存

### SSH 密钥缓存检测

`scripts/ssh-ask` 在审批通过后会检查 GPG agent 中的 SSH 密钥缓存状态：

```bash
gpg-connect-agent "keyinfo --ssh-list" /bye
```

输出格式：`S KEYINFO <keygrip> D - - <cached> P - <ttl> S`

- 第 7 列为 `1` 表示密钥已缓存，可以直接使用
- 第 7 列为 `-` 表示密钥未缓存，需要触发解锁流程

### GPG Agent 配置要求

为了使用 `PRESET_PASSPHRASE` 功能，需要在 `~/.gnupg/gpg-agent.conf` 中添加：

```
allow-preset-passphrase
```

修改后需要重启 GPG agent：

```bash
gpgconf --kill gpg-agent
```

## 安全约束

**严格禁止在日志中记录密码或可能包含密码的内容**：

- ❌ 不在日志中打印密码明文
- ❌ 不在日志中打印密码的十六进制编码
- ❌ 不在日志中打印包含密码的命令行参数
- ❌ 不回显用户提交的密码内容
- ✅ 用户发送的密码消息立即删除
- ✅ 密码通过 stdin 传递给子进程，不出现在进程列表中
- ✅ 日志只记录操作结果（成功/失败），不记录密码相关内容
