package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// Anthropic Messages API（POST /v1/messages）入站协议支持。
//
// 转换拓扑：入向 Anthropic → OpenAI chat body（复用既有上游管线与 payload 规范化）；
// 出向上游 SSE/聚合结果 → Anthropic message / SSE 事件语法（anthropic_stream.go）。
// 错误信封按 Anthropic 规范 {"type":"error","error":{type,message}}。

// httpFail 入向协议校验失败的结构化错误（status + 原生格式 code + message）。
type httpFail struct {
	status int
	code   string
	msg    string
}

// anthropicSink relaySink 的 Anthropic 实现：错误用 Anthropic 信封，
// 响应按 stream 与否走流式翻译器或聚合转换。
type anthropicSink struct {
	model string // 原生请求的 model 名（响应原样回显，Anthropic 语义）
}

func (s anthropicSink) writeError(w http.ResponseWriter, status int, code, msg, hint string) {
	writeAnthropicError(w, status, code, msg, hint)
}

func (s anthropicSink) writeUpstream(h *Handler, w http.ResponseWriter, rc io.ReadCloser, st *chatStat, at relayAttempt, stream bool) {
	if !stream {
		resp, err := upstream.Aggregate(rc)
		rc.Close()
		if err != nil {
			// 上游流解析失败：客户端还没看到任何输出，回 502（Anthropic 信封）。
			writeAnthropicError(w, http.StatusBadGateway, "upstream_parse", err.Error(), "")
			st.status = http.StatusBadGateway
			return
		}
		writeJSON(w, http.StatusOK, buildAnthropicMessage(resp, s.model))
		h.noteSyncObservation(at, resp, st)
		return
	}
	// 流式：chatStatsReader tee 采集 usage/TTFB（字节原样透传），翻译器产出
	// Anthropic 事件语法（message_start/content_block_*/message_delta/...）。
	stats := newChatStatsReaderSince(rc, st.start)
	wrote, empty, terr := writeAnthropicStream(h, w, stats, s.model, at)
	rc.Close()
	if !wrote {
		// 未向客户端发出任何字节（空流 / 首帧即错）：回干净的原生错误响应。
		msg := "empty upstream stream"
		if terr != nil {
			msg = terr.Error()
		}
		writeAnthropicError(w, http.StatusBadGateway, "upstream_parse", msg, "")
		st.status = http.StatusBadGateway
		return
	}
	// 已开流（200 + ≥1 事件）：错误帧/断连由翻译器内部处理，这里只做观测收敛。
	// st.status 必须显式落 200——noteStreamObservation 只在空流时改写为 502，
	// 缺省零值会把成功流记成 status=0（日志/统计口径错误）。
	st.status = http.StatusOK
	h.noteStreamObservation(at, stats, st, empty)
}

// anthropicMessages 处理 POST /v1/messages。
func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	// 当日积分预算闸（同 chatCompletions：读 body 之前，被拒不先吃请求体）。
	if !h.budget.admit() {
		used, limit, _ := h.budget.snapshot()
		writeAnthropicError(w, http.StatusTooManyRequests, "daily_budget_exceeded",
			fmt.Sprintf("daily credit budget exhausted (used %.2f of %.2f, resets at 00:00 CST)", used, limit), "")
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error(), "")
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(raw, &peek)
	body, stickyOverride, fail := convertAnthropicRequest(raw, h.aliasModel(peek.Model))
	if fail != nil {
		writeAnthropicError(w, fail.status, fail.code, fail.msg, "")
		return
	}
	st := newChatStat(time.Now(), body, peek.Stream, h.budget)
	defer st.done()
	h.relayChat(w, r, body, peek.Stream, stickyOverride, st, anthropicSink{model: peek.Model})
}

