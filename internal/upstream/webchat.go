// webchat.go 国际版每日活跃打卡（网页通道 agent 会话）。
//
// 官方规则：国际版账号「通过客户端发起有效对话」可领每日活跃 30 积分（Pro 50）。
// 实测（参考 workbuddy2api-hub issue #75/#59/#90）：桌面身份的 chat/completions
// 不计数，网页通道的 agent 会话才算——且建会话只是**排队**，必须接上沙箱并请求
// 这一轮才会真的跑完，否则永远停在 CREATING、不加分。
//
// 完整链路（与网页端逐字对齐，全部只带 Bearer accessToken + X-User-Id 两个凭据头，
// 无桌面端 X-IDE-* 指纹，复用账号现有凭证即可）：
//
//	POST {global-base}/console/as/conversations/          建会话（排队）→ data.id
//	GET  {global-base}/console/as/conversations/{id}/session → data.link/token/sessionId/cwd
//	GET  {link}（Accept: text/event-stream）               接沙箱 SSE，响应头 Acp-Connection-Id
//	POST {link} ×3（带 Acp-Connection-Id）                 initialize → session/load → session/prompt
//	GET  {global-base}/console/as/conversations/{id}      轮询 data.status 到 completed
//
// 代价比照 hub：会真实起一次任务、消耗少量积分（远小于 30/50 的奖励）。
// 每号每日一次由 scheduler 的 webchat 任务闸门负责，本文件不做幂等。
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"workbuddy2api/internal/auth"
)

// 网页通道常量（对齐 hub wb_accounts.py 抓包值）。
const (
	webConversationsPath = "/console/as/conversations/"
	webUserAgent         = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36 Edg/140.0.0.0"
	// webChatModelDefault 打卡用最便宜的模型（hub 实测值）；上游下架时用
	// global.webchat_model 覆盖，不必改代码。
	webChatModelDefault = "deepseek-v4.1-flash"
	// webChatPromptDefault 打卡提示词：agent 自己把话说完即可。
	webChatPromptDefault = "Hi"
	// webStepTimeout 建会话/取 session/ACP POST 的单步超时。
	webStepTimeout = 30 * time.Second
)

// webTurnTimeout / webPollInterval 用 var（const 块外）：turn 可被
// Client.WebChatTurnTimeout 覆盖，interval 测试要缩短——真实值见上注释。
var (
	webTurnTimeout = 120 * time.Second
	webPollInterval = 3 * time.Second
)

// acpProtocolVersion / acpClientCapabilities ACP initialize 参数：打卡只需要
// agent 自己把话说完，不提供文件系统/终端能力（hub 同款）。
const acpProtocolVersion = 1

var acpClientCapabilities = map[string]any{
	"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
	"terminal": false,
}

// WebChatResult 单次网页通道打卡的结果（scheduler 日志与测试断言用）。
// Updates/Chunks 用 int32：读循环 goroutine 以 atomic 累加（JSON 序列化不受影响）。
type WebChatResult struct {
	Conversation string `json:"conversation"` // 会话 id
	Status       string `json:"status"`       // 完成态（completed）
	Chunks       int32  `json:"chunks"`       // agent 输出段数（0 也可能成功，留观测）
	Updates      int32  `json:"updates"`      // session/update 事件数
	ElapsedMS    int64  `json:"elapsed_ms"`
}

// WebChatEligible 账号是否参与网页通道打卡：global.enabled 且 realm=global
// （globalOn 双闸——逃生门关闭时 global 账号按 CN 处理，绝不打网页通道）。
func (c *Client) WebChatEligible(a *auth.Auth) bool { return c.globalOn(a) }

// webChatModel / webChatPrompt 生效的打卡模型与提示词（config 覆盖，空回落默认）。
func (c *Client) webChatModel() string {
	if c.WebChatModel != "" {
		return c.WebChatModel
	}
	return webChatModelDefault
}

func (c *Client) webChatPrompt() string {
	if c.WebChatPrompt != "" {
		return c.WebChatPrompt
	}
	return webChatPromptDefault
}

// webHeaders 网页通道出站头：Bearer accessToken + X-User-Id + 网页端 UA，
// Origin/Referer 用 global chat base（与网页端同源）。无桌面端指纹。
func (c *Client) webHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", c.globalChatBase())
	req.Header.Set("Referer", c.globalChatBase()+"/app")
	req.Header.Set("User-Agent", webUserAgent)
}

