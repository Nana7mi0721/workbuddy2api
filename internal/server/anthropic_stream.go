package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// Anthropic Messages 流式翻译器：上游 OpenAI chat SSE → Anthropic 事件语法。
//
// 事件序列（官方规范）：message_start → ping → [content_block_start →
// content_block_delta* → content_block_stop]* → message_delta → message_stop；
// 中途上游 error 帧 → event: error。
//
// 映射规则：
//   - delta.reasoning_content → thinking 块（thinking_delta；收口补 signature_delta
//     空签名，签名语义见 buildAnthropicMessage）；
//   - delta.content → text 块（text_delta）；
//   - delta.tool_calls → tool_use 块（input_json_delta，按上游 tool index 分块）；
//   - finish_reason 帧与 usage 帧缓存，[DONE] 时统一收口 message_delta
//     （stop_reason 映射 + usage 修正——message_start 的 input_tokens 只能先 0，
//     上游 usage 永远在末帧才出现，LiteLLM 同款已验证模式）。
//
// 写出策略：首帧懒发 HTTP 头 + message_start。空流/首帧即错时一个字节都没写，
// sink 可回干净的原生错误响应（比 chat 流式路径的 SSE error 帧更利于客户端排障）。
type anthropicStreamCtx struct {
	h     *Handler
	w     http.ResponseWriter
	flush func()
	model string // 原生请求的 model 名（回显）
	at    relayAttempt

	wrote    bool // 已向客户端写出任何字节（头 + ≥1 事件）
	started  bool // message_start 已发
	sawFrame bool // 见到过有效上游帧（空流判定）
	upID     string

	blockIdx int    // 下一个 content block index
	openKind string // "" | "text" | "thinking" | "tool_use"
	openIdx  int
	openTool int // openKind=="tool_use" 时的上游 tool index

	finish string         // finish_reason（末帧给出）
	usage  map[string]any // 末帧 usage
}

// writeAnthropicStream 驱动翻译器：从 src（上游 SSE）读帧、写出 Anthropic 事件。
// 返回：wrote=已写出字节；empty=未见到任何有效帧（空流）；err=致命错误。
// 约定：err!=nil 且 !wrote → 调用方可回原生错误响应；err!=nil 且 wrote → 客户端
// 已收到 error 事件（或断连），无后续可做。
func writeAnthropicStream(h *Handler, w http.ResponseWriter, src io.Reader, model string, at relayAttempt) (wrote bool, empty bool, err error) {
	c := &anthropicStreamCtx{h: h, w: w, model: model, at: at}
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
			// 上游读错误：未开流可回原生错误；已开流按收口处理（已出的内容保留）。
			if !c.started {
				return c.wrote, !c.sawFrame, rerr
			}
			break
		}
	}
	if !c.sawFrame {
		return c.wrote, true, nil
	}
	if err := c.finalize(); err != nil {
		return c.wrote, false, err
	}
	return c.wrote, false, nil
}

// writeHeaders 懒发 SSE 响应头（仅在确定要产出事件时调用）。
func (c *anthropicStreamCtx) writeHeaders() {
	h := c.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	c.w.WriteHeader(http.StatusOK)
}

// emit 写出一个 SSE 事件（event: + data: 空行），并立即 flush。
func (c *anthropicStreamCtx) emit(event string, payload any) error {
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

// start 懒发 message_start + ping（首个有效帧到来时调用）。
func (c *anthropicStreamCtx) start() error {
	if c.started {
		return nil
	}
	c.writeHeaders()
	c.started = true
	if c.upID == "" {
		c.upID = "msg_" + session.NewMessageID()
	}
	// 与 buildAnthropicMessage 同一 id 约定：剥 chatcmpl- 前缀、补 msg_ 前缀
	// （流式与非流式两条路径产出同形 id）。
	msgID := strings.TrimPrefix(c.upID, "chatcmpl-")
	if !strings.HasPrefix(msgID, "msg_") {
		msgID = "msg_" + msgID
	}
	if err := c.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant", "model": c.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			// input_tokens 上游末帧才出，先 0 占位；message_delta 统一修正。
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}); err != nil {
		return err
	}
	return c.emit("ping", map[string]any{"type": "ping"})
}

