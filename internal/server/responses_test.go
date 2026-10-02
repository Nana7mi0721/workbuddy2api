package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// ---------------------------------------------------------------------------
// /v1/responses handler 级
// ---------------------------------------------------------------------------

// TestResponsesSync 非流式：input string + instructions → 上游 chat body 正确、
// 响应为 Response 对象（output message / status / usage）。
func TestResponsesSync(t *testing.T) {
	var got string
	up := captureUpstream(t, &got, 200, sseOK, true)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"hello world","instructions":"be brief","max_output_tokens":100}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 上游 body：instructions → system 前置、input → user、max_output_tokens → max_tokens。
	var sent map[string]any
	if err := json.Unmarshal([]byte(got), &sent); err != nil {
		t.Fatalf("upstream body not json: %v (%s)", err, got)
	}
	msgs := sent["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("upstream messages = %d want 2 (%s)", len(msgs), got)
	}
	if m := msgs[0].(map[string]any); m["role"] != "system" || m["content"] != "be brief" {
		t.Errorf("system = %v", m)
	}
	if m := msgs[1].(map[string]any); m["role"] != "user" || m["content"] != "hello world" {
		t.Errorf("user = %v", m)
	}
	if sent["max_tokens"].(float64) != 100 {
		t.Errorf("max_tokens = %v", sent["max_tokens"])
	}
	// 响应：Response 对象。
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if out["object"] != "response" || out["status"] != "completed" || out["model"] != "glm-5.2" {
		t.Errorf("response = %v", out)
	}
	output := out["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %v", output)
	}
	item := output[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "assistant" {
		t.Errorf("item = %v", item)
	}
	content := item["content"].([]any)[0].(map[string]any)
	if content["type"] != "output_text" || content["text"] != "你好" {
		t.Errorf("content = %v", content)
	}
	usage := out["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 1 || usage["output_tokens"].(float64) != 1 || usage["total_tokens"].(float64) != 2 {
		t.Errorf("usage = %v", usage)
	}
	if rec.Header().Get("X-Wb-Account") != "u1" {
		t.Errorf("X-Wb-Account = %q", rec.Header().Get("X-Wb-Account"))
	}
}

// TestResponsesInputItems 入向 input items 映射：message parts / function_call /
// function_call_output / reasoning 跳过。
func TestResponsesInputItems(t *testing.T) {
	var got string
	up := captureUpstream(t, &got, 200, sseOK, true)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	raw := `{"model":"glm-5.2","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"weather?"}]},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"old thinking"}]},
		{"type":"function_call","call_id":"call_9","name":"get_weather","arguments":"{\"city\":\"杭州\"}"},
		{"type":"function_call_output","call_id":"call_9","output":"sunny"}
	]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(raw)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(got), &sent)
	msgs := sent["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %d want 3 (reasoning skipped): %s", len(msgs), got)
	}
	u := msgs[0].(map[string]any)
	if u["role"] != "user" {
		t.Errorf("m0 = %v", u)
	}
	part := u["content"].([]any)[0].(map[string]any)
	if part["type"] != "text" || part["text"] != "weather?" {
		t.Errorf("part = %v", part)
	}
	a := msgs[1].(map[string]any)
	tc := a["tool_calls"].([]any)[0].(map[string]any)
	if tc["id"] != "call_9" || mapString(tc["function"].(map[string]any), "name") != "get_weather" {
		t.Errorf("tool_call = %v", tc)
	}
	tm := msgs[2].(map[string]any)
	if tm["role"] != "tool" || tm["tool_call_id"] != "call_9" || tm["content"] != "sunny" {
		t.Errorf("tool msg = %v", tm)
	}
}

// TestResponsesStreamEvents 流式：完整 Responses 事件序列 + completed 聚合对象。
func TestResponsesStreamEvents(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"input":"hi"}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	want := []string{
		"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added",
		"response.output_text.delta", "response.output_text.done",
		"response.content_part.done", "response.output_item.done",
		"response.completed",
	}
	evs := parseSSE(t, rec.Body.String())
	if len(evs) != len(want) {
		t.Fatalf("events = %v want %d events", evNames(evs), len(want))
	}
	for i, w := range want {
		if evs[i].event != w {
			t.Fatalf("event[%d] = %q want %q", i, evs[i].event, w)
		}
	}
	var delta struct {
		Delta string `json:"delta"`
		ItemID string `json:"item_id"`
	}
	if err := json.Unmarshal([]byte(evs[4].data), &delta); err != nil || delta.Delta != "你好" {
		t.Errorf("text delta = %s err=%v", evs[4].data, err)
	}
	var completed struct {
		Response struct {
			Status string           `json:"status"`
			Output []map[string]any `json:"output"`
			Usage  struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
				TotalTokens  int `json:"total_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(evs[8].data), &completed); err != nil {
		t.Fatalf("completed: %v", err)
	}
	if completed.Response.Status != "completed" || len(completed.Response.Output) != 1 {
		t.Errorf("completed response = %+v", completed.Response)
	}
	if completed.Response.Usage.InputTokens != 1 || completed.Response.Usage.OutputTokens != 1 {
		t.Errorf("completed usage = %+v", completed.Response.Usage)
	}
}

