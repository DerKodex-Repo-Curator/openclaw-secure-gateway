# 测试脚本

本目录包含用于测试 gpg-approval-bot 功能的脚本。

## 脚本说明

### test-gpg-clearsign.sh
测试 GPG clear-sign 功能。

**用法**：
```bash
./test-gpg-clearsign.sh
```

**前置条件**：
- 已安装 GPG
- 已导入或生成 GPG 密钥
- GPG wrapper 已配置（如果使用审批系统）

### test-ssh-connection.sh
测试 SSH 连接（使用 GPG agent 管理的密钥）。

**用法**：
```bash
# 使用默认目标（git@github.com）
./test-ssh-connection.sh

# 指定自定义目标
SSH_TARGET=user@example.com ./test-ssh-connection.sh
```

**前置条件**：
- 已安装 SSH
- SSH_AUTH_SOCK 已设置
- GPG agent 已启用 SSH 支持
- SSH wrapper 已配置（如果使用审批系统）

### test-in-container.sh
在 Docker 容器内测试 GPG 和 SSH 审批功能。

**用法**：
```bash
# 在 docker-compose 项目目录中运行
./test-in-container.sh

# 指定自定义容器名称和 SSH 目标
CONTAINER_NAME=my-app SSH_TARGET=user@example.com ./test-in-container.sh
```

**前置条件**：
- 在 docker-compose 项目目录中运行
- 容器已启动
- 容器中已配置 GPG/SSH wrapper

### rebuild-and-test.sh
重新构建 bot 并验证修复。

**用法**：
```bash
# 在 docker-compose 项目目录中运行
./rebuild-and-test.sh

# 指定自定义服务名称
BOT_SERVICE=my-bot APP_SERVICE=my-app ./rebuild-and-test.sh
```

**前置条件**：
- 在 docker-compose 项目目录中运行
- docker-compose.yaml 中定义了相关服务

## 环境变量

- `CONTAINER_NAME`：容器名称（默认：app）
- `SSH_TARGET`：SSH 连接目标（默认：git@github.com）
- `BOT_SERVICE`：Bot 服务名称（默认：gpg-approval-bot）
- `APP_SERVICE`：应用服务名称（默认：app）

## 注意事项

1. **脱敏处理**：所有脚本已移除敏感信息（如具体的邮箱地址、服务器地址等）
2. **自定义配置**：用户应根据实际环境修改脚本中的默认值或使用环境变量
3. **审批系统**：如果使用 Telegram 审批系统，测试时需要在 Telegram 中手动审批
4. **权限设置**：确保脚本有执行权限（`chmod +x *.sh`）
