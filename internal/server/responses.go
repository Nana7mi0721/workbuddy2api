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

// OpenAI Responses API（POST /v1/responses）入站协议支持（服务 Codex CLI 等）。
//
// 转换拓扑：入向 Responses（instructions + input items）→ OpenAI chat body；
// 出向上游 SSE/聚合结果 → Response 对象 / Responses 事件流（responses_stream.go）。
// 错误信封沿用 OpenAI error 形态（writeOpenAIErrorHint）。

// responsesSink relaySink 的 Responses 实现。
type responsesSink struct {
	model string // 原生请求的 model 名（响应原样回显）
}

func (s responsesSink) writeError(w http.ResponseWriter, status int, code, msg, hint string) {
	writeOpenAIErrorHint(w, status, code, msg, hint)
}

func (s responsesSink) writeUpstream(h *Handler, w http.ResponseWriter, rc io.ReadCloser, st *chatStat, at relayAttempt, stream bool) {
	if !stream {
		resp, err := upstream.Aggregate(rc)
		rc.Close()
		if err != nil {
			writeOpenAIErrorHint(w, http.StatusBadGateway, "upstream_parse", err.Error(), "")
			st.status = http.StatusBadGateway
			return
		}
		writeJSON(w, http.StatusOK, buildResponsesResponse(resp, s.model))
		h.noteSyncObservation(at, resp, st)
		return
	}
	stats := newChatStatsReaderSince(rc, st.start)
	wrote, empty, terr := writeResponsesStream(h, w, stats, s.model, at)
	rc.Close()
	if !wrote {
		// 未向客户端发出任何字节（空流 / 首帧即错）：回干净的原生错误响应。
		msg := "empty upstream stream"
		if terr != nil {
			msg = terr.Error()
		}
		writeOpenAIErrorHint(w, http.StatusBadGateway, "upstream_parse", msg, "")
		st.status = http.StatusBadGateway
		return
	}
	// 已开流（200 + ≥1 事件）：错误帧/断连由翻译器内部处理，这里只做观测收敛。
	// st.status 必须显式落 200（同 anthropicSink，缺省零值会记成 status=0）。
	st.status = http.StatusOK
	h.noteStreamObservation(at, stats, st, empty)
}

// openaiResponses 处理 POST /v1/responses。
func (h *Handler) openaiResponses(w http.ResponseWriter, r *http.Request) {
	// 当日积分预算闸（同 chatCompletions：读 body 之前）。
	if !h.budget.admit() {
		used, limit, _ := h.budget.snapshot()
		writeOpenAIErrorHint(w, http.StatusTooManyRequests, "daily_budget_exceeded",
			fmt.Sprintf("daily credit budget exhausted (used %.2f of %.2f, resets at 00:00 CST)", used, limit), "")
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error(), "")
		return
	}
	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(raw, &peek)
	body, fail := convertResponsesRequest(raw, h.aliasModel(peek.Model))
	if fail != nil {
		writeOpenAIErrorHint(w, fail.status, fail.code, fail.msg, "")
		return
	}
	st := newChatStat(time.Now(), body, peek.Stream, h.budget)
	defer st.done()
	h.relayChat(w, r, body, peek.Stream, "", st, responsesSink{model: peek.Model})
}