// convertAnthropicRequest 把 Anthropic Messages 请求转换为 OpenAI chat body。
// model 是调用方已做 alias 解析后的请求模型名（原样写入 body，relayChat 再剥前缀）。
// 返回转换后 body 与粘性会话覆盖键（Anthropic metadata.user_id —— 转换后的
// OpenAI body 提取不出它，StickyFallbackKey 对 metadata.user_id 会抑制 fallback）。
func convertAnthropicRequest(raw []byte, model string) ([]byte, string, *httpFail) {
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, "", &httpFail{400, "invalid_request_error", "invalid JSON body: " + err.Error()}
	}
	if model == "" {
		return nil, "", &httpFail{400, "invalid_request_error", "model: field is required"}
	}
	// max_tokens 必填（Anthropic 规范；OpenAI chat 侧同样只认 max_tokens）。
	maxTokens := mapInt(req, "max_tokens")
	if maxTokens <= 0 {
		return nil, "", &httpFail{400, "invalid_request_error", "max_tokens: field is required and must be a positive integer"}
	}

	// system：string 或 text 块数组 → 单条 system 消息（其余块类型忽略）。
	var msgs []map[string]any
	if sys := anthropicTextOf(req["system"]); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}

	// messages：role 只有 user/assistant；块逐个映射。
	rawMsgs, _ := req["messages"].([]any)
	for _, mi := range rawMsgs {
		m, ok := mi.(map[string]any)
		if !ok {
			continue
		}
		role := mapString(m, "role")
		if role == "assistant" {
			msg := convertAnthropicAssistant(m)
			if msg != nil {
				msgs = append(msgs, msg)
			}
			continue
		}
		if role != "user" {
			continue
		}
		userMsgs, fail := convertAnthropicUser(m)
		if fail != nil {
			return nil, "", fail
		}
		msgs = append(msgs, userMsgs...)
	}
	if len(msgs) == 0 {
		return nil, "", &httpFail{400, "invalid_request_error", "messages: at least one message is required"}
	}

	out := map[string]any{
		"model":      model,
		"messages":   msgs,
		"max_tokens": maxTokens,
	}
	if v, ok := req["temperature"].(float64); ok {
		out["temperature"] = v
	}
	if v, ok := req["top_p"].(float64); ok {
		out["top_p"] = v
	}
	// stop_sequences → stop（OpenAI 命名）。
	if ss, ok := req["stop_sequences"].([]any); ok && len(ss) > 0 {
		out["stop"] = ss
	}
	// tools：input_schema → parameters（OpenAI 嵌套 function 形态）。
	if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
		var fns []map[string]any
		for _, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			name := mapString(tm, "name")
			if name == "" {
				continue
			}
			params, ok := tm["input_schema"]
			if !ok || params == nil {
				params = map[string]any{"type": "object"}
			}
			fns = append(fns, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        name,
					"description": mapString(tm, "description"),
					"parameters":  params,
				},
			})
		}
		if len(fns) > 0 {
			out["tools"] = fns
		}
	}
	// tool_choice：Anthropic 对象形态 → 上游字符串形态（payload.normalizeToolChoice
	// 不会再二次转换字符串，直接落终态）。
	if tc, ok := req["tool_choice"].(map[string]any); ok {
		switch mapString(tc, "type") {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "none":
			out["tool_choice"] = "none"
		case "tool":
			if n := mapString(tc, "name"); n != "" {
				out["tool_choice"] = n
			}
		}
	} else if s, ok := req["tool_choice"].(string); ok && s != "" {
		out["tool_choice"] = s
	}
	// thinking：budget_tokens → reasoning_effort 分档（上游 effort 规范化链路会再按
	// 模型支持降档）。disabled 不发——模型默认行为，客户端显式关思考时上游深度思考
	// 类模型仍按默认档，语义损失可接受（Anthropic thinking 与上游 effort 无精确映射）。
	if th, ok := req["thinking"].(map[string]any); ok && mapString(th, "type") == "enabled" {
		effort := "low"
		if budget := mapInt(th, "budget_tokens"); budget >= 16384 {
			effort = "high"
		} else if budget >= 8192 {
			effort = "medium"
		}
		out["reasoning_effort"] = effort
	}

	body, err := json.Marshal(out)
	if err != nil {
		return nil, "", &httpFail{400, "invalid_request_error", "convert request: " + err.Error()}
	}
	// metadata.user_id：Claude Code 每会话携带 user_<hash>_account_.._session_<uuid>，
	// 天然是会话级粘性键。
	var stickyOverride string
	if md, ok := req["metadata"].(map[string]any); ok {
		stickyOverride = mapString(md, "user_id")
	}
	return body, stickyOverride, nil
}

