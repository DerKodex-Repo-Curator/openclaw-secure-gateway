# gpg-approval-bot

用于 GPG/SSH 操作的 Telegram 审批服务。

## 功能
- 接收审批请求（允许/拒绝按钮）
- 审批通过后进入密码输入阶段（PENDING）
- 用户发送密码后自动删除消息并尝试解锁
- 成功后写回 `unlocked`，供 wrapper 继续执行真实命令
- 超时/拒绝时写回失败状态

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
- `APPROVAL_DIR`：审批状态文件目录（默认 `/approvals`）
- `GPG_APPROVAL_TIMEOUT`：超时时间（秒，默认 60）
- `GPG_UNLOCK_KEY_HINT`：可选，解锁 key 提示

## 独立启动
```bash
cp .env.example .env
cp docker-compose.yaml.example docker-compose.yaml
docker compose up -d --build
```

## 其他 Docker 容器如何使用
核心思路：
- 把本目录的 `scripts/gpg-ask`、`scripts/ssh-ask` 挂载到目标容器覆盖 `/usr/local/bin/gpg`、`/usr/local/bin/ssh`
- 共享审批目录（例如 `/approvals`）
- 共享宿主机 `.gnupg` 与 gpg-agent sockets

在你的 compose 里可直接加：

```yaml
services:
  your-app:
    # ... 你的镜像/构建配置
    environment:
      APPROVAL_DIR: /approvals
      SSH_AUTH_SOCK: /home/node/.gnupg/S.gpg-agent.ssh
    volumes:
      - /absolute/path/to/gpg-approval-bot/scripts/gpg-ask:/usr/local/bin/gpg:ro
      - /absolute/path/to/gpg-approval-bot/scripts/ssh-ask:/usr/local/bin/ssh:ro
      - /absolute/path/to/approvals:/approvals
      - /home/your-user/.gnupg:/home/node/.gnupg
      - /run/user/1000/gnupg/S.gpg-agent:/home/node/.gnupg/S.gpg-agent
      - /run/user/1000/gnupg/S.gpg-agent.extra:/home/node/.gnupg/S.gpg-agent.extra
      - /run/user/1000/gnupg/S.gpg-agent.browser:/home/node/.gnupg/S.gpg-agent.browser
      - /run/user/1000/gnupg/S.gpg-agent.ssh:/home/node/.gnupg/S.gpg-agent.ssh
```

## 安全约束
- 不在日志中打印密码。
- 不回显用户提交的密码内容。
- 用户发送内容应尽快删除。