// convertResponsesRequest 把 Responses 请求转换为 OpenAI chat body。
// 本网关无响应存储：previous_response_id 显式拒绝（客户端应整体重发上下文，
// Codex CLI 的 store=false 模式正是如此，不受影响）。
func convertResponsesRequest(raw []byte, model string) ([]byte, *httpFail) {
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, &httpFail{400, "invalid_request", "invalid JSON body: " + err.Error()}
	}
	if model == "" {
		return nil, &httpFail{400, "invalid_request", "model: field is required"}
	}
	if p := mapString(req, "previous_response_id"); p != "" {
		return nil, &httpFail{400, "invalid_request",
			"previous_response_id is not supported (this gateway is stateless); resend the full conversation in input (Codex store=false mode is unaffected)"}
	}

	// instructions → 前置 system 消息（Responses 语义：仅作用于本请求的 system）。
	var msgs []map[string]any
	if instr := mapString(req, "instructions"); instr != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": instr})
	}
	switch input := req["input"].(type) {
	case string:
		if input != "" {
			msgs = append(msgs, map[string]any{"role": "user", "content": input})
		}
	case []any:
		for _, ii := range input {
			item, ok := ii.(map[string]any)
			if !ok {
				continue
			}
			converted, fail := convertResponsesInput(item)
			if fail != nil {
				return nil, fail
			}
			msgs = append(msgs, converted...)
		}
	case nil:
		return nil, &httpFail{400, "invalid_request", "input: field is required"}
	default:
		return nil, &httpFail{400, "invalid_request", "input: must be a string or an array of input items"}
	}
	if len(msgs) == 0 {
		return nil, &httpFail{400, "invalid_request", "input: at least one input item is required"}
	}

	out := map[string]any{"model": model, "messages": msgs}
	// max_output_tokens → max_tokens（上游只认后者）。
	if v := mapInt(req, "max_output_tokens"); v > 0 {
		out["max_tokens"] = v
	}
	if v, ok := req["temperature"].(float64); ok {
		out["temperature"] = v
	}
	if v, ok := req["top_p"].(float64); ok {
		out["top_p"] = v
	}
	// tools：Responses 扁平 function 形态 → OpenAI 嵌套形态。其余工具类型
	// （web_search / local_shell / apply_patch 等）上游无对应物，显式拒绝。
	if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
		var fns []map[string]any
		for _, t := range tools {
			tm, ok := t.(map[string]any)
			if !ok {
				continue
			}
			ttype := mapString(tm, "type")
			if ttype == "" {
				ttype = "function"
			}
			if ttype != "function" {
				return nil, &httpFail{400, "invalid_request",
					fmt.Sprintf("tools: unsupported tool type %q (only built-in function tools are supported)", ttype)}
			}
			name := mapString(tm, "name")
			if name == "" {
				continue
			}
			params, ok := tm["parameters"]
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
	// tool_choice：字符串透传；{"type":"function","name"} → OpenAI 对象形态
	// （payload.normalizeToolChoice 随后统一压成上游字符串形态）。
	switch tc := req["tool_choice"].(type) {
	case string:
		if tc != "" {
			out["tool_choice"] = tc
		}
	case map[string]any:
		if mapString(tc, "type") == "function" {
			out["tool_choice"] = map[string]any{
				"type":     "function",
				"function": map[string]any{"name": mapString(tc, "name")},
			}
		}
	}
	// reasoning.effort → reasoning_effort（summary 等其余字段无上游对应物，忽略）。
	if rs, ok := req["reasoning"].(map[string]any); ok {
		if e := mapString(rs, "effort"); e != "" {
			out["reasoning_effort"] = e
		}
	}
	// prompt_cache_key：透传（session.ExtractKey 已识别为粘性键，同时上游侧有
	// 前缀缓存收益）；metadata 透传同理。
	if pk := mapString(req, "prompt_cache_key"); pk != "" {
		out["prompt_cache_key"] = pk
	}
	if md, ok := req["metadata"].(map[string]any); ok && md != nil {
		out["metadata"] = md
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, &httpFail{400, "invalid_request", "convert request: " + err.Error()}
	}
	return body, nil
}