// anthropicTextOf 把 Anthropic content（string 或 blocks 数组）中的文本拼接出来。
func anthropicTextOf(content any) string {
	s, ok := content.(string)
	if ok {
		return s
	}
	blocks, ok := content.([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, bi := range blocks {
		b, ok := bi.(map[string]any)
		if !ok {
			continue
		}
		if mapString(b, "type") == "text" {
			if t := mapString(b, "text"); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// convertAnthropicAssistant 把 assistant 消息转成 OpenAI assistant 消息：
// text 块 → content；tool_use 块 → tool_calls；thinking/redacted_thinking 丢弃
// （上游不留存推理痕迹，客户端回传的 thinking 块一律剥离）。
func convertAnthropicAssistant(m map[string]any) map[string]any {
	var texts []string
	var toolCalls []map[string]any
	blocks, _ := m["content"].([]any)
	if s, ok := m["content"].(string); ok {
		texts = append(texts, s)
	}
	for _, bi := range blocks {
		b, ok := bi.(map[string]any)
		if !ok {
			continue
		}
		switch mapString(b, "type") {
		case "text":
			if t := mapString(b, "text"); t != "" {
				texts = append(texts, t)
			}
		case "tool_use":
			input := b["input"]
			if input == nil {
				input = map[string]any{}
			}
			args, err := json.Marshal(input)
			if err != nil {
				args = []byte("{}")
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   anthropicToolID(mapString(b, "id")),
				"type": "function",
				"function": map[string]any{
					"name":      mapString(b, "name"),
					"arguments": string(args),
				},
			})
		case "document":
			// document 块不支持：返回 nil 让调用方报错？——assistant 消息里不会出现
			// document（规范上只在 user 消息），此处按未知块忽略。
		}
	}
	msg := map[string]any{"role": "assistant", "content": strings.Join(texts, "\n")}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	return msg
}

// convertAnthropicUser 把 user 消息转成一组 OpenAI 消息：text/image 块累积为一条
// user 消息（content parts 数组），tool_result 块逐个转成独立 role:tool 消息
// （OpenAI 语义），出现在 tool_result 之后的文本另起一条 user 消息（顺序保持）。
func convertAnthropicUser(m map[string]any) ([]map[string]any, *httpFail) {
	var out []map[string]any
	var parts []map[string]any
	flushUser := func() {
		if len(parts) == 0 {
			return
		}
		out = append(out, map[string]any{"role": "user", "content": parts})
		parts = nil
	}
	blocks, _ := m["content"].([]any)
	if s, ok := m["content"].(string); ok {
		parts = append(parts, map[string]any{"type": "text", "text": s})
	}
	for _, bi := range blocks {
		b, ok := bi.(map[string]any)
		if !ok {
			continue
		}
		switch mapString(b, "type") {
		case "text":
			if t := mapString(b, "text"); t != "" {
				parts = append(parts, map[string]any{"type": "text", "text": t})
			}
		case "image":
			part := convertAnthropicImage(b)
			if part == nil {
				return nil, &httpFail{400, "invalid_request_error", "image block: unsupported source (only base64 and url are supported)"}
			}
			parts = append(parts, part)
		case "tool_result":
			flushUser()
			toolMsg, imgs := convertAnthropicToolResult(b)
			out = append(out, toolMsg)
			if len(imgs) > 0 {
				// tool_result 里夹带的图片：OpenAI tool 消息只收文本，另起 user 消息承载。
				out = append(out, map[string]any{"role": "user", "content": imgs})
			}
		case "thinking", "redacted_thinking":
			// 丢弃（同 assistant）。
		case "document":
			return nil, &httpFail{400, "invalid_request_error", "document blocks are not supported by this gateway"}
		default:
			// 未知块忽略（前向兼容）。
		}
	}
	flushUser()
	return out, nil
}

// convertAnthropicToolResult 把 tool_result 块转成 OpenAI tool 消息。
// content 的文本拼为字符串；夹带的图片以 parts 返回（调用方另起 user 消息）。
func convertAnthropicToolResult(b map[string]any) (map[string]any, []map[string]any) {
	var texts []string
	var imgs []map[string]any
	switch c := b["content"].(type) {
	case string:
		texts = append(texts, c)
	case []any:
		for _, pi := range c {
			pm, ok := pi.(map[string]any)
			if !ok {
				continue
			}
			switch mapString(pm, "type") {
			case "text":
				texts = append(texts, mapString(pm, "text"))
			case "image":
				if part := convertAnthropicImage(pm); part != nil {
					imgs = append(imgs, part)
				}
			}
		}
	}
	return map[string]any{
		"role":         "tool",
		"tool_call_id": mapString(b, "tool_use_id"),
		"content":      strings.Join(texts, "\n"),
	}, imgs
}

// convertAnthropicImage 把 Anthropic image 块转为 OpenAI image_url part。
// 支持 base64（data URL）与 url 两种 source；其余返回 nil。
func convertAnthropicImage(b map[string]any) map[string]any {
	src, _ := b["source"].(map[string]any)
	if src == nil {
		return nil
	}
	switch mapString(src, "type") {
	case "base64":
		media := mapString(src, "media_type")
		if media == "" {
			media = "image/png"
		}
		data := mapString(src, "data")
		if data == "" {
			return nil
		}
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + media + ";base64," + data}}
	case "url":
		u := mapString(src, "url")
		if u == "" {
			return nil
		}
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}}
	}
	return nil
}