// webEnvelopeJSON 网页通道 {code,msg,data} 信封调用（复用 doJSON 的信封解析，
// 但用本文件自己的请求头——console/as 域不走 billing/CLI 头族）。
func (c *Client) webEnvelopeJSON(ctx context.Context, a *auth.Auth, method, fullURL string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, rdr)
	if err != nil {
		return nil, err
	}
	c.webHeaders(req, a)
	return c.doJSON(req)
}

// WebDailyCheckin 跑完一次网页通道每日活跃会话（建会话 → 沙箱 → ACP → 轮询到 completed）。
// 失败时错误里尽量带上会话 id（运维要能去网页端对账）。每号每日一次的幂等闸在 scheduler。
func (c *Client) WebDailyCheckin(a *auth.Auth) (*WebChatResult, error) {
	if !c.globalOn(a) {
		return nil, fmt.Errorf("webchat: 仅限国际版账号（global.enabled 且 realm=global）")
	}
	if a.AccessTokenValue() == "" {
		return nil, fmt.Errorf("webchat: 账号无 accessToken")
	}
	started := time.Now()
	base := c.globalChatBase()
	prompt, model := c.webChatPrompt(), c.webChatModel()

	// 1. 建会话（只是排队）。
	createBody := map[string]any{
		"prompt":             prompt,
		"model":              model,
		"conversationOrigin": "workbuddy-app",
		// 网页端建会话固定带这两项（抓包所得），保持请求形态一致。
		"plugins": []map[string]string{{"name": "weixinpay", "marketplace": "codebuddy-builtin"}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), webStepTimeout)
	data, err := c.webEnvelopeJSON(ctx, a, http.MethodPost, base+webConversationsPath, createBody)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("webchat: create conversation: %w", err)
	}
	var cr struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &cr); err != nil || cr.ID == "" {
		return nil, fmt.Errorf("webchat: create conversation: missing id (data: %s)", truncate(string(data), 120))
	}
	conv := cr.ID

	// 2. 取沙箱 link+token（没有它会话永远停在 CREATING）。
	ctx, cancel = context.WithTimeout(context.Background(), webStepTimeout)
	data, err = c.webEnvelopeJSON(ctx, a, http.MethodGet, base+webConversationsPath+conv+"/session", nil)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("webchat %s: session query: %w", conv, err)
	}
	var ss struct {
		Link      string `json:"link"`
		Endpoint  string `json:"endpoint"`
		Token     string `json:"token"`
		SessionID string `json:"sessionId"`
		Cwd       string `json:"cwd"`
	}
	if err := json.Unmarshal(data, &ss); err != nil {
		return nil, fmt.Errorf("webchat %s: session parse: %w", conv, err)
	}
	link := firstNonEmptyStr(ss.Link, ss.Endpoint)
	if link == "" || ss.Token == "" {
		return nil, fmt.Errorf("webchat %s: sandbox not ready (no link/token)", conv)
	}
	sessionID := firstNonEmptyStr(ss.SessionID, conv)
	cwd := ss.Cwd
	if cwd == "" {
		cwd = "/workspace"
	}

	// 3-5. 接沙箱 + ACP 三步 + 轮询到 completed。
	res, err := c.webAcpTurn(a, base, conv, link, ss.Token, sessionID, cwd, prompt)
	if err != nil {
		return nil, fmt.Errorf("webchat %s: %w", conv, err)
	}
	res.Conversation = conv
	res.ElapsedMS = time.Since(started).Milliseconds()
	return res, nil
}

