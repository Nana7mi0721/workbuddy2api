package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/session"
)

// OpenAI Responses 流式翻译器：上游 OpenAI chat SSE → Responses 事件语法
// （服务 Codex CLI）。
//
// 事件序列：response.created → response.in_progress → 每个 output item 一组
// [response.output_item.added →（message: content_part.added → output_text.delta* →
// output_text.done → content_part.done）/（reasoning: reasoning_summary_text.delta* →
// reasoning_summary_text.done）/（function_call: function_call_arguments.delta* →
// function_call_arguments.done）→ response.output_item.done] → response.completed
// （内含聚合完成的完整 response 对象 + usage）；上游 error 帧 → response.failed。
//
// 写出策略与 anthropic_stream.go 一致：首帧懒发 HTTP 头，空流/首帧即错时一个字节
// 未写，sink 可回干净的原生错误响应。
type responsesStreamCtx struct {
	h     *Handler
	w     http.ResponseWriter
	flush func()
	model string
	at    relayAttempt

	wrote    bool
	started  bool // response.created 已发
	sawFrame bool
	respID   string
	seq      int64 // 事件 sequence_number 递增

	outputIndex int    // 下一个 output item 的 output_index
	curKind     string // "" | "message" | "reasoning" | "function_call"
	curIndex    int    // 当前 item 的 output_index
	curItemID   string
	curBuf      strings.Builder // 当前 item 的文本累积（text/summary/arguments 复用）
	partAdded   bool            // message item 的 content_part.added 是否已发

	// function_call item 的标识（tool_call delta 首片给齐后开 item）。
	curToolIdx int
	curCallID  string
	curName    string

	items []map[string]any // 已收口的 output items（response.completed 聚合）

	finish string
	usage  map[string]any
}

// writeResponsesStream 驱动翻译器（约定同 writeAnthropicStream）。
func writeResponsesStream(h *Handler, w http.ResponseWriter, src io.Reader, model string, at relayAttempt) (wrote bool, empty bool, err error) {
	c := &responsesStreamCtx{h: h, w: w, model: model, at: at, curToolIdx: -1}
	if f, ok := w.(http.Flusher); ok {
		c.flush = f.Flush
	} else {
		c.flush = func() {}
	}
	br := bufio.NewReaderSize(src, 64*1024)
	for {
		line, rerr := br.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, "data: ") {
				payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data: "))
				if payload == "[DONE]" {
					break
				}
				stop, ferr := c.frame([]byte(payload))
				if ferr != nil {
					return c.wrote, false, ferr
				}
				if stop {
					return c.wrote, false, nil
				}
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			if !c.started {
				return c.wrote, !c.sawFrame, rerr
			}
			break
		}
	}
	if !c.sawFrame {
		return c.wrote, true, nil
	}
	if err := c.finishStream(); err != nil {
		return c.wrote, false, err
	}
	return c.wrote, false, nil
}

func (c *responsesStreamCtx) writeHeaders() {
	h := c.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	c.w.WriteHeader(http.StatusOK)
}

func (c *responsesStreamCtx) emit(event string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return err
	}
	c.wrote = true
	c.flush()
	return nil
}

func (c *responsesStreamCtx) nextSeq() int64 {
	c.seq++
	return c.seq
}

// responseSkeleton 组装流事件里的 response 对象（created/in_progress 共用）。
func (c *responsesStreamCtx) responseSkeleton(status string) map[string]any {
	return map[string]any{
		"id":                 c.respID,
		"object":             "response",
		"created_at":         time.Now().Unix(),
		"status":             status,
		"model":              c.model,
		"output":             []any{},
		"error":              nil,
		"incomplete_details": nil,
	}
}

// start 懒发 response.created + response.in_progress（首个有效帧到来时）。
func (c *responsesStreamCtx) start() error {
	if c.started {
		return nil
	}
	c.writeHeaders()
	c.started = true
	if c.respID == "" {
		c.respID = "resp_" + session.NewMessageID()
	} else if !strings.HasPrefix(c.respID, "resp_") {
		// 与 buildResponsesResponse 同一 id 约定：上游 chunk id 裸 hex，统一补前缀。
		c.respID = "resp_" + c.respID
	}
	if err := c.emit("response.created", map[string]any{
		"type": "response.created", "sequence_number": c.nextSeq(), "response": c.responseSkeleton("in_progress"),
	}); err != nil {
		return err
	}
	return c.emit("response.in_progress", map[string]any{
		"type": "response.in_progress", "sequence_number": c.nextSeq(), "response": c.responseSkeleton("in_progress"),
	})
}

