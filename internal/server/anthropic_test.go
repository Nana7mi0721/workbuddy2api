package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// ---------------------------------------------------------------------------
// 多协议入站测试共享的夹具（/v1/messages、/v1/responses）
// ---------------------------------------------------------------------------

// captureUpstream 返回 fake upstream，并把打到上游的（prepareBody 之后的）请求体
// 记录到 *got，供入向转换断言。
func captureUpstream(t *testing.T, got *string, status int, body string, isStream bool) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			*got = string(b)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// sseEv 一条 SSE 事件（event: 名 + data: 载荷）。
type sseEv struct{ event, data string }

// parseSSE 解析 "event: X\ndata: Y\n\n" 序列（多协议流式翻译的输出形态）。
func parseSSE(t *testing.T, body string) []sseEv {
	t.Helper()
	var out []sseEv
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "event: "):
			out = append(out, sseEv{event: strings.TrimPrefix(line, "event: ")})
		case strings.HasPrefix(line, "data: "):
			if len(out) > 0 {
				out[len(out)-1].data = strings.TrimPrefix(line, "data: ")
			}
		}
	}
	return out
}

// sseToolOK 带 tool_call 分片的 SSE 夹具（两段 arguments 分片 + finish_reason=tool_calls
// + 末帧 usage）。
const sseToolOK = "data: {\"id\":\"chatcmpl-2\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-2\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\"}}]}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-2\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"杭州\\\"}\"}}]}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-2\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n" +
	"data: [DONE]\n\n"

const anthropicBaseReq = `{"model":"glm-5.2","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}`

// ---------------------------------------------------------------------------
// /v1/messages handler 级
// ---------------------------------------------------------------------------

// TestAnthropicMessagesSync 非流式：聚合结果转 Anthropic message 对象
// （content text 块、model 回显、usage 映射、stop_reason）。
func TestAnthropicMessagesSync(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(anthropicBaseReq)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not json: %v body=%s", err, rec.Body)
	}
	if out["type"] != "message" || out["role"] != "assistant" {
		t.Errorf("type/role = %v/%v want message/assistant", out["type"], out["role"])
	}
	if out["model"] != "glm-5.2" {
		t.Errorf("model echo = %v want glm-5.2", out["model"])
	}
	content, ok := out["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content missing: %v", out["content"])
	}
	blk := content[0].(map[string]any)
	if blk["type"] != "text" || blk["text"] != "你好" {
		t.Errorf("text block = %v want 你好", blk)
	}
	if out["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v want end_turn", out["stop_reason"])
	}
	usage := out["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 1 || usage["output_tokens"].(float64) != 1 {
		t.Errorf("usage = %v want 1/1", usage)
	}
	// OpenAI 信封的兄弟路径不受影响：响应头应带服务账号。
	if rec.Header().Get("X-Wb-Account") != "u1" {
		t.Errorf("X-Wb-Account = %q want u1", rec.Header().Get("X-Wb-Account"))
	}
}