// firstNonEmptyStr 返回第一个非空串（全空返回 ""）。
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// webAcpTurn ACP over streamable-HTTP：SSE 通道收事件、POST 发 JSON-RPC 请求，
// 请求顺序 initialize → session/load → session/prompt（顺序错了沙箱不认），
// 然后轮询会话状态到 completed（事件只是观测，完成判定以状态接口为准）。
func (c *Client) webAcpTurn(a *auth.Auth, base, conv, link, token, sessionID, cwd, prompt string) (*WebChatResult, error) {
	res := &WebChatResult{Status: ""}
	turn := webTurnTimeout
	if c.WebChatTurnTimeout > 0 {
		turn = c.WebChatTurnTimeout
	}
	poll := webPollInterval
	if c.WebChatPollInterval > 0 {
		poll = c.WebChatPollInterval
	}
	ctx, cancel := context.WithTimeout(context.Background(), turn)
	defer cancel()

	// 3. 打开 SSE 通道，拿 Acp-Connection-Id（不接流，POST 发不出去）。
	sseReq, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, fmt.Errorf("acp open: %w", err)
	}
	sseReq.Header.Set("Accept", "text/event-stream")
	sseReq.Header.Set("Authorization", "Bearer "+token)
	sseReq.Header.Set("User-Agent", webUserAgent)
	sseResp, err := c.chatHTTP().Do(sseReq)
	if err != nil {
		return nil, fmt.Errorf("acp open: %w", err)
	}
	if sseResp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(sseResp.Body, 4<<10))
		sseResp.Body.Close()
		return nil, fmt.Errorf("acp open: http %d", sseResp.StatusCode)
	}
	connID := sseResp.Header.Get("Acp-Connection-Id")
	if connID == "" {
		io.Copy(io.Discard, io.LimitReader(sseResp.Body, 4<<10))
		sseResp.Body.Close()
		return nil, fmt.Errorf("acp open: no Acp-Connection-Id")
	}
	// 后台读事件流：只数 session/update / agent_message_chunk 留观测；
	// ctx 到期或 Body.Close 使读循环自然退出（事件内容不参与完成判定）。
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		sc := bufio.NewScanner(sseResp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(line[len("data:"):])
			if payload == "" || payload == "[DONE]" {
				continue
			}
			var msg struct {
				Method string `json:"method"`
				Params struct {
					Update struct {
						SessionUpdate string `json:"sessionUpdate"`
					} `json:"update"`
				} `json:"params"`
			}
			if json.Unmarshal([]byte(payload), &msg) != nil {
				continue
			}
			if msg.Method == "session/update" {
				atomic.AddInt32(&res.Updates, 1)
				if msg.Params.Update.SessionUpdate == "agent_message_chunk" {
					atomic.AddInt32(&res.Chunks, 1)
				}
			}
		}
	}()
	// 从这里起任何返回路径都收掉 SSE：先关 Body（读循环随即退出）再等 goroutine，
	// 之后外层的 defer cancel() 才跑（LIFO）——不泄漏连接与 goroutine。
	defer func() {
		sseResp.Body.Close()
		<-readerDone
	}()

	// 4. JSON-RPC 三连（每步独立短连接 + 单步超时，hub 同款；顺序错了沙箱不认）。
	acpPost := func(method string, params any, id int) error {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		stepCtx, stepCancel := context.WithTimeout(ctx, webStepTimeout)
		defer stepCancel()
		req, err := http.NewRequestWithContext(stepCtx, http.MethodPost, link, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Acp-Connection-Id", connID)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("User-Agent", webUserAgent)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
			return fmt.Errorf("%s: http %d", method, resp.StatusCode)
		}
		return nil
	}
	steps := []struct {
		method string
		params any
	}{
		{"initialize", map[string]any{"protocolVersion": acpProtocolVersion, "clientCapabilities": acpClientCapabilities}},
		{"session/load", map[string]any{"sessionId": sessionID, "cwd": cwd, "mcpServers": []any{}}},
		{"session/prompt", map[string]any{"sessionId": sessionID, "prompt": []map[string]string{{"type": "text", "text": prompt}}}},
	}
	for i, st := range steps {
		if err := acpPost(st.method, st.params, i+1); err != nil {
			return nil, fmt.Errorf("acp %s: %w", st.method, err)
		}
	}

	// 5. 轮询会话状态到 completed（hub 口径：completed 即跑完；failed/error 即失败；
	// 超时未完成报错——上报型观测里 chunks 可能为 0，但状态接口没到 completed 就不算有效对话）。
	statusFn := func() string {
		sctx, scancel := context.WithTimeout(ctx, webStepTimeout)
		defer scancel()
		data, err := c.webEnvelopeJSON(sctx, a, http.MethodGet, base+webConversationsPath+conv, nil)
		if err != nil {
			return "" // 单次查询失败不终止等待（网络抖动），交给 deadline
		}
		var st struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(data, &st)
		return st.Status
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	defer func() {
		sseResp.Body.Close()
		<-readerDone
	}()
	for {
		select {
		case <-ctx.Done():
			st := statusFn()
			return nil, fmt.Errorf("turn not completed in %s (status=%s)", turn, orDefault(st, "未知"))
		case <-ticker.C:
			st := statusFn()
			res.Status = st
			switch st {
			case "completed":
				return res, nil
			case "failed", "error":
				return nil, fmt.Errorf("conversation status=%s", st)
			}
		}
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