// buildAnthropicMessage 把 Aggregate 的 OpenAI chat.completion 聚合结果转换为
// Anthropic message 对象（非流式）。
func buildAnthropicMessage(agg map[string]any, model string) map[string]any {
	var msg map[string]any
	finish := "stop"
	if choices, ok := agg["choices"].([]any); ok && len(choices) > 0 {
		if c, ok := choices[0].(map[string]any); ok {
			msg, _ = c["message"].(map[string]any)
			if f, ok := c["finish_reason"].(string); ok && f != "" {
				finish = f
			}
		}
	}
	var content []map[string]any
	if r := mapString(msg, "reasoning_content"); r != "" {
		// signature 置空串：本网关不持久化上游推理签名，且入向会剥离客户端回传的
		// thinking 块，空签名不会被二次消费。
		content = append(content, map[string]any{"type": "thinking", "thinking": r, "signature": ""})
	}
	if txt := mapString(msg, "content"); txt != "" {
		content = append(content, map[string]any{"type": "text", "text": txt})
	}
	if tcs := toolCallList(msg["tool_calls"]); len(tcs) > 0 {
		for _, m := range tcs {
			fn, _ := m["function"].(map[string]any)
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    anthropicToolID(mapString(m, "id")),
				"name":  mapString(fn, "name"),
				"input": parseToolArguments(mapString(fn, "arguments")),
			})
		}
	}
	if content == nil {
		content = []map[string]any{}
	}
	u, _ := agg["usage"].(map[string]any)
	usage := map[string]any{
		"input_tokens":  mapInt(u, "prompt_tokens"),
		"output_tokens": mapInt(u, "completion_tokens"),
	}
	if v, ok := u["prompt_cache_hit_tokens"]; ok {
		usage["cache_read_input_tokens"] = v
	}
	if v, ok := u["prompt_cache_write_tokens"]; ok {
		usage["cache_creation_input_tokens"] = v
	}
	id := "msg_" + strings.TrimPrefix(mapString(agg, "id"), "chatcmpl-")
	if id == "msg_" {
		id = "msg_" + session.NewMessageID()
	}
	return map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   mapAnthropicStopReason(finish),
		"stop_sequence": nil,
		"usage":         usage,
	}
}

