package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type UpdateResponse struct {
	OK     bool     `json:"ok"`
	Result []Update `json:"result"`
}

type Update struct {
	UpdateID      int64          `json:"update_id"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
	Message       *TGMessage     `json:"message,omitempty"`
}

type CallbackQuery struct {
	ID      string      `json:"id"`
	Data    string      `json:"data"`
	Message *TGMessage  `json:"message,omitempty"`
	From    interface{} `json:"from,omitempty"`
}

type TGMessage struct {
	MessageID int64  `json:"message_id"`
	Date      int64  `json:"date"`
	Text      string `json:"text"`
	Chat      struct {
		ID int64 `json:"id"`
	} `json:"chat"`
}

type tgSendMessageResp struct {
	OK     bool `json:"ok"`
	Result struct {
		MessageID int64 `json:"message_id"`
	} `json:"result"`
}

type rpcRequestBody struct {
	Kind          string `json:"kind"`
	Cmd           string `json:"cmd"`
	TimeoutSecond int    `json:"timeout_seconds"`
	KeyHint       string `json:"key_hint,omitempty"`
}

type rpcWaitBody struct {
	RequestID     string `json:"request_id"`
	TimeoutSecond int    `json:"timeout_seconds"`
}

type approvalReq struct {
	ID        string
	Kind      string // gpg | ssh | sudo
	Cmd       string
	KeyHint   string
	CreatedAt time.Time
	Deadline  time.Time
	MessageID int64
	PromptID  int64

	mu        sync.Mutex
	state     string // waiting_approval | waiting_password | approved | denied | timeout
	done      chan string
	finalized bool
}

type approvalManager struct {
	mu       sync.RWMutex
	requests map[string]*approvalReq
	grantMu  sync.Mutex
	grants   map[string]time.Time
	grantTTL map[string]time.Duration
}

func newManager() *approvalManager {
	return &approvalManager{
		requests: make(map[string]*approvalReq),
		grants:   make(map[string]time.Time),
		grantTTL: make(map[string]time.Duration),
	}
}

func (m *approvalManager) add(r *approvalReq) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[r.ID] = r
}

func (m *approvalManager) get(id string) (*approvalReq, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.requests[id]
	return r, ok
}

func (m *approvalManager) latestPendingPassword() *approvalReq {
	m.mu.RLock()
	defer m.mu.RUnlock()
	list := make([]*approvalReq, 0)
	for _, r := range m.requests {
		r.mu.Lock()
		st := r.state
		r.mu.Unlock()
		if st == "waiting_password" {
			list = append(list, r)
		}
	}
	if len(list) == 0 {
		return nil
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt.After(list[j].CreatedAt)
	})
	return list[0]
}

func (m *approvalManager) setGrantTTL(kind string, seconds int) {
	m.grantMu.Lock()
	defer m.grantMu.Unlock()
	if seconds <= 0 {
		m.grantTTL[kind] = 0
		return
	}
	m.grantTTL[kind] = time.Duration(seconds) * time.Second
}

func (m *approvalManager) hasValidGrant(kind string) bool {
	m.grantMu.Lock()
	defer m.grantMu.Unlock()
	ttl, ok := m.grantTTL[kind]
	if !ok || ttl <= 0 {
		return false
	}
	expireAt, ok := m.grants[kind]
	if !ok {
		return false
	}
	if time.Now().After(expireAt) {
		delete(m.grants, kind)
		return false
	}
	return true
}

func (m *approvalManager) rememberGrant(kind string) {
	m.grantMu.Lock()
	defer m.grantMu.Unlock()
	ttl, ok := m.grantTTL[kind]
	if !ok || ttl <= 0 {
		return
	}
	m.grants[kind] = time.Now().Add(ttl)
}

func (r *approvalReq) transition(to string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finalized {
		return false
	}
	r.state = to
	if to == "approved" || to == "denied" || to == "timeout" {
		r.finalized = true
		select {
		case r.done <- to:
		default:
		}
		close(r.done)
	}
	return true
}

func main() {
	token := strings.TrimSpace(os.Getenv("GPG_APPROVAL_BOT_TOKEN"))
	chatIDStr := strings.TrimSpace(os.Getenv("GPG_APPROVAL_CHAT_ID"))
	socketPath := strings.TrimSpace(os.Getenv("APPROVAL_SOCKET"))
	if socketPath == "" {
		socketPath = "/run/approval-rpc/approval.sock"
	}
	if token == "" || chatIDStr == "" {
		log.Fatal("缺少 GPG_APPROVAL_BOT_TOKEN 或 GPG_APPROVAL_CHAT_ID")
	}
	allowedChatID, err := strconv.ParseInt(chatIDStr, 10, 64)
	if err != nil {
		log.Fatalf("GPG_APPROVAL_CHAT_ID 非法: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		log.Fatalf("创建 socket 目录失败: %v", err)
	}
	_ = os.Remove(socketPath)

	baseURL := "https://api.telegram.org/bot" + token
	mgr := newManager()
	mgr.setGrantTTL("ssh", envInt("SSH_APPROVAL_CACHE_SECONDS", 1800))
	mgr.setGrantTTL("gpg", envInt("GPG_APPROVAL_CACHE_SECONDS", 1800))
	mgr.setGrantTTL("sudo", envInt("SUDO_APPROVAL_CACHE_SECONDS", 0))

	go runRPCServer(socketPath, baseURL, allowedChatID, mgr)
	log.Printf("gpg-approval-bot 启动，socket=%s", socketPath)

	offset := int64(0)
	for {
		updates, nextOffset, err := getUpdates(baseURL, offset)
		if err != nil {
			log.Printf("getUpdates 失败: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		offset = nextOffset
		for _, upd := range updates {
			if upd.CallbackQuery != nil {
				if err := handleCallback(baseURL, upd.CallbackQuery, allowedChatID, mgr); err != nil {
					log.Printf("处理回调失败: %v", err)
				}
			}
			if upd.Message != nil {
				if err := handleMessage(baseURL, upd.Message, allowedChatID, mgr); err != nil {
					log.Printf("处理消息失败: %v", err)
				}
			}
		}
	}
}

func runRPCServer(socketPath, baseURL string, allowedChatID int64, mgr *approvalManager) {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/v1/request", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body rpcRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		kind := strings.TrimSpace(body.Kind)
		if kind != "gpg" && kind != "ssh" && kind != "sudo" && kind != "gpg_unlock_for_ssh" {
			http.Error(w, "kind must be gpg or ssh or sudo or gpg_unlock_for_ssh", http.StatusBadRequest)
			return
		}
		cmd := strings.TrimSpace(body.Cmd)
		if cmd == "" {
			http.Error(w, "cmd required", http.StatusBadRequest)
			return
		}
		timeout := body.TimeoutSecond
		if timeout <= 0 {
			timeout = 60
		}
		if mgr.hasValidGrant(kind) {
			reqID := shortID(kind)
			req := &approvalReq{
				ID:        reqID,
				Kind:      kind,
				Cmd:       cmd,
				KeyHint:   strings.TrimSpace(body.KeyHint),
				CreatedAt: time.Now(),
				Deadline:  time.Now(),
				state:     "approved",
				done:      make(chan string, 1),
				finalized: true,
			}
			close(req.done)
			mgr.add(req)
			log.Printf("命中审批缓存 id=%s kind=%s", req.ID, req.Kind)
			_ = sendCachedNotification(baseURL, allowedChatID, req)
			writeJSON(w, map[string]any{"request_id": reqID, "cached": true})
			return
		}

		reqID := shortID(kind)
		req := &approvalReq{
			ID:        reqID,
			Kind:      kind,
			Cmd:       cmd,
			KeyHint:   strings.TrimSpace(body.KeyHint),
			CreatedAt: time.Now(),
			Deadline:  time.Now().Add(time.Duration(timeout) * time.Second),
			state:     "waiting_approval",
			done:      make(chan string, 1),
		}

		msgID, err := sendApprovalMessage(baseURL, allowedChatID, req, timeout)
		if err != nil {
			http.Error(w, "send telegram failed", http.StatusBadGateway)
			log.Printf("发送审批消息失败: %v", err)
			return
		}
		req.MessageID = msgID
		mgr.add(req)
		log.Printf("创建审批请求 id=%s kind=%s", req.ID, req.Kind)

		go func(rq *approvalReq, t int) {
			<-time.After(time.Duration(t) * time.Second)
			rq.mu.Lock()
			state := rq.state
			rq.mu.Unlock()
			if state == "waiting_approval" || state == "waiting_password" {
				if rq.transition("timeout") {
					status := "⏱️ 超时失败（未审批）"
					if state == "waiting_password" {
						status = "⏱️ 超时失败（未输入密码）"
						_ = editPendingPromptMessage(baseURL, allowedChatID, rq, "⏱️ 超时失败（未输入密码）")
					}
					_ = editStatusMessage(baseURL, allowedChatID, rq.MessageID, rq.Cmd, status)
				}
			}
		}(req, timeout)

		writeJSON(w, map[string]any{"request_id": reqID})
	})

	mux.HandleFunc("/v1/wait", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body rpcWaitBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		reqID := strings.TrimSpace(body.RequestID)
		if reqID == "" {
			http.Error(w, "request_id required", http.StatusBadRequest)
			return
		}
		req, ok := mgr.get(reqID)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		waitSec := body.TimeoutSecond
		if waitSec <= 0 {
			waitSec = 90
		}

		req.mu.Lock()
		st := req.state
		req.mu.Unlock()
		if st == "approved" || st == "denied" || st == "timeout" {
			writeJSON(w, map[string]any{"status": st})
			return
		}

		select {
		case st, ok := <-req.done:
			if !ok {
				req.mu.Lock()
				st = req.state
				req.mu.Unlock()
			}
			writeJSON(w, map[string]any{"status": st})
		case <-time.After(time.Duration(waitSec) * time.Second):
			writeJSON(w, map[string]any{"status": "wait_timeout"})
		}
	})

	server := &http.Server{Handler: mux}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		log.Fatalf("监听 unix socket 失败: %v", err)
	}
	_ = os.Chmod(socketPath, 0o660)
	if err := server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("rpc server 退出: %v", err)
	}
}

func sendCachedNotification(baseURL string, chatID int64, req *approvalReq) error {
	emoji := "🔑"
	title := "GPG"
	if req.Kind == "ssh" {
		emoji = "🔐"
		title = "SSH"
	} else if req.Kind == "sudo" {
		emoji = "🛡️"
		title = "SUDO"
	} else if req.Kind == "gpg_unlock_for_ssh" {
		emoji = "🔓"
		title = "GPG 解锁（SSH 需要）"
	}
	cmdMD := strings.ReplaceAll(req.Cmd, "`", "\\`")
	text := fmt.Sprintf("✅ %s %s 已成功（已缓存）\n\n调用信息: `%s`", emoji, title, cmdMD)
	return postJSON(baseURL+"/sendMessage", map[string]any{
		"chat_id":    chatID,
		"parse_mode": "Markdown",
		"text":       text,
	})
}

func sendApprovalMessage(baseURL string, chatID int64, req *approvalReq, timeout int) (int64, error) {
	emoji := "🔑"
	title := "GPG"
	if req.Kind == "ssh" {
		emoji = "🔐"
		title = "SSH"
	} else if req.Kind == "sudo" {
		emoji = "🛡️"
		title = "SUDO"
	} else if req.Kind == "gpg_unlock_for_ssh" {
		emoji = "🔓"
		title = "GPG 解锁（SSH 需要）"
	}
	cmdMD := strings.ReplaceAll(req.Cmd, "`", "\\`")
	text := fmt.Sprintf("%s %s 调用审批\n\n调用信息: `%s`\n\n请在 %d 秒内审批", emoji, title, cmdMD, timeout)
	cbApprove := fmt.Sprintf("approve:%s", req.ID)
	cbDeny := fmt.Sprintf("deny:%s", req.ID)
	respBody, err := postJSONRead(baseURL+"/sendMessage", map[string]any{
		"chat_id":    chatID,
		"parse_mode": "Markdown",
		"text":       text,
		"reply_markup": map[string]any{
			"inline_keyboard": []any{
				[]any{
					map[string]any{"text": "✅ 允许", "callback_data": cbApprove},
					map[string]any{"text": "❌ 拒绝", "callback_data": cbDeny},
				},
			},
		},
	})
	if err != nil {
		return 0, err
	}
	var parsed tgSendMessageResp
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return 0, err
	}
	if !parsed.OK || parsed.Result.MessageID == 0 {
		return 0, errors.New("sendMessage 未返回有效 message_id")
	}
	return parsed.Result.MessageID, nil
}

func handleCallback(baseURL string, cb *CallbackQuery, allowedChatID int64, mgr *approvalManager) error {
	if cb.Message == nil {
		return errors.New("callback message 为空")
	}
	if cb.Message.Chat.ID != allowedChatID {
		_ = postJSON(baseURL+"/answerCallbackQuery", map[string]any{
			"callback_query_id": cb.ID,
			"text":              "未授权的聊天来源",
			"show_alert":        true,
		})
		return fmt.Errorf("拒绝未授权 chat_id=%d", cb.Message.Chat.ID)
	}
	parts := strings.SplitN(cb.Data, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("callback_data 非法: %s", cb.Data)
	}
	action, requestID := parts[0], parts[1]
	req, ok := mgr.get(requestID)
	if !ok {
		_ = postJSON(baseURL+"/answerCallbackQuery", map[string]any{
			"callback_query_id": cb.ID,
			"text":              "请求不存在或已过期",
		})
		return nil
	}

	if action == "deny" {
		if req.transition("denied") {
			_ = editStatusMessage(baseURL, allowedChatID, req.MessageID, req.Cmd, "❌ 已拒绝")
		}
		_ = postJSON(baseURL+"/answerCallbackQuery", map[string]any{"callback_query_id": cb.ID, "text": "❌ 已拒绝"})
		return nil
	}
	if action != "approve" {
		return fmt.Errorf("未知 action: %s", action)
	}

	if req.Kind == "ssh" || req.Kind == "sudo" {
		if req.transition("approved") {
			// 注意：SSH 审批不立即记录缓存，需要等待后续的 GPG 解锁成功
			// 只有 sudo 立即记录缓存
			if req.Kind == "sudo" {
				mgr.rememberGrant(req.Kind)
			}
			_ = editStatusMessage(baseURL, allowedChatID, req.MessageID, req.Cmd, "✅ 已批准")
		}
		_ = postJSON(baseURL+"/answerCallbackQuery", map[string]any{"callback_query_id": cb.ID, "text": "✅ 已批准"})
		return nil
	}

	if req.Kind == "gpg_unlock_for_ssh" {
		req.mu.Lock()
		if req.finalized {
			req.mu.Unlock()
			return nil
		}
		req.state = "waiting_password"
		req.mu.Unlock()
		status := "🟡 已批准，需要密码解锁 GPG agent（用于 SSH）\n请输入密码（发送后自动删除）"
		if req.KeyHint != "" {
			status += fmt.Sprintf("\n密钥提示: %s", req.KeyHint)
		}
		_ = editStatusMessage(baseURL, allowedChatID, req.MessageID, req.Cmd, status)
		promptID, err := sendPendingPromptMessage(baseURL, allowedChatID)
		if err != nil {
			log.Printf("发送 PENDING 提示失败 request_id=%s err=%v", req.ID, err)
		} else {
			req.mu.Lock()
			req.PromptID = promptID
			req.mu.Unlock()
		}
		_ = postJSON(baseURL+"/answerCallbackQuery", map[string]any{"callback_query_id": cb.ID, "text": "✅ 已批准"})
		return nil
	}

	req.mu.Lock()
	if req.finalized {
		req.mu.Unlock()
		return nil
	}
	req.state = "waiting_password"
	req.mu.Unlock()
	status := "🟡 已批准，但需要密码（PENDING）\n请输入密码（发送后自动删除）"
	if req.KeyHint != "" {
		status += fmt.Sprintf("\n密钥提示: %s", req.KeyHint)
	}
	_ = editStatusMessage(baseURL, allowedChatID, req.MessageID, req.Cmd, status)
	promptID, err := sendPendingPromptMessage(baseURL, allowedChatID)
	if err != nil {
		log.Printf("发送 PENDING 提示失败 request_id=%s err=%v", req.ID, err)
	} else {
		req.mu.Lock()
		req.PromptID = promptID
		req.mu.Unlock()
	}
	_ = postJSON(baseURL+"/answerCallbackQuery", map[string]any{"callback_query_id": cb.ID, "text": "✅ 已批准"})
	return nil
}

func handleMessage(baseURL string, msg *TGMessage, allowedChatID int64, mgr *approvalManager) error {
	if msg.Chat.ID != allowedChatID {
		return nil
	}

	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return nil
	}

	// 命令处理
	if strings.HasPrefix(text, "/lock") {
		log.Printf("收到命令: /lock")

		err := handleLockCommand(baseURL, allowedChatID, mgr)
		_ = deleteMessage(baseURL, msg.Chat.ID, msg.MessageID)
		return err
	}

	if strings.HasPrefix(text, "/unlock") {
		log.Printf("收到命令: /unlock")
		parts := strings.Fields(text)
		keyHint := ""
		if len(parts) > 1 {
			keyHint = parts[1]
		}
		err := handleUnlockCommand(baseURL, allowedChatID, mgr, keyHint)
		_ = deleteMessage(baseURL, msg.Chat.ID, msg.MessageID)
		return err
	}

	// 密码处理：只有在有待处理的密码请求时才处理
	req := mgr.latestPendingPassword()

	// 无论是否有待处理请求，都要删除来自授权聊天的消息（可能是晚到的密码）
	defer func() { _ = deleteMessage(baseURL, msg.Chat.ID, msg.MessageID) }()

	if req == nil {
		// 没有待处理的密码请求，忽略此消息
		log.Printf("收到非命令消息但无待处理密码请求，忽略")
		return nil
	}

	// 确认是密码输入，记录日志但不打印密码内容
	log.Printf("收到密码输入，尝试解锁 request_id=%s", req.ID)
	passphrase := text

	ok, err := unlockKey(passphrase, req.KeyHint)
	if !ok {
		if err != nil {
			log.Printf("解锁失败 request_id=%s err=%v", req.ID, err)
		}
		_ = editPendingPromptMessage(baseURL, allowedChatID, req, "❌ 密码验证失败，请重新输入（发送后自动删除）")
		return nil
	}
	if req.transition("approved") {
		// manual_unlock 不记录缓存
		if req.Kind != "manual_unlock" {
			// gpg_unlock_for_ssh 解锁成功后，记录 ssh 审批缓存
			if req.Kind == "gpg_unlock_for_ssh" {
				mgr.rememberGrant("ssh")
			} else {
				mgr.rememberGrant(req.Kind)
			}
		}
		_ = editStatusMessage(baseURL, allowedChatID, req.MessageID, req.Cmd, "✅ 已成功")
	}
	_ = deletePendingPromptMessage(baseURL, allowedChatID, req)
	log.Printf("解锁成功 request_id=%s", req.ID)
	return nil
}

func handleLockCommand(baseURL string, chatID int64, mgr *approvalManager) error {
	log.Printf("执行 /lock 命令")

	// 清空 bot 缓存
	mgr.grantMu.Lock()
	mgr.grants = make(map[string]time.Time)
	mgr.grantMu.Unlock()

	// 锁定 GPG agent
	cmd := exec.Command("gpg-connect-agent", "reloadagent", "/bye")
	if err := cmd.Run(); err != nil {
		log.Printf("锁定 GPG agent 失败: %v", err)
		err2 := postJSON(baseURL+"/sendMessage", map[string]any{
			"chat_id": chatID,
			"text":    "🔒 锁定失败：" + err.Error(),
		})
		if err2 != nil {
			log.Printf("发送失败消息失败: %v", err2)
		}
		return err
	}

	log.Printf("手动锁定 GPG agent 成功")
	err := postJSON(baseURL+"/sendMessage", map[string]any{
		"chat_id": chatID,
		"text":    "🔒 GPG agent 已锁定，所有缓存已清空",
	})
	if err != nil {
		log.Printf("发送成功消息失败: %v", err)
	}
	return err
}

func handleUnlockCommand(baseURL string, chatID int64, mgr *approvalManager, keyHint string) error {
	reqID := shortID("manual")
	req := &approvalReq{
		ID:        reqID,
		Kind:      "manual_unlock",
		Cmd:       "/unlock " + keyHint,
		KeyHint:   keyHint,
		CreatedAt: time.Now(),
		Deadline:  time.Now().Add(300 * time.Second),
		state:     "waiting_password",
		done:      make(chan string, 1),
	}
	mgr.add(req)

	text := "🔓 手动解锁 GPG agent\n\n请输入密码（发送后自动删除）"
	if keyHint != "" {
		text += fmt.Sprintf("\n密钥提示: %s", keyHint)
	}
	respBody, err := postJSONRead(baseURL+"/sendMessage", map[string]any{
		"chat_id":    chatID,
		"parse_mode": "Markdown",
		"text":       text,
	})
	if err != nil {
		return err
	}
	var parsed tgSendMessageResp
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return err
	}
	if parsed.OK && parsed.Result.MessageID > 0 {
		req.mu.Lock()
		req.MessageID = parsed.Result.MessageID
		req.mu.Unlock()
	}

	promptID, err := sendPendingPromptMessage(baseURL, chatID)
	if err != nil {
		return err
	}
	req.mu.Lock()
	req.PromptID = promptID
	req.mu.Unlock()

	log.Printf("手动解锁请求 id=%s key_hint=%s", reqID, keyHint)
	return nil
}

func getUpdates(baseURL string, offset int64) ([]Update, int64, error) {
	url := fmt.Sprintf("%s/getUpdates?timeout=25&offset=%d", baseURL, offset)
	resp, err := http.Get(url)
	if err != nil {
		return nil, offset, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, offset, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, offset, fmt.Errorf("getUpdates 状态码=%d", resp.StatusCode)
	}
	var parsed UpdateResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, offset, err
	}
	if !parsed.OK {
		return nil, offset, errors.New("Telegram 返回 ok=false")
	}
	next := offset
	for _, u := range parsed.Result {
		if u.UpdateID >= next {
			next = u.UpdateID + 1
		}
	}
	return parsed.Result, next, nil
}

func unlockKey(passphrase, keyHint string) (bool, error) {
	// 第一步：验证密码正确性（使用 gpg --sign）
	probe, err := os.CreateTemp("", "gpg-unlock-probe-*.txt")
	if err != nil {
		return false, err
	}
	probePath := probe.Name()
	if _, err := probe.WriteString("unlock probe\n"); err != nil {
		_ = os.Remove(probePath)
		return false, err
	}
	_ = probe.Close()
	defer os.Remove(probePath)

	args := []string{"--batch", "--yes", "--pinentry-mode", "loopback", "--passphrase", passphrase}
	if strings.TrimSpace(keyHint) != "" {
		args = append(args, "--local-user", keyHint)
	}
	args = append(args, "--output", "/dev/null", "--sign", probePath)
	cmd := exec.Command("gpg", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("gpg 解锁失败: %s", strings.TrimSpace(string(out)))
	}

	// 第二步：将密码预设到 GPG agent 缓存
	// 获取目标密钥的所有 keygrip（主密钥和所有子密钥）
	var keyInfoCmd *exec.Cmd
	if strings.TrimSpace(keyHint) != "" {
		// 如果指定了 key_hint，只获取该密钥的 keygrip
		keyInfoCmd = exec.Command("gpg", "--with-keygrip", "--list-secret-keys", keyHint)
	} else {
		// 否则获取默认密钥的 keygrip
		keyInfoCmd = exec.Command("gpg", "--with-keygrip", "--list-secret-keys")
	}
	keyInfoOut, err := keyInfoCmd.CombinedOutput()
	if err != nil {
		log.Printf("警告：无法获取密钥列表: %v", err)
		return true, nil // 密码验证已成功，继续
	}

	// 从 gpg --with-keygrip 输出中提取 keygrip
	// 格式：
	//   sec   rsa4096/KEYID ...
	//         Keygrip = XXXXXXXX...
	//   ssb   rsa4096/KEYID ...
	//         Keygrip = YYYYYYYY...
	var keygrips []string
	lines := strings.Split(string(keyInfoOut), "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "Keygrip = ") {
			keygrip := strings.TrimPrefix(trimmed, "Keygrip = ")
			keygrips = append(keygrips, keygrip)
		}
	}

	if len(keygrips) == 0 {
		log.Printf("警告：未找到目标密钥的 keygrip")
		return true, nil
	}

	// 将密码转换为十六进制（PRESET_PASSPHRASE 要求）
	hexPass := fmt.Sprintf("%X", []byte(passphrase))

	// 预设目标密钥的所有 keygrip
	presetCount := 0
	for _, keygrip := range keygrips {
		// 使用 PRESET_PASSPHRASE 命令：PRESET_PASSPHRASE <keygrip> <timeout> <hexstring>
		// timeout -1 表示使用默认缓存时间
		presetCmd := exec.Command("gpg-connect-agent")
		presetCmd.Stdin = strings.NewReader(fmt.Sprintf("PRESET_PASSPHRASE %s -1 %s\n/bye\n", keygrip, hexPass))
		if err := presetCmd.Run(); err == nil {
			presetCount++
		}
	}

	if presetCount > 0 {
		log.Printf("已预设 %d 个密钥的密码到 GPG agent 缓存", presetCount)
	} else {
		log.Printf("警告：未能预设密码到 GPG agent（可能需要在 gpg-agent.conf 中启用 allow-preset-passphrase）")
	}

	return true, nil
}

func editStatusMessage(baseURL string, chatID, messageID int64, cmd, status string) error {
	esc := strings.ReplaceAll(cmd, "`", "\\`")
	text := fmt.Sprintf("调用信息: `%s`\n\n%s", esc, status)
	prefix := "🔑 GPG 调用审批"
	if strings.HasPrefix(cmd, "ssh ") {
		prefix = "🔐 SSH 调用审批"
	} else if strings.HasPrefix(cmd, "sudo ") {
		prefix = "🛡️ SUDO 调用审批"
	}
	text = prefix + "\n\n" + text
	return postJSON(baseURL+"/editMessageText", map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"parse_mode": "Markdown",
		"text":       text,
	})
}

func sendPendingPromptMessage(baseURL string, chatID int64) (int64, error) {
	respBody, err := postJSONRead(baseURL+"/sendMessage", map[string]any{
		"chat_id":    chatID,
		"parse_mode": "Markdown",
		"text":       "🔴 *请输入密码（发送后自动删除）*",
	})
	if err != nil {
		return 0, err
	}
	var parsed tgSendMessageResp
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return 0, err
	}
	if !parsed.OK || parsed.Result.MessageID == 0 {
		return 0, errors.New("sendMessage 未返回有效 message_id")
	}
	return parsed.Result.MessageID, nil
}

func editPendingPromptMessage(baseURL string, chatID int64, req *approvalReq, text string) error {
	req.mu.Lock()
	messageID := req.PromptID
	req.mu.Unlock()
	if messageID <= 0 {
		return nil
	}
	return postJSON(baseURL+"/editMessageText", map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"parse_mode": "Markdown",
		"text":       "🔴 *" + strings.ReplaceAll(text, "*", "\\*") + "*",
	})
}

func deletePendingPromptMessage(baseURL string, chatID int64, req *approvalReq) error {
	req.mu.Lock()
	messageID := req.PromptID
	req.PromptID = 0
	req.mu.Unlock()
	if messageID <= 0 {
		return nil
	}
	return deleteMessage(baseURL, chatID, messageID)
}

func deleteMessage(baseURL string, chatID, messageID int64) error {
	if messageID <= 0 {
		return nil
	}
	return postJSON(baseURL+"/deleteMessage", map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
	})
}

func postJSON(url string, payload map[string]any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s 状态码=%d body=%s", url, resp.StatusCode, string(body))
	}
	return nil
}

func postJSONRead(url string, payload map[string]any) ([]byte, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("POST %s 状态码=%d", url, resp.StatusCode)
	}
	return body, nil
}

func writeJSON(w http.ResponseWriter, v map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func shortID(prefix string) string {
	now := time.Now().UnixNano()
	id := fmt.Sprintf("%x", now)
	if len(id) > 8 {
		id = id[len(id)-8:]
	}
	if prefix == "ssh" {
		return "ssh-" + id
	}
	return id
}

func envInt(name string, defaultValue int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return defaultValue
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("环境变量 %s 不是整数，使用默认值 %d", name, defaultValue)
		return defaultValue
	}
	return v
}