// closeBlock 收口当前打开的 content block（无打开块时空操作）。
func (c *anthropicStreamCtx) closeBlock() error {
	if c.openKind == "" {
		return nil
	}
	if c.openKind == "thinking" {
		// 官方流 thinking 块收口时发 signature_delta；本网关不持久化签名，空串占位
		// （入向会剥离客户端回传的 thinking 块，不会被二次消费）。
		if err := c.emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": c.openIdx,
			"delta": map[string]any{"type": "signature_delta", "signature": ""},
		}); err != nil {
			return err
		}
	}
	if err := c.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": c.openIdx}); err != nil {
		return err
	}
	c.openKind = ""
	return nil
}

// ensureBlock 确保指定类型的文本类块处于打开状态（类型切换先收口旧块）。
func (c *anthropicStreamCtx) ensureBlock(kind string) error {
	if c.openKind == kind {
		return nil
	}
	if err := c.closeBlock(); err != nil {
		return err
	}
	c.openIdx = c.blockIdx
	c.blockIdx++
	cb := map[string]any{"type": kind}
	if kind == "text" {
		cb["text"] = ""
	}
	if kind == "thinking" {
		cb["thinking"] = ""
	}
	if err := c.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": c.openIdx, "content_block": cb,
	}); err != nil {
		return err
	}
	c.openKind = kind
	return nil
}

// ensureTool 确保上游第 idx 个 tool_call 对应的 tool_use 块处于打开状态。
func (c *anthropicStreamCtx) ensureTool(idx int, id, name string) error {
	if c.openKind == "tool_use" && c.openTool == idx {
		return nil
	}
	if err := c.closeBlock(); err != nil {
		return err
	}
	c.openIdx = c.blockIdx
	c.blockIdx++
	c.openTool = idx
	if id == "" {
		id = "toolu_" + session.NewMessageID()
	}
	if err := c.emit("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": c.openIdx,
		"content_block": map[string]any{
			"type": "tool_use", "id": id, "name": name, "input": map[string]any{},
		},
	}); err != nil {
		return err
	}
	c.openKind = "tool_use"
	return nil
}

// finalize 收口整条消息：关块 → message_delta（stop_reason + usage 修正）→ message_stop。
func (c *anthropicStreamCtx) finalize() error {
	if !c.started {
		return nil
	}
	if err := c.closeBlock(); err != nil {
		return err
	}
	if err := c.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": mapAnthropicStopReason(c.finish), "stop_sequence": nil},
		// input_tokens 为扩展字段（官方 message_delta.usage 只有 output_tokens），
		// 补上让只读末帧 usage 的客户端拿到真实输入侧用量。
		"usage": map[string]any{
			"output_tokens": mapInt(c.usage, "completion_tokens"),
			"input_tokens":  mapInt(c.usage, "prompt_tokens"),
		},
	}); err != nil {
		return err
	}
	return c.emit("message_stop", map[string]any{"type": "message_stop"})
}

// frame 处理一帧上游 OpenAI chunk。返回 stop=true 表示流终止（错误帧已处理或写出失败）。
func (c *anthropicStreamCtx) frame(payload []byte) (stop bool, err error) {
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
		return false, nil // 非 JSON 帧忽略（注释行/畸形帧，与透传路径的宽容一致）
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
			// 首帧即错：一个字节没写，回给 sink 走原生错误响应（信息量 > SSE error 事件）。
			return true, fmt.Errorf("upstream stream error: %s", msg)
		}
		errObj := map[string]any{"type": "api_error", "message": msg}
		if hint := c.h.hintOf(upstream.ErrServer, msg, c.at.BareModel, c.at.HasImage, nil); hint != "" {
			errObj["gateway_hint"] = hint
		}
		if err := c.emit("error", map[string]any{"type": "error", "error": errObj}); err != nil {
			return true, err
		}
		return true, nil
	}
	c.sawFrame = true
	if c.upID == "" {
		c.upID = chunk.ID
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
			if err := c.ensureBlock("thinking"); err != nil {
				return true, err
			}
			if err := c.emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": c.openIdx,
				"delta": map[string]any{"type": "thinking_delta", "thinking": d.ReasoningContent},
			}); err != nil {
				return true, err
			}
		}
		if d.Content != "" {
			if err := c.start(); err != nil {
				return true, err
			}
			if err := c.ensureBlock("text"); err != nil {
				return true, err
			}
			if err := c.emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": c.openIdx,
				"delta": map[string]any{"type": "text_delta", "text": d.Content},
			}); err != nil {
				return true, err
			}
		}
		for _, tc := range d.ToolCalls {
			if err := c.start(); err != nil {
				return true, err
			}
			if err := c.ensureTool(tc.Index, tc.ID, tc.Function.Name); err != nil {
				return true, err
			}
			if tc.Function.Arguments != "" {
				if err := c.emit("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": c.openIdx,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
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