// mapAnthropicStopReason OpenAI finish_reason → Anthropic stop_reason。
func mapAnthropicStopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		// stop / content_filter / 未知 → end_turn（Anthropic 无 content_filter 语义）。
		return "end_turn"
	}
}

// anthropicErrorType 按 HTTP 状态映射 Anthropic error.type。
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// writeAnthropicError 以 Anthropic 错误信封输出（{"type":"error","error":{...}}）。
// OpenAI 侧的 code 语义并入 type 映射与 message；hint 非空时附加 gateway_hint
// （扩展字段，官方 SDK 忽略未知字段）。
func writeAnthropicError(w http.ResponseWriter, status int, code, msg, hint string) {
	_ = code
	errObj := map[string]any{"type": anthropicErrorType(status), "message": msg}
	if hint != "" {
		errObj["gateway_hint"] = hint
	}
	writeJSON(w, status, map[string]any{"type": "error", "error": errObj})
}

// anthropicCountTokens 处理 POST /v1/messages/count_tokens：启发式估算 input_tokens
// （约 3.5 字符/token + 每消息 4 开销 + 图片按 1600 token 定额），不发起上游调用。
// Anthropic 官方值来自真实 tokenizer；本估算供客户端预检上下文预算，量级即可。
func (h *Handler) anthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error(), "")
		return
	}
	n, fail := estimateAnthropicInputTokens(raw)
	if fail != nil {
		writeAnthropicError(w, fail.status, fail.code, fail.msg, "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": n})
}

// estimateAnthropicInputTokens 启发式估算（见 anthropicCountTokens）。
func estimateAnthropicInputTokens(raw []byte) (int, *httpFail) {
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		return 0, &httpFail{400, "invalid_request_error", "invalid JSON body: " + err.Error()}
	}
	var chars, images int
	var addContent func(v any, depth int)
	addContent = func(v any, depth int) {
		if depth > 20 {
			return
		}
		switch c := v.(type) {
		case string:
			chars += len(c)
		case []any:
			for _, b := range c {
				addContent(b, depth+1)
			}
		case map[string]any:
			switch mapString(c, "type") {
			case "text":
				chars += len(mapString(c, "text"))
			case "image":
				images++
			case "tool_result":
				addContent(c["content"], depth+1)
			case "tool_use":
				b, _ := json.Marshal(c["input"])
				chars += len(b)
			default:
				// 未知块：保守计一次字段文本。
				chars += len(mapString(c, "text"))
			}
		}
	}
	addContent(req["system"], 0)
	if msgs, ok := req["messages"].([]any); ok {
		for _, mi := range msgs {
			m, ok := mi.(map[string]any)
			if !ok {
				continue
			}
			chars += 16 // 每消息角色/结构开销（近似）
			addContent(m["content"], 1)
		}
	}
	if tools, ok := req["tools"].([]any); ok {
		for _, t := range tools {
			b, err := json.Marshal(t)
			if err == nil {
				chars += len(b)
			}
		}
	}
	tokens := (chars*10 + 34) / 35 // chars / 3.5 向上取整
	tokens += images * 1600
	if tokens <= 0 {
		tokens = 1
	}
	return tokens, nil
}

// anthropicToolID 复用客户端给的 id；缺失时生成 toolu_ 前缀的唯一 id。
func anthropicToolID(id string) string {
	if id != "" {
		return id
	}
	return "toolu_" + session.NewMessageID()
}

// toolCallList 取出 message.tool_calls 的统一形态：Aggregate 内存路径产
// []map[string]any，JSON 反序列化路径产 []any，两者都归一成前者。
func toolCallList(v any) []map[string]any {
	switch tcs := v.(type) {
	case []map[string]any:
		return tcs
	case []any:
		out := make([]map[string]any, 0, len(tcs))
		for _, tc := range tcs {
			if m, ok := tc.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// parseToolArguments 把 tool_call 的 arguments JSON 字符串解析为对象；失败回 {}。
func parseToolArguments(args string) any {
	var v any
	if args == "" || json.Unmarshal([]byte(args), &v) != nil {
		return map[string]any{}
	}
	return v
}
