package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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

type UnlockRequest struct {
	RequestID string `json:"request_id"`
	Cmd       string `json:"cmd"`
	KeyHint   string `json:"key_hint"`
	CreatedAt int64  `json:"created_at"`
	MessageID int64  `json:"message_id"`
}

type tgSendMessageResp struct {
	OK     bool `json:"ok"`
	Result struct {
		MessageID int64 `json:"message_id"`
	} `json:"result"`
}

func main() {
	token := strings.TrimSpace(os.Getenv("GPG_APPROVAL_BOT_TOKEN"))
	chatIDStr := strings.TrimSpace(os.Getenv("GPG_APPROVAL_CHAT_ID"))
	approvalDir := strings.TrimSpace(os.Getenv("APPROVAL_DIR"))
	if approvalDir == "" {
		approvalDir = "/approvals"
	}
	if token == "" || chatIDStr == "" {
		log.Fatal("缺少独立的 GPG_APPROVAL_BOT_TOKEN 或 GPG_APPROVAL_CHAT_ID")
	}
	allowedChatID, err := strconv.ParseInt(chatIDStr, 10, 64)
	if err != nil {
		log.Fatalf("GPG_APPROVAL_CHAT_ID 非法: %v", err)
	}
	if err := os.MkdirAll(approvalDir, 0o700); err != nil {
		log.Fatalf("创建审批目录失败: %v", err)
	}

	baseURL := "https://api.telegram.org/bot" + token
	offset := int64(0)
	log.Printf("gpg-approval-bot 启动，审批目录=%s", approvalDir)

	for {
		if err := promptPendingUnlockRequests(baseURL, allowedChatID, approvalDir); err != nil {
			log.Printf("扫描解锁请求失败: %v", err)
		}

		updates, nextOffset, err := getUpdates(baseURL, offset)
		if err != nil {
			log.Printf("getUpdates 失败: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		offset = nextOffset
		for _, upd := range updates {
			if upd.CallbackQuery != nil {
				if err := handleCallback(baseURL, upd.CallbackQuery, allowedChatID, approvalDir); err != nil {
					log.Printf("处理回调失败: %v", err)
				}
			}
			if upd.Message != nil {
				if err := handleMessage(baseURL, upd.Message, allowedChatID, approvalDir); err != nil {
					log.Printf("处理消息失败: %v", err)
				}
			}
		}
	}
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
		return nil, offset, fmt.Errorf("getUpdates 状态码=%d body=%s", resp.StatusCode, string(body))
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

func handleCallback(baseURL string, cb *CallbackQuery, allowedChatID int64, approvalDir string) error {
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
	if action != "approve" && action != "deny" {
		return fmt.Errorf("未知 action: %s", action)
	}

	respPath := filepath.Join(approvalDir, requestID)
	if err := os.WriteFile(respPath, []byte(action), 0o600); err != nil {
		return fmt.Errorf("写审批结果失败: %w", err)
	}
	log.Printf("callback 已写审批结果 request_id=%s action=%s", requestID, action)

	statusText := "✅ 已允许"
	statusMark := "✅"
	statusAction := "已批准"
	if action == "deny" {
		statusText = "❌ 已拒绝"
		statusMark = "❌"
		statusAction = "已拒绝"
	}
	_ = postJSON(baseURL+"/answerCallbackQuery", map[string]any{
		"callback_query_id": cb.ID,
		"text":              statusText,
	})

	baseText := extractMessageTop(cb.Message.Text)
	newText := baseText + "\n\n" + statusMark + " " + statusAction
	_ = postJSON(baseURL+"/editMessageText", map[string]any{
		"chat_id":    cb.Message.Chat.ID,
		"message_id": cb.Message.MessageID,
		"text":       newText,
	})
	return nil
}

func handleMessage(baseURL string, msg *TGMessage, allowedChatID int64, approvalDir string) error {
	if msg.Chat.ID != allowedChatID {
		return nil
	}
	// 中文注释：用户发送的任何消息都立即删除，避免口令或其他内容残留。
	defer func() {
		_ = deleteMessage(baseURL, msg.Chat.ID, msg.MessageID)
	}()

	req, reqPath, sentMarkPath, hasPending, err := findPendingUnlockRequest(approvalDir)
	if err != nil {
		return err
	}
	if !hasPending {
		log.Printf("收到文本消息但当前无待解锁请求")
		return nil
	}
	log.Printf("收到文本消息，将作为解锁口令 request_id=%s", req.RequestID)

	passphrase := msg.Text
	if strings.TrimSpace(passphrase) == "" {
		return nil
	}

	ok, err := unlockKey(passphrase, req.KeyHint)
	if !ok {
		// 中文注释：解锁失败时静默等待下一次输入，不写失败结果，允许重试。
		if err != nil {
			log.Printf("解锁失败 request_id=%s", req.RequestID)
		}
		return nil
	}
	log.Printf("解锁成功 request_id=%s", req.RequestID)

	resultPath := filepath.Join(approvalDir, "unlock_"+req.RequestID+".result")
	if writeErr := os.WriteFile(resultPath, []byte("unlocked"), 0o600); writeErr != nil {
		return writeErr
	}
	// 中文注释：解锁成功后继续编辑原审批消息，不额外发送新消息。
	_ = req.MessageID
	_ = os.Remove(reqPath)
	_ = os.Remove(sentMarkPath)
	return nil
}

func promptPendingUnlockRequests(baseURL string, chatID int64, approvalDir string) error {
	matches, err := filepath.Glob(filepath.Join(approvalDir, "unlock_*.json"))
	if err != nil {
		return err
	}
	for _, path := range matches {
		mark := path + ".sent"
		if _, err := os.Stat(mark); err == nil {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var req UnlockRequest
		if err := json.Unmarshal(b, &req); err != nil {
			continue
		}
		if req.MessageID <= 0 {
			log.Printf("跳过 PENDING 更新：message_id 缺失 request_id=%s", req.RequestID)
			continue
		}
		text := buildStateMessage(req.Cmd, "🟡 已批准，但需要密码（PENDING）\n请输入密码（发送后自动删除）")
		if req.KeyHint != "" {
			text += fmt.Sprintf("\n密钥提示: %s", req.KeyHint)
		}
		if err := postJSON(baseURL+"/editMessageText", map[string]any{
			"chat_id":    chatID,
			"message_id": req.MessageID,
			"parse_mode": "Markdown",
			"text":       text,
		}); err != nil {
			log.Printf("更新 PENDING 失败 request_id=%s", req.RequestID)
			continue
		}
		log.Printf("已更新为 PENDING request_id=%s message_id=%d", req.RequestID, req.MessageID)
		_ = os.WriteFile(mark, []byte("1"), 0o600)
	}
	return nil
}

func findPendingUnlockRequest(approvalDir string) (UnlockRequest, string, string, bool, error) {
	matches, err := filepath.Glob(filepath.Join(approvalDir, "unlock_*.json"))
	if err != nil {
		return UnlockRequest{}, "", "", false, err
	}
	if len(matches) == 0 {
		return UnlockRequest{}, "", "", false, nil
	}
	sort.Strings(matches)
	reqPath := matches[0]
	b, err := os.ReadFile(reqPath)
	if err != nil {
		return UnlockRequest{}, "", "", false, err
	}
	var req UnlockRequest
	if err := json.Unmarshal(b, &req); err != nil {
		return UnlockRequest{}, "", "", false, err
	}
	return req, reqPath, reqPath + ".sent", true, nil
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

func sendMessageAndGetID(baseURL string, chatID int64, text string) (int64, error) {
	respBody, err := postJSONRead(baseURL+"/sendMessage", map[string]any{
		"chat_id": chatID,
		"text":    text,
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

func deleteMessage(baseURL string, chatID, messageID int64) error {
	if messageID <= 0 {
		return nil
	}
	return postJSON(baseURL+"/deleteMessage", map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
	})
}

func readPromptMessageID(markPath string) int64 {
	b, err := os.ReadFile(markPath)
	if err != nil {
		return 0
	}
	id, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0
	}
	return id
}

func extractMessageTop(text string) string {
	parts := strings.Split(text, "\n\n请在")
	base := parts[0]
	for _, marker := range []string{"\n\n✅ ", "\n\n❌ ", "\n\n🟡 "} {
		if idx := strings.Index(base, marker); idx >= 0 {
			base = base[:idx]
		}
	}
	lines := strings.Split(base, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.Contains(line, "请在") || strings.Contains(line, "请输入密码") {
			continue
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func buildStateMessage(cmd, status string) string {
	esc := strings.ReplaceAll(cmd, "`", "\\`")
	top := fmt.Sprintf("🔑 GPG 调用审批\n\n调用信息: `%s`", esc)
	return top + "\n\n" + status
}

func formatElapsedFromMessage(messageDate int64) string {
	if messageDate <= 0 {
		return "0分0秒"
	}
	now := time.Now().Unix()
	if now < messageDate {
		return "0分0秒"
	}
	elapsed := now - messageDate
	mins := elapsed / 60
	secs := elapsed % 60
	return fmt.Sprintf("%d分%d秒", mins, secs)
}