// openItem 开启一个新的 output item（类型切换先收口旧 item）。
func (c *responsesStreamCtx) openItem(kind string) error {
	if c.curKind == kind {
		return nil
	}
	if err := c.closeItem(); err != nil {
		return err
	}
	c.curKind = kind
	c.curIndex = c.outputIndex
	c.outputIndex++
	c.curBuf.Reset()
	c.partAdded = false
	switch kind {
	case "message":
		c.curItemID = "msg_" + session.NewMessageID()
		return c.emit("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "sequence_number": c.nextSeq(), "output_index": c.curIndex,
			"item": map[string]any{
				"id": c.curItemID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{},
			},
		})
	case "reasoning":
		c.curItemID = "rs_" + session.NewMessageID()
		return c.emit("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "sequence_number": c.nextSeq(), "output_index": c.curIndex,
			"item": map[string]any{"id": c.curItemID, "type": "reasoning", "summary": []any{}},
		})
	case "function_call":
		c.curItemID = "fc_" + session.NewMessageID()
		return c.emit("response.output_item.added", map[string]any{
			"type": "response.output_item.added", "sequence_number": c.nextSeq(), "output_index": c.curIndex,
			"item": map[string]any{
				"id": c.curItemID, "type": "function_call", "call_id": c.curCallID,
				"name": c.curName, "arguments": "", "status": "in_progress",
			},
		})
	}
	return nil
}

// closeItem 收口当前 item：各类型补齐 done 事件与最终 item 对象，并计入 items。
func (c *responsesStreamCtx) closeItem() error {
	if c.curKind == "" {
		return nil
	}
	text := c.curBuf.String()
	switch c.curKind {
	case "message":
		if c.partAdded {
			if err := c.emit("response.output_text.done", map[string]any{
				"type": "response.output_text.done", "sequence_number": c.nextSeq(),
				"item_id": c.curItemID, "output_index": c.curIndex, "content_index": 0, "text": text,
			}); err != nil {
				return err
			}
			if err := c.emit("response.content_part.done", map[string]any{
				"type": "response.content_part.done", "sequence_number": c.nextSeq(),
				"item_id": c.curItemID, "output_index": c.curIndex, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
			}); err != nil {
				return err
			}
		}
		item := map[string]any{
			"id": c.curItemID, "type": "message", "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}},
		}
		if err := c.emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "sequence_number": c.nextSeq(), "output_index": c.curIndex, "item": item,
		}); err != nil {
			return err
		}
		c.items = append(c.items, item)
	case "reasoning":
		if text != "" {
			if err := c.emit("response.reasoning_summary_text.done", map[string]any{
				"type": "response.reasoning_summary_text.done", "sequence_number": c.nextSeq(),
				"item_id": c.curItemID, "output_index": c.curIndex, "summary_index": 0, "text": text,
			}); err != nil {
				return err
			}
		}
		item := map[string]any{
			"id": c.curItemID, "type": "reasoning",
			"summary": []map[string]any{{"type": "summary_text", "text": text}},
		}
		if err := c.emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "sequence_number": c.nextSeq(), "output_index": c.curIndex, "item": item,
		}); err != nil {
			return err
		}
		c.items = append(c.items, item)
	case "function_call":
		if err := c.emit("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "sequence_number": c.nextSeq(),
			"item_id": c.curItemID, "output_index": c.curIndex, "arguments": text,
		}); err != nil {
			return err
		}
		item := map[string]any{
			"id": c.curItemID, "type": "function_call", "call_id": c.curCallID,
			"name": c.curName, "arguments": text, "status": "completed",
		}
		if err := c.emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "sequence_number": c.nextSeq(), "output_index": c.curIndex, "item": item,
		}); err != nil {
			return err
		}
		c.items = append(c.items, item)
	}
	c.curKind = ""
	return nil
}