// TestAnthropicMessagesStream 流式：事件序列与语法（message_start → ping →
// content_block_start → text_delta → content_block_stop → message_delta → message_stop）。
func TestAnthropicMessagesStream(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	evs := parseSSE(t, rec.Body.String())
	want := []string{"message_start", "ping", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if len(evs) != len(want) {
		t.Fatalf("events = %d (%v) want %d", len(evs), evNames(evs), len(want))
	}
	for i, w := range want {
		if evs[i].event != w {
			t.Fatalf("event[%d] = %q want %q (all=%v)", i, evs[i].event, w, evNames(evs))
		}
	}
	var start struct {
		Message struct {
			ID    string `json:"id"`
			Model string `json:"model"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(evs[0].data), &start); err != nil || !strings.HasPrefix(start.Message.ID, "msg_") {
		t.Errorf("message_start = %s err=%v", evs[0].data, err)
	} else if start.Message.Model != "glm-5.2" {
		t.Errorf("message_start model = %q", start.Message.Model)
	}
	var delta struct {
		Index int `json:"index"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	}
	if err := json.Unmarshal([]byte(evs[3].data), &delta); err != nil {
		t.Fatalf("delta: %v", err)
	}
	if delta.Index != 0 || delta.Delta.Type != "text_delta" || delta.Delta.Text != "你好" {
		t.Errorf("text delta = %+v", delta)
	}
	var md struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Usage struct {
			OutputTokens int `json:"output_tokens"`
			InputTokens  int `json:"input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(evs[5].data), &md); err != nil {
		t.Fatalf("message_delta: %v", err)
	}
	if md.Delta.StopReason != "end_turn" || md.Usage.OutputTokens != 1 || md.Usage.InputTokens != 1 {
		t.Errorf("message_delta = %+v", md)
	}
}

// TestAnthropicMessagesToolUseStream 工具调用流式：tool_call 分片 → tool_use 块
// （input_json_delta），stop_reason=tool_use。
func TestAnthropicMessagesToolUseStream(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseToolOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"weather?"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	evs := parseSSE(t, rec.Body.String())
	names := evNames(evs)
	if len(names) < 7 || names[2] != "content_block_start" || names[3] != "content_block_delta" {
		t.Fatalf("events = %v", names)
	}
	var start struct {
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
	}
	if err := json.Unmarshal([]byte(evs[2].data), &start); err != nil {
		t.Fatalf("block start: %v", err)
	}
	if start.ContentBlock.Type != "tool_use" || start.ContentBlock.ID != "call_1" || start.ContentBlock.Name != "get_weather" {
		t.Errorf("tool block start = %+v", start.ContentBlock)
	}
	// 末三事件：content_block_stop / message_delta(stop_reason=tool_use) / message_stop。
	tail := names[len(names)-3:]
	if tail[0] != "content_block_stop" || tail[1] != "message_delta" || tail[2] != "message_stop" {
		t.Errorf("tail events = %v", tail)
	}
	var md struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
	}
	if err := json.Unmarshal([]byte(evs[len(evs)-2].data), &md); err != nil || md.Delta.StopReason != "tool_use" {
		t.Errorf("stop_reason = %+v err=%v", md, err)
	}
}

// TestAnthropicMessagesToolUseSync 非流式工具调用：tool_calls → tool_use 块（input 解析）。
func TestAnthropicMessagesToolUseSync(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseToolOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(anthropicBaseReq)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	content := out["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v (empty text block must not be emitted)", content)
	}
	blk := content[0].(map[string]any)
	if blk["type"] != "tool_use" || blk["name"] != "get_weather" || blk["id"] != "call_1" {
		t.Errorf("tool block = %v", blk)
	}
	input, ok := blk["input"].(map[string]any)
	if !ok || input["city"] != "杭州" {
		t.Errorf("tool input = %v", blk["input"])
	}
	if out["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason = %v want tool_use", out["stop_reason"])
	}
}

// TestAnthropicAuthViaXAPIKey Anthropic 客户端用 x-api-key 头鉴权；401 回
// Anthropic 信封（authentication_error）；Bearer 路径不受影响。
func TestAnthropicAuthViaXAPIKey(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "sk-test"})

	// 无密钥 → 401 + Anthropic 信封。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(anthropicBaseReq)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401", rec.Code)
	}
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Type != "error" || env.Error.Type != "authentication_error" {
		t.Errorf("401 envelope = %s (err=%v)", rec.Body, err)
	}

	// x-api-key 正确 → 200。
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(anthropicBaseReq))
	req.Header.Set("x-api-key", "sk-test")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("x-api-key code=%d body=%s", rec.Code, rec.Body)
	}

	// Bearer 亦可用（OpenAI 客户端同表鉴权不受影响）。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/messages", strings.NewReader(anthropicBaseReq))
	req.Header.Set("Authorization", "Bearer sk-test")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("bearer code=%d body=%s", rec.Code, rec.Body)
	}
}

// TestAnthropicUpstream429MapsToRateLimit 上游 429 → Anthropic 信封 rate_limit_error
// （不再轮转后的末端透传路径）。
func TestAnthropicUpstream429MapsToRateLimit(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		return 429, `{"code":429,"msg":"rate limited"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(anthropicBaseReq)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429 body=%s", rec.Code, rec.Body)
	}
	var env struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Error.Type != "rate_limit_error" {
		t.Errorf("429 envelope = %s (err=%v)", rec.Body, err)
	}
	if !strings.Contains(env.Error.Message, "rate limited") {
		t.Errorf("upstream原文 must passthrough, got %q", env.Error.Message)
	}
}

// TestAnthropicEmptyStreamSync502 空流（只有 [DONE]）且未发出字节 → 502 + Anthropic 信封。
func TestAnthropicEmptyStreamSync502(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, "data: [DONE]\n\n", true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	for _, stream := range []bool{false, true} {
		body := `{"model":"glm-5.2","max_tokens":8,"stream":` + map[bool]string{false: "false", true: "true"}[stream] + `,"messages":[{"role":"user","content":"hi"}]}`
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
		if rec.Code != http.StatusBadGateway {
			t.Errorf("stream=%v: code=%d want 502 body=%s", stream, rec.Code, rec.Body)
			continue
		}
		var env struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Error.Type != "api_error" {
			t.Errorf("stream=%v: 502 envelope = %s", stream, rec.Body)
		}
	}
}

// TestAnthropicMaxTokensRequired max_tokens 缺失 → 400（Anthropic 规范必填）。
func TestAnthropicMaxTokensRequired(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400", rec.Code, rec.Body)
	}
	var env struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Error.Type != "invalid_request_error" {
		t.Errorf("400 envelope = %s", rec.Body)
	}
}

// TestAnthropicCountTokens count_tokens：启发式估算，不打上游。
func TestAnthropicCountTokens(t *testing.T) {
	var called int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		called++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":8,"system":"you are helpful","messages":[{"role":"user","content":[{"type":"text","text":"hello world this is a test message"}]}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if called != 0 {
		t.Errorf("count_tokens must not hit upstream, calls=%d", called)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not json: %v", err)
	}
	n, ok := out["input_tokens"].(float64)
	if !ok || n < 5 {
		t.Errorf("input_tokens = %v want >5 estimate", out["input_tokens"])
	}
}

// TestAnthropicStickyOverrideByUserID metadata.user_id 作为粘性键：绑定应落在
// user_id 上（转换后的 OpenAI body 提取不出该字段，靠 stickyOverride 传入）。
func TestAnthropicStickyOverrideByUserID(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{TTL: time.Minute, Store: st})
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"glm-5.2","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"metadata":{"user_id":"user_abc_session_xyz"}}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	uid, ok := st.lastUID("user_abc_session_xyz")
	if !ok || uid != "u1" {
		t.Fatalf("sticky should bind metadata.user_id → u1, got %q ok=%v (binds=%v)", uid, ok, st.binds)
	}
}

