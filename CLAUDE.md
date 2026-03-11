# CLAUDE.md

本文件为 AI 助手提供 gpg-approval-bot 项目的技术指导。

## 项目定位

这是一个 Telegram bot，用于审批 GPG/SSH 操作。Bot 通过 Unix socket 接收审批请求，通过 Telegram 与用户交互。

## 架构概览

### 核心组件

1. **Bot 主程序**（`main.go`）
   - HTTP 服务器：监听 Unix socket，接收审批请求
   - Telegram bot：处理用户交互（审批/拒绝/密码输入）
   - 密码解锁：验证密码并预设到 GPG agent 缓存

2. **Wrapper 脚本**
   - `scripts/gpg-ask`：拦截 GPG 命令，发送审批请求
   - `scripts/ssh-ask`：拦截 SSH 命令，发送审批请求并检测密钥缓存状态

3. **容器化部署**
   - `Dockerfile`：构建 bot 镜像
   - `scripts/entrypoint.sh`：容器启动时清空 GPG agent 缓存

### 工作流程

#### GPG 操作流程

1. 用户执行 `gpg --sign file`
2. `scripts/gpg-ask` 拦截命令，发送审批请求到 bot
3. Bot 在 Telegram 中显示审批按钮
4. 用户点击"允许"，bot 请求输入密码
5. 用户发送密码，bot 立即删除消息
6. Bot 调用 `unlockKey()` 函数：
   - 使用 `gpg --sign` 验证密码正确性
   - 使用 `PRESET_PASSPHRASE` 预设密码到 GPG agent 缓存
7. 解锁成功，wrapper 执行真实的 GPG 命令

#### SSH 操作流程

1. 用户执行 `ssh git@github.com`
2. `scripts/ssh-ask` 拦截命令，发送审批请求到 bot
3. Bot 在 Telegram 中显示审批按钮
4. 用户点击"允许"
5. `scripts/ssh-ask` 检查 GPG agent 中的 SSH 密钥缓存状态：
   - 如果密钥已缓存（第 7 列为 `1`），直接执行 SSH 命令
   - 如果密钥未缓存（第 7 列为 `-`），触发解锁流程（同 GPG 流程）
6. SSH 命令使用 GPG agent 中缓存的密码完成认证

## 关键技术细节

### 1. 密码缓存机制

**问题**：为什么需要 `PRESET_PASSPHRASE`？

GPG 主密钥有多个子密钥：
- 签名子密钥 `[S]`：用于 GPG 签名
- 认证子密钥 `[A]`：用于 SSH 认证
- 加密子密钥 `[E]`：用于加密

虽然它们共享主密钥的密码，但在 GPG agent 中有**独立的缓存状态**。

使用 `gpg --pinentry-mode loopback --passphrase <password> --sign` 验证密码时：
- ✅ 能验证密码正确性
- ❌ 只会缓存签名子密钥的密码
- ❌ 不会缓存认证子密钥的密码

导致 SSH 连接时仍然无法使用缓存的密码。

**解决方案**：使用 `PRESET_PASSPHRASE` 命令

```go
// 获取所有密钥的 keygrip
keyInfoCmd := exec.Command("gpg-connect-agent", "keyinfo --list", "/bye")
keyInfoOut, _ := keyInfoCmd.CombinedOutput()

// 将密码转换为十六进制
hexPass := fmt.Sprintf("%X", []byte(passphrase))

// 预设所有密钥的密码
for _, line := range strings.Split(string(keyInfoOut), "\n") {
    if strings.HasPrefix(line, "S KEYINFO ") {
        fields := strings.Fields(line)
        keygrip := fields[2]
        presetCmd := exec.Command("gpg-connect-agent")
        presetCmd.Stdin = strings.NewReader(fmt.Sprintf("PRESET_PASSPHRASE %s -1 %s\n/bye\n", keygrip, hexPass))
        presetCmd.Run()
    }
}
```

这样可以一次性预设所有子密钥（包括认证子密钥）的密码到 GPG agent 缓存。

### 2. SSH 密钥缓存检测

**问题**：如何判断 SSH 密钥是否已缓存？

使用 `gpg-connect-agent "keyinfo --ssh-list" /bye` 查询密钥状态。

输出格式：
```
S KEYINFO <keygrip> D - - <cached> P - <ttl> S
字段:    1      2    3 4 5 6   7    8 9  10  11
```

- 第 7 列为 `1`：密钥已缓存，可以直接使用
- 第 7 列为 `-`：密钥未缓存，需要解锁

**检测代码**（`scripts/ssh-ask`）：

```bash
key_cached="$(gpg-connect-agent "keyinfo --ssh-list" /bye 2>/dev/null | grep "^S KEYINFO" | awk '{if ($7 == "1") print "cached"}' | head -n 1)"
if [ -z "$key_cached" ]; then
    # 密钥未缓存，触发解锁流程
fi
```

### 3. GPG Agent 配置要求

`PRESET_PASSPHRASE` 命令需要 GPG agent 启用 `allow-preset-passphrase` 选项。

在 `~/.gnupg/gpg-agent.conf` 中添加：
```
allow-preset-passphrase
```