// finishStream 收口整条响应：关 item → response.completed（聚合 response + usage）。
func (c *responsesStreamCtx) finishStream() error {
	if !c.started {
		return nil
	}
	if err := c.closeItem(); err != nil {
		return err
	}
	status := "completed"
	var incomplete any
	if c.finish == "length" {
		status = "incomplete"
		incomplete = map[string]any{"reason": "max_output_tokens"}
	}
	resp := c.responseSkeleton(status)
	resp["output"] = c.items
	resp["incomplete_details"] = incomplete
	u := c.usage
	pt := mapInt(u, "prompt_tokens")
	ct := mapInt(u, "completion_tokens")
	total := mapInt(u, "total_tokens")
	if total == 0 {
		total = pt + ct
	}
	resp["usage"] = map[string]any{
		"input_tokens":          pt,
		"output_tokens":         ct,
		"total_tokens":          total,
		"input_tokens_details":  map[string]any{"cached_tokens": mapInt(u, "prompt_cache_hit_tokens")},
		"output_tokens_details": map[string]any{"reasoning_tokens": 0},
	}
	return c.emit("response.completed", map[string]any{
		"type": "response.completed", "sequence_number": c.nextSeq(), "response": resp,
	})
}

// frame 处理一帧上游 OpenAI chunk（结构同 anthropic_stream.go，产出 Responses 事件）。
func (c *responsesStreamCtx) frame(payload []byte) (stop bool, err error) {
	var chunk struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Error *struct {
			Code    any    `json:"code"`
			Msg     string `json:"msg"`
			Message string `json:"message"`
		} `json:"error"`
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if json.Unmarshal(payload, &chunk) != nil {
		return false, nil
	}
	if chunk.Error != nil {
		msg := chunk.Error.Message
		if msg == "" {
			msg = chunk.Error.Msg
		}
		if msg == "" {
			msg = "upstream stream error"
		}
		if !c.started {
			return true, fmt.Errorf("upstream stream error: %s", msg)
		}
		resp := c.responseSkeleton("failed")
		resp["output"] = c.items
		resp["error"] = map[string]any{"code": "upstream_error", "message": msg}
		if err := c.emit("response.failed", map[string]any{
			"type": "response.failed", "sequence_number": c.nextSeq(), "response": resp,
		}); err != nil {
			return true, err
		}
		return true, nil
	}
	c.sawFrame = true
	if c.respID == "" {
		c.respID = chunk.ID
	}
	if chunk.Usage != nil {
		c.usage = chunk.Usage
	}
	if len(chunk.Choices) > 0 {
		d := chunk.Choices[0].Delta
		if d.ReasoningContent != "" {
			if err := c.start(); err != nil {
				return true, err
			}
			if err := c.openItem("reasoning"); err != nil {
				return true, err
			}
			c.curBuf.WriteString(d.ReasoningContent)
			if err := c.emit("response.reasoning_summary_text.delta", map[string]any{
				"type": "response.reasoning_summary_text.delta", "sequence_number": c.nextSeq(),
				"item_id": c.curItemID, "output_index": c.curIndex, "summary_index": 0, "delta": d.ReasoningContent,
			}); err != nil {
				return true, err
			}
		}
		if d.Content != "" {
			if err := c.start(); err != nil {
				return true, err
			}
			if err := c.openItem("message"); err != nil {
				return true, err
			}
			if !c.partAdded {
				c.partAdded = true
				if err := c.emit("response.content_part.added", map[string]any{
					"type": "response.content_part.added", "sequence_number": c.nextSeq(),
					"item_id": c.curItemID, "output_index": c.curIndex, "content_index": 0,
					"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
				}); err != nil {
					return true, err
				}
			}
			c.curBuf.WriteString(d.Content)
			if err := c.emit("response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "sequence_number": c.nextSeq(),
				"item_id": c.curItemID, "output_index": c.curIndex, "content_index": 0, "delta": d.Content,
			}); err != nil {
				return true, err
			}
		}
		for _, tc := range d.ToolCalls {
			if err := c.start(); err != nil {
				return true, err
			}
			if c.curKind != "function_call" || c.curToolIdx != tc.Index {
				// 新 tool_call：首片带 id/name，先定标识再开 item。
				c.curToolIdx = tc.Index
				c.curCallID = tc.ID
				if c.curCallID == "" {
					c.curCallID = "call_" + session.NewMessageID()
				}
				c.curName = tc.Function.Name
				if err := c.openItem("function_call"); err != nil {
					return true, err
				}
			}
			if tc.Function.Arguments != "" {
				c.curBuf.WriteString(tc.Function.Arguments)
				if err := c.emit("response.function_call_arguments.delta", map[string]any{
					"type": "response.function_call_arguments.delta", "sequence_number": c.nextSeq(),
					"item_id": c.curItemID, "output_index": c.curIndex, "delta": tc.Function.Arguments,
				}); err != nil {
					return true, err
				}
			}
		}
		if fr := chunk.Choices[0].FinishReason; fr != nil && *fr != "" {
			c.finish = *fr
		}
	}
	return false, nil
}