// ---------------------------------------------------------------------------
// convertAnthropicRequest 单元级
// ---------------------------------------------------------------------------

// TestAnthropicRequestConversion 富请求入向转换：system / 文本+图片块 /
// thinking 剥离 / assistant tool_use / tool_result / tools / tool_choice /
// thinking budget → reasoning_effort / metadata.user_id。
func TestAnthropicRequestConversion(t *testing.T) {
	raw := `{
		"model": "claude-sonnet-4-5",
		"max_tokens": 1024,
		"system": "be nice",
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "hi"},
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "QUJD"}},
				{"type": "thinking", "thinking": "secret", "signature": "sig"}
			]},
			{"role": "assistant", "content": [
				{"type": "text", "text": "calling"},
				{"type": "tool_use", "id": "toolu_1", "name": "f", "input": {"a": 1}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "res"}
			]}
		],
		"tools": [{"name": "f", "description": "d", "input_schema": {"type": "object", "properties": {"a": {"type": "number"}}}}],
		"tool_choice": {"type": "any"},
		"thinking": {"type": "enabled", "budget_tokens": 20000},
		"stop_sequences": ["STOP"],
		"metadata": {"user_id": "user_abc"}
	}`
	body, sticky, fail := convertAnthropicRequest([]byte(raw), "cn:glm-5.3")
	if fail != nil {
		t.Fatalf("convert fail: %+v", fail)
	}
	if sticky != "user_abc" {
		t.Errorf("sticky override = %q want user_abc", sticky)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if out["model"] != "cn:glm-5.3" {
		t.Errorf("model = %v", out["model"])
	}
	if out["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v want high (budget 20000)", out["reasoning_effort"])
	}
	if out["tool_choice"] != "required" {
		t.Errorf("tool_choice = %v want required (any)", out["tool_choice"])
	}
	if ss, ok := out["stop"].([]any); !ok || len(ss) != 1 || ss[0] != "STOP" {
		t.Errorf("stop = %v", out["stop"])
	}
	msgs := out["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages = %d want 4 (system/user/assistant/tool)", len(msgs))
	}
	if m := msgs[0].(map[string]any); m["role"] != "system" || m["content"] != "be nice" {
		t.Errorf("system = %v", m)
	}
	// user：text + image（data URL），thinking 块被剥离。
	u1 := msgs[1].(map[string]any)
	parts := u1["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("user parts = %d want 2 (thinking stripped)", len(parts))
	}
	img := parts[1].(map[string]any)
	if img["type"] != "image_url" || !strings.Contains(mapString(img["image_url"].(map[string]any), "url"), "data:image/png;base64,QUJD") {
		t.Errorf("image part = %v", img)
	}
	// assistant：tool_calls（arguments 序列化）。
	a := msgs[2].(map[string]any)
	tcs := a["tool_calls"].([]any)
	tc := tcs[0].(map[string]any)
	if tc["id"] != "toolu_1" || mapString(tc["function"].(map[string]any), "name") != "f" {
		t.Errorf("assistant tool_call = %v", tc)
	}
	if mapString(tc["function"].(map[string]any), "arguments") != `{"a":1}` {
		t.Errorf("arguments = %v", tc["function"].(map[string]any)["arguments"])
	}
	// tool 消息。
	tm := msgs[3].(map[string]any)
	if tm["role"] != "tool" || tm["tool_call_id"] != "toolu_1" || tm["content"] != "res" {
		t.Errorf("tool message = %v", tm)
	}
	// tools → OpenAI 嵌套 function。
	fns := out["tools"].([]any)
	fn := fns[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "f" || fn["parameters"] == nil {
		t.Errorf("tools = %v", fns)
	}
}

// TestAnthropicConvertDocumentRejected document 块显式 400。
func TestAnthropicConvertDocumentRejected(t *testing.T) {
	raw := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"x"}}]}]}`
	_, _, fail := convertAnthropicRequest([]byte(raw), "m")
	if fail == nil || fail.status != 400 {
		t.Fatalf("fail = %+v want 400", fail)
	}
}

// TestModelAliasApplied alias 表生效：入站 claude 名映射到网关模型（realm 前缀保留）。
func TestModelAliasApplied(t *testing.T) {
	h := NewHandler(Config{ModelAlias: map[string]string{"claude-sonnet-4-5": "cn:glm-5.3"}})
	if got := h.aliasModel("claude-sonnet-4-5"); got != "cn:glm-5.3" {
		t.Errorf("alias = %q", got)
	}
	if got := h.aliasModel("glm-5.2"); got != "glm-5.2" {
		t.Errorf("passthrough = %q", got)
	}
	if got := h.aliasModel(""); got != "" {
		t.Errorf("empty = %q", got)
	}
}

// evNames 提取事件名序列（断言输出用）。
func evNames(evs []sseEv) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.event)
	}
	return out
}