修改后重启 GPG agent：
```bash
gpgconf --kill gpg-agent
```

### 4. 容器启动时清空缓存

`scripts/entrypoint.sh` 在容器启动时清空 GPG agent 缓存：

```bash
echo "清空 GPG agent 缓存..."
gpg-connect-agent reloadagent /bye
```

这样可以确保容器重启后需要重新审批和解锁，避免安全风险。

## 安全原则（必须遵守）

### 密码处理

**严格禁止在日志中记录密码或可能包含密码的内容**：

- ❌ `log.Printf("密码: %s", password)` - 绝对禁止
- ❌ `log.Printf("十六进制密码: %X", []byte(password))` - 绝对禁止
- ❌ `log.Printf("命令: gpg --passphrase %s", password)` - 绝对禁止
- ❌ `log.Printf("解锁参数: %v", args)` - 如果 args 包含密码则禁止
- ✅ `log.Printf("解锁成功 request_id=%s", requestID)` - 只记录结果

### 密码传递

- ✅ 通过 `stdin` 传递密码给子进程
- ✅ 使用 `--passphrase` 参数时确保不会出现在进程列表中
- ✅ 密码只在内存中短暂存在，使用后立即清理

### 消息处理

- ✅ 用户发送的密码消息立即删除（`deleteMessage`）
- ✅ 审批消息在操作完成后删除
- ✅ 不在 Telegram 消息中回显密码

## 常见问题排查

### SSH 连接失败：agent refused operation

**症状**：
```
sign_and_send_pubkey: signing failed for ED25519 "(none)" from agent: agent refused operation
```

**原因**：SSH 认证子密钥未缓存密码

**排查步骤**：

1. 检查密钥缓存状态：
   ```bash
   gpg-connect-agent "keyinfo --ssh-list" /bye
   ```
   第 7 列应该是 `1`（已缓存），如果是 `-`（未缓存）则说明密码预设失败。

2. 检查 GPG agent 配置：
   ```bash
   grep allow-preset-passphrase ~/.gnupg/gpg-agent.conf
   ```
   如果没有这一行，需要添加并重启 GPG agent。

3. 检查 bot 日志：
   ```bash
   docker compose logs gpg-approval-bot | grep "预设"
   ```
   应该看到 "已预设 N 个密钥的密码到 GPG agent 缓存"。

### SSH 每次都触发解锁流程

**症状**：即使密钥已缓存，SSH 连接仍然触发解锁流程

**原因**：`scripts/ssh-ask` 的检测逻辑错误

**排查步骤**：

1. 手动测试检测逻辑：
   ```bash
   gpg-connect-agent "keyinfo --ssh-list" /bye | grep "^S KEYINFO" | awk '{print $7}'
   ```
   应该输出 `1`（已缓存）或 `-`（未缓存）。

2. 检查 `scripts/ssh-ask` 第 39 行：
   ```bash
   key_cached="$(gpg-connect-agent "keyinfo --ssh-list" /bye 2>/dev/null | grep "^S KEYINFO" | awk '{if ($7 == "1") print "cached"}' | head -n 1)"
   ```
   确保检查的是第 7 列，不是第 5 列。

## 修改建议

### 修改代码时的注意事项

1. **密码处理**：任何涉及密码的代码修改，必须确保不会在日志中泄露密码
2. **向后兼容**：修改 API 或配置时，考虑现有部署的兼容性
3. **错误处理**：密码验证失败时，给出清晰的错误信息，但不要泄露密码
4. **测试**：修改密码缓存或检测逻辑后，必须测试完整流程（首次解锁 + 缓存使用）

### 添加新功能时的考虑

1. **审批缓存**：新的操作类型是否需要审批缓存？
2. **密码缓存**：新的操作类型是否需要密码缓存？
3. **安全性**：新功能是否会引入密码泄露风险？
4. **用户体验**：新功能是否会增加不必要的审批次数？

## 测试方法

### 单元测试

目前没有单元测试，建议添加：

- `unlockKey()` 函数的测试（使用 mock GPG agent）
- 审批缓存逻辑的测试
- 消息处理逻辑的测试

### 集成测试

1. **GPG 签名测试**：
   ```bash
   echo "test" | gpg --clear-sign
   ```
   应该触发 Telegram 审批，输入密码后成功签名。

2. **SSH 连接测试**（首次）：
   ```bash
   ssh -T git@github.com
   ```
   应该触发 Telegram 审批和解锁流程，成功连接。

3. **SSH 连接测试**（缓存）：
   ```bash
   ssh -T git@github.com
   ```
   应该直接使用缓存，无需解锁，成功连接。

4. **缓存状态检查**：
   ```bash
   gpg-connect-agent "keyinfo --ssh-list" /bye
   ```
   第 7 列应该是 `1`（已缓存）。

## 相关文件

- `main.go:731-795` - `unlockKey()` 函数（密码验证和预设）
- `scripts/ssh-ask:36-60` - SSH 密钥缓存检测逻辑
- `scripts/entrypoint.sh` - 容器启动脚本（清空缓存）
- `Dockerfile` - 容器构建文件
- `README.md` - 用户文档