// TestResponsesToolCallStream 工具调用流式：function_call item + arguments 分片。
func TestResponsesToolCallStream(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseToolOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"input":"weather?"}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	evs := parseSSE(t, rec.Body.String())
	names := evNames(evs)
	// 首帧 content="" 不开消息块：直接进 function_call item。
	if len(names) < 8 || names[2] != "response.output_item.added" {
		t.Fatalf("events = %v", names)
	}
	var added struct {
		Item struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
			Name   string `json:"name"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(evs[2].data), &added); err != nil ||
		added.Item.Type != "function_call" || added.Item.CallID != "call_1" || added.Item.Name != "get_weather" {
		t.Errorf("item added = %s err=%v", evs[2].data, err)
	}
	if !strings.Contains(names[3], "function_call_arguments.delta") {
		t.Errorf("event[3] = %q", names[3])
	}
	// 末端：arguments.done / output_item.done / response.completed。
	tail := names[len(names)-3:]
	if tail[0] != "response.function_call_arguments.done" || tail[1] != "response.output_item.done" || tail[2] != "response.completed" {
		t.Errorf("tail = %v", tail)
	}
	var completed struct {
		Response struct {
			Output []map[string]any `json:"output"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(evs[len(evs)-1].data), &completed); err != nil {
		t.Fatalf("completed: %v", err)
	}
	if len(completed.Response.Output) != 1 {
		t.Fatalf("output = %v", completed.Response.Output)
	}
	fc := completed.Response.Output[0]
	if fc["type"] != "function_call" || fc["arguments"] != `{"city":"杭州"}` {
		t.Errorf("function_call item = %v", fc)
	}
}

// TestResponsesRejectsPreviousResponseID 无响应存储：previous_response_id 显式 400。
func TestResponsesRejectsPreviousResponseID(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"hi","previous_response_id":"resp_x"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "previous_response_id") {
		t.Errorf("error should explain statelessness: %s", rec.Body)
	}
}

// TestResponsesUnsupportedToolType 非内置工具类型显式 400（上游无对应物）。
func TestResponsesUnsupportedToolType(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"glm-5.2","input":"hi","tools":[{"type":"web_search"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400", rec.Code, rec.Body)
	}
}

// TestResponsesReasoningEffortAndTools reasoning.effort / 扁平 tools / tool_choice
// 的入向映射。
func TestResponsesReasoningEffortAndTools(t *testing.T) {
	var got string
	up := captureUpstream(t, &got, 200, sseOK, true)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	raw := `{"model":"glm-5.2","input":"hi",
		"reasoning":{"effort":"high","summary":"auto"},
		"tools":[{"type":"function","name":"f","description":"d","parameters":{"type":"object"}}],
		"tool_choice":{"type":"function","name":"f"},
		"prompt_cache_key":"cache-1"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(raw)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(got), &sent)
	if sent["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v", sent["reasoning_effort"])
	}
	if sent["prompt_cache_key"] != "cache-1" {
		t.Errorf("prompt_cache_key = %v", sent["prompt_cache_key"])
	}
	fn := sent["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "f" {
		t.Errorf("tools = %v", sent["tools"])
	}
	// tool_choice 对象形态会被 payload.normalizeToolChoice 压成上游字符串形态 "f"。
	if sent["tool_choice"] != "f" {
		t.Errorf("tool_choice = %v want f (normalized string)", sent["tool_choice"])
	}
}

// TestResponsesModelAlias alias 表对 Responses 同样生效。
func TestResponsesModelAlias(t *testing.T) {
	var got string
	up := captureUpstream(t, &got, 200, sseOK, true)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, ModelAlias: map[string]string{"gpt-5.6-codex": "glm-5.3"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"gpt-5.6-codex","input":"hi"}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["model"] != "gpt-5.6-codex" {
		t.Errorf("response model echo = %v want original gpt-5.6-codex", out["model"])
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(got), &sent)
	if sent["model"] != "glm-5.3" {
		t.Errorf("upstream model = %v want glm-5.3", sent["model"])
	}
}

// TestModelsNegotiatedAnthropicShape /v1/models 内容协商：anthropic-version 头 →
// Anthropic 形状；无头 → OpenAI 形状（回归不变）。
func TestModelsNegotiatedAnthropicShape(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, "not-json", false })
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var openai map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &openai)
	if openai["object"] != "list" {
		t.Errorf("openai shape = %v", openai)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("anthropic-version", "2023-06-01")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var anth struct {
		Data    []map[string]any `json:"data"`
		HasMore bool             `json:"has_more"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &anth); err != nil {
		t.Fatalf("anthropic shape not json: %v (%s)", err, rec.Body)
	}
	if anth.Data == nil {
		t.Errorf("anthropic shape missing data array: %s", rec.Body)
	}
}
