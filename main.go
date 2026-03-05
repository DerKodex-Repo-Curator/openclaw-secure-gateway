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
	Kind      string // gpg | ssh
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
}

func newManager() *approvalManager {
	return &approvalManager{requests: make(map[string]*approvalReq)}
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
		if kind != "gpg" && kind != "ssh" {
			http.Error(w, "kind must be gpg or ssh", http.StatusBadRequest)
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

func sendApprovalMessage(baseURL string, chatID int64, req *approvalReq, timeout int) (int64, error) {
	emoji := "🔑"
	title := "GPG"
	if req.Kind == "ssh" {
		emoji = "🔐"
		title = "SSH"
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

	if req.Kind == "ssh" {
		if req.transition("approved") {
			_ = editStatusMessage(baseURL, allowedChatID, req.MessageID, req.Cmd, "✅ 已批准")
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
	defer func() { _ = deleteMessage(baseURL, msg.Chat.ID, msg.MessageID) }()

	passphrase := msg.Text
	if strings.TrimSpace(passphrase) == "" {
		return nil
	}

	req := mgr.latestPendingPassword()
	if req == nil {
		return nil
	}

	ok, err := unlockKey(passphrase, req.KeyHint)
	if !ok {
		if err != nil {
			log.Printf("解锁失败 request_id=%s err=%v", req.ID, err)
		}
		_ = editPendingPromptMessage(baseURL, allowedChatID, req, "❌ 密码验证失败，请重新输入（发送后自动删除）")
		return nil
	}
	if req.transition("approved") {
		_ = editStatusMessage(baseURL, allowedChatID, req.MessageID, req.Cmd, "✅ 已成功")
	}
	_ = deletePendingPromptMessage(baseURL, allowedChatID, req)
	log.Printf("解锁成功 request_id=%s", req.ID)
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
	return true, nil
}

func editStatusMessage(baseURL string, chatID, messageID int64, cmd, status string) error {
	esc := strings.ReplaceAll(cmd, "`", "\\`")
	text := fmt.Sprintf("调用信息: `%s`\n\n%s", esc, status)
	prefix := "🔑 GPG 调用审批"
	if strings.HasPrefix(cmd, "ssh ") {
		prefix = "🔐 SSH 调用审批"
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