// convertResponsesInput 把一个 Responses input item 转成 0~N 条 OpenAI 消息。
// 非对话型 item（reasoning / item_reference / 工具内部态调用记录）跳过——
// 它们是平台内部态，上游对话历史不消费。
func convertResponsesInput(item map[string]any) ([]map[string]any, *httpFail) {
	switch mapString(item, "type") {
	case "", "message":
		role := mapString(item, "role")
		if role == "" {
			role = "user"
		}
		switch c := item["content"].(type) {
		case string:
			return []map[string]any{{"role": role, "content": c}}, nil
		case []any:
			var parts []map[string]any
			for _, pi := range c {
				pm, ok := pi.(map[string]any)
				if !ok {
					continue
				}
				switch mapString(pm, "type") {
				case "input_text", "output_text", "text":
					if t := mapString(pm, "text"); t != "" {
						parts = append(parts, map[string]any{"type": "text", "text": t})
					}
				case "input_image":
					u := mapString(pm, "image_url")
					if u == "" {
						u = mapString(pm, "url")
					}
					if u == "" {
						return nil, &httpFail{400, "invalid_request", "input_image: only image_url is supported (file_id upload flow is not)"}
					}
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
				default:
					// input_audio 等不支持的 part 忽略。
				}
			}
			if len(parts) == 0 {
				return nil, nil
			}
			return []map[string]any{{"role": role, "content": parts}}, nil
		default:
			return nil, nil
		}
	case "function_call":
		// 历史中的助手工具调用 → assistant 消息的 tool_calls（回放给上游做工具闭环）。
		callID := mapString(item, "call_id")
		if callID == "" {
			callID = mapString(item, "id")
		}
		args := mapString(item, "arguments")
		if args == "" {
			args = "{}"
		}
		return []map[string]any{{
			"role":    "assistant",
			"content": "",
			"tool_calls": []map[string]any{{
				"id":   callID,
				"type": "function",
				"function": map[string]any{
					"name":      mapString(item, "name"),
					"arguments": args,
				},
			}},
		}}, nil
	case "function_call_output":
		// 工具结果 → role:tool 消息（output string 或 parts 数组）。
		var texts []string
		switch v := item["output"].(type) {
		case string:
			texts = append(texts, v)
		case []any:
			for _, pi := range v {
				pm, ok := pi.(map[string]any)
				if !ok {
					continue
				}
				switch mapString(pm, "type") {
				case "output_text", "text":
					texts = append(texts, mapString(pm, "text"))
				}
			}
		default:
			if v != nil {
				b, err := json.Marshal(v)
				if err == nil {
					texts = append(texts, string(b))
				}
			}
		}
		return []map[string]any{{
			"role":         "tool",
			"tool_call_id": mapString(item, "call_id"),
			"content":      strings.Join(texts, "\n"),
		}}, nil
	default:
		// reasoning / item_reference / web_search_call / ... 平台内部态：跳过。
		return nil, nil
	}
}

// buildResponsesResponse 把 Aggregate 的 OpenAI chat.completion 聚合结果转换为
// Responses response 对象（非流式）。
func buildResponsesResponse(agg map[string]any, model string) map[string]any {
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
	output := []map[string]any{}
	if r := mapString(msg, "reasoning_content"); r != "" {
		// 上游原始推理痕迹作为 reasoning summary 透出（Responses 无原文思考字段，
		// summary_text 是官方推荐的承载形态）。
		output = append(output, map[string]any{
			"id":      "rs_" + session.NewMessageID(),
			"type":    "reasoning",
			"summary": []map[string]any{{"type": "summary_text", "text": r}},
		})
	}
	if txt := mapString(msg, "content"); txt != "" {
		output = append(output, map[string]any{
			"id":     "msg_" + session.NewMessageID(),
			"type":   "message",
			"role":   "assistant",
			"status": "completed",
			"content": []map[string]any{{
				"type": "output_text", "text": txt, "annotations": []any{},
			}},
		})
	}
	if tcs := toolCallList(msg["tool_calls"]); len(tcs) > 0 {
		for _, m := range tcs {
			fn, _ := m["function"].(map[string]any)
			output = append(output, map[string]any{
				"id":        "fc_" + session.NewMessageID(),
				"type":      "function_call",
				"call_id":   mapString(m, "id"),
				"name":      mapString(fn, "name"),
				"arguments": mapString(fn, "arguments"),
				"status":    "completed",
			})
		}
	}
	status := "completed"
	var incomplete any
	if finish == "length" {
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	}
	u, _ := agg["usage"].(map[string]any)
	pt := mapInt(u, "prompt_tokens")
	ct := mapInt(u, "completion_tokens")
	total := mapInt(u, "total_tokens")
	if total == 0 {
		total = pt + ct
	}
	return map[string]any{
		"id":                  "resp_" + session.NewMessageID(),
		"object":              "response",
		"created_at":          time.Now().Unix(),
		"status":              status,
		"model":               model,
		"output":              output,
		"error":               nil,
		"incomplete_details":  incomplete,
		"parallel_tool_calls": true,
		"usage": map[string]any{
			"input_tokens":          pt,
			"output_tokens":         ct,
			"total_tokens":          total,
			"input_tokens_details":  map[string]any{"cached_tokens": mapInt(u, "prompt_cache_hit_tokens")},
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		},
	}
}
