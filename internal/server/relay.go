package server

import (
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/logfmt"
	"workbuddy2api/internal/prompt"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

// relaySink 抽象入站协议（OpenAI chat / Anthropic messages / OpenAI responses）在
// **响应侧**的差异：错误信封 + 上游流的消费与原生格式写出。中继核心 relayChat 负责
// 选号/轮转/冷却/降级/头族等与协议无关的一切；协议差异全部收敛在 sink 实现。
//
// 与 chatCompletions 的分工：chat 路径保持独立不动（2500 行回归测试 + 上游合并
// 冲突面最小化）；新协议经由本接口复用同一套账号调度语义。
type relaySink interface {
	// writeError 输出**完整**的原生格式错误响应（writeJSON 语义：内部 WriteHeader）。
	// code 是网关侧错误码（daily_budget_exceeded / content_blocked /
	// no_healthy_account / ...）；格式实现自行决定呈现方式（Anthropic 映射 error.type，
	// OpenAI 家族进 error.code）。
	writeError(w http.ResponseWriter, status int, code, msg, hint string)
	// writeUpstream 消费上游 SSE 流（rc 所有权归实现，必须 Close）并写出原生响应。
	// stream=false 时先聚合再一次性写出。实现负责：
	//   - st 的观测字段填充与成本账本（用 relay.go 的 noteStreamObservation /
	//     noteSyncObservation，与 chatCompletions 同口径）；
	//   - 空流/半截流的兜底语义（未发出字节→原生错误 502；已发出→200 观测收敛 502）。
	writeUpstream(h *Handler, w http.ResponseWriter, rc io.ReadCloser, st *chatStat, at relayAttempt, stream bool)
}

// relayAttempt 一次成功上游调用（已 NoteSuccess、响应头未写）的上下文，
// 供 sink 写响应时记账（成本账本按账号归属）与组装错误 hint。
type relayAttempt struct {
	Acct      *auth.Auth
	BareModel string
	HasImage  bool
}

// relayChat 新入站协议（/v1/messages、/v1/responses）共享的中继核心。
// 从 chatCompletions 的轮转循环逐段适配（选号/粘性/冷却/降级/头族语义零漂移，
// 沿途注释指向 handler.go 原始出处），差异仅两处：
//   - 响应写出经 sink（原生协议格式）；
//   - 粘性键覆盖 stickyOverride：Anthropic metadata.user_id 之类的原生会话标识，
//     转换后的 OpenAI body 提取不出（StickyFallbackKey 对 metadata.user_id 会抑制
//     fallback），由调用方显式传入。
//
// 调用契约：调用方已完成 budget.admit()、原生 body 读取、入向协议转换、chatStat
// 构造（含 defer st.done()）；body 是**已转换的 OpenAI chat 形态**，model 字段为
// 请求模型名（可带 realm 前缀）。st 由调用方传入（统计对象生命周期覆盖整个请求，
// relayChat 内的各失败/成功分支只写字段）。
func (h *Handler) relayChat(w http.ResponseWriter, r *http.Request, body []byte, stream bool, stickyOverride string, st *chatStat, sink relaySink) {
	// realm 前缀解析（D6）：同 chatCompletions —— bareModel 用于选号/粘性/账本/
	// 出站 body 重写（前缀是网关侧路由协议，上游只认裸名）。
	realm, bareModel := resolveModel(parseModelFromBody(body))

	tried := map[string]bool{}
	var lastErr error

	// 会话粘性（handler.go chatCompletions 同构）：sessKey 供会话头族聚合、
	// stickyKey 供粘性选号，两者分开维护；stickyOverride 只影响 stickyKey。
	sessKey := session.ExtractKey(body)
	stickyKey := sessKey
	if stickyKey == "" {
		if stickyOverride != "" {
			stickyKey = stickyOverride
		} else {
			stickyKey = session.StickyFallbackKey(body)
		}
	}
	stickyUID := ""
	if h.cfg.Session != nil && stickyKey != "" {
		// 传完整模型名（含 realm 前缀）：realmAwareAvailableForModel 闭包需要完整
		// 名才能正确过滤 global 集合（见 chatCompletions 同位置注释）。
		if uid, ok := h.cfg.Session.ResolveForModel(stickyKey, parseModelFromBody(body)); ok {
			stickyUID = uid
		}
	}

	// 轮级聚合键 + 请求形态（改写前取，理由同 chatCompletions）。
	turnKey := session.TurnKey(body)
	reqHasImage := hasImagePart(body)

	// 在途租约（同 chatCompletions：Pick 成功占名额，出口统一释放）。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	unbindSticky := func() {
		if stickyUID != "" {
			h.cfg.Session.Unbind(stickyKey)
			stickyUID = ""
		}
	}
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}

	// 系统提示词改写（handler.go 801-817 同构：custom/append/passthrough + 降级期）。
	degradedApplied := false
	if h.cfg.PromptMode == "custom" && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	} else if h.cfg.PromptMode == "append" && h.cfg.PromptText != "" && !h.degrade.Active() {
		body = prompt.Append(body, h.cfg.PromptText)
	} else if (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && h.degrade.Active() {
		body = prompt.Rewrite(body, prompt.Degraded)
		degradedApplied = true
	}

	// outbound model 名重写为 bareModel（D6）。
	if bareModel != parseModelFromBody(body) {
		body = rewriteModel(body, bareModel)
	}

	// 会话头族（handler.go 825-861 同构）：轮级 ConversationRequestID 复合键。
	chatMeta := upstream.ChatMeta{ConversationID: session.ResolveConversationID(body)}
	if v := r.Header.Get("X-Conversation-Request-ID"); v != "" {
		chatMeta.ConversationRequestID = v
	} else if turnKey != "" && sessKey != "" {
		chatMeta.ConversationRequestID = session.TurnRequestID(sessKey + ":" + turnKey)
	} else if turnKey != "" {
		chatMeta.ConversationRequestID = session.TurnRequestID(turnKey)
	} else if sessKey != "" {
		chatMeta.ConversationRequestID = session.RequestIDForKey(sessKey)
	} else {
		chatMeta.ConversationRequestID = session.TurnRequestID("")
	}
	chatMeta.TraceID = r.Header.Get("X-Trace-ID")

	// 本次请求密钥的可见业务分组（nil = 不限分组）。
	var keyGroups []string
	if ki := authKeyFrom(r.Context()); ki != nil {
		keyGroups = ki.Groups
	}

	for i := 0; i < h.cfg.MaxRotate; i++ {
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUIDForModelGroups(stickyUID, bareModel, keyGroups)
			if acct == nil || (realm != "" && acct.Realm() != realm) {
				unbindSticky()
			}
		}
		if acct == nil {
			acct = h.cfg.Pool.PickExcludingForRealmGroups(tried, bareModel, realm, keyGroups)
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			break
		}
		st.uid = acct.UID
		st.nick = acct.Nickname
		tried[acct.UID] = true

		if !h.cfg.Pool.Acquire(acct.UID) {
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			if !rotateBackoff(i, r.Context()) {
				break
			}
			continue
		}
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）。
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				h.cfg.Pool.NoteRefreshFail(acct.UID, err)
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					h.cfg.Pool.NoteSessionDead(acct.UID)
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				if !rotateBackoff(i, r.Context()) {
					break
				}
				continue
			}
			h.cfg.Pool.NoteRefreshOK(acct.UID)
			acct.BackfillRealm()
			if err := acct.SaveAtomic(); err != nil {
				log.Printf("ERR: [server] relay refresh acct=%s: save auth failed: %v", logfmt.Label(acct.UID, acct.Nickname), err)
			}
		}

		var clientIP string
		if h.cfg.Upstream.PassthroughIP {
			clientIP = upstream.ExtractClientIP(r)
		}
		rc, status, respBody, terr := h.cfg.Upstream.ChatStreamContext(r.Context(), acct, body, clientIP, chatMeta)
		var uerr *upstream.Error
		if errors.As(terr, &uerr) {
			status = uerr.Status
		}
		if uerr == nil && terr != nil {
			// 传输层抖动：换号不喂熔断，喂连败计数（issue #114，同 chatCompletions）。
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			h.cfg.Pool.NoteFailures(acct.UID)
			fail(acct.UID)
			if !rotateBackoff(i, r.Context()) {
				break
			}
			continue
		}
		if status >= 400 {
			st.status = status
			var kind upstream.ErrKind
			if uerr != nil {
				kind = uerr.Kind
			} else {
				kind = upstream.Classify(status, string(respBody))
				uerr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			}
			// 内容拦截误报 → 降级重试（handler.go 985-993 同构）。
			if kind == upstream.ErrContentBlocked && (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && !degradedApplied {
				h.degrade.Trigger()
				body = prompt.Rewrite(body, prompt.Degraded)
				degradedApplied = true
				delete(tried, acct.UID)
				releaseHeld()
				log.Printf("WARN: [server] content-blocked (likely fingerprint false positive) -> degraded prompt retry")
				continue
			}
			// 内容拦截（二次）：立即回，不轮转不罚号。
			if kind == upstream.ErrContentBlocked {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "content blocked by upstream content firewall"
				}
				sink.writeError(w, http.StatusBadRequest, "content_blocked", msg,
					h.hintOf(upstream.ErrContentBlocked, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				return
			}
			// 11115 prompt too long：立即透传原文，不罚号不轮转。
			if kind == upstream.ErrPromptTooLong {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				sink.writeError(w, http.StatusBadRequest, "prompt_too_long", promptTooLongMessage(string(respBody)),
					h.hintOf(upstream.ErrPromptTooLong, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				return
			}
			// 图片无效：立即透传，不罚号不轮转。
			if kind == upstream.ErrImageInvalid {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "image request was rejected by upstream"
				}
				sink.writeError(w, http.StatusBadRequest, "image_invalid", msg,
					h.hintOf(upstream.ErrImageInvalid, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				return
			}
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody), RetryAfter: uerr.RetryAfter}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
			fail(acct.UID)
			if kind == upstream.ErrWafBlock && h.wafIP.noteWaf(acct.UID) {
				break
			}
			if !rotateBackoff(i, r.Context()) {
				break
			}
			continue
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		// 响应头透出实际服务的账号（放在 NoteSuccess 之后，语义同 chatCompletions）。
		w.Header().Set("X-Wb-Account", acct.UID)
		// 11102 负缓存清命 + 粘性跟随最终成功号。
		h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
		if stickyKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(stickyKey, acct.UID)
		}
		sink.writeUpstream(h, w, rc, st, relayAttempt{Acct: acct, BareModel: bareModel, HasImage: reqHasImage}, stream)
		return
	}

	// 末端错误透传（handler.go 1155-1186 同构）：上游错误原文透传，格式由 sink 定。
	status := http.StatusServiceUnavailable
	code := "no_healthy_account"
	msg := "all accounts are temporarily unavailable, please retry later"
	hint := upstream.NoHealthyAccountHint()
	var ue *upstream.Error
	if errors.As(lastErr, &ue) {
		hint = h.hintOf(ue.Kind, ue.Msg, bareModel, reqHasImage, ue)
		switch ue.Kind {
		case upstream.ErrSoftRate:
			status = http.StatusTooManyRequests
			code = "rate_limit_exceeded"
			msg = "rate limited: all accounts are cooling down, please wait a moment and try again"
		case upstream.ErrWafBlock:
			if h.wafIP.active() {
				code = "waf_ip_blocked"
				msg = "waf ip-level block: upstream firewall is blocking the gateway IP, rotation stopped; retry after the block window expires"
			}
		}
		if s := strings.TrimSpace(ue.Msg); s != "" {
			msg = s
		}
	}
	sink.writeError(w, status, code, msg, hint)
	st.status = status
}

// noteStreamObservation 流式响应完成后的观测收敛（与 chatCompletions 1094-1119
// 同口径）：st 字段填充 + metrics + 成本账本。emptyStream=true 时收敛为 502 观测。
func (h *Handler) noteStreamObservation(at relayAttempt, stats *chatStatsReader, st *chatStat, emptyStream bool) {
	if emptyStream {
		st.status = http.StatusBadGateway
		log.Printf("WARN: [server] stream acct=%s model=%s: empty upstream stream (200+0 frames)",
			logfmt.Label(at.Acct.UID, at.Acct.Nickname), at.BareModel)
	}
	st.ttfb = stats.TTFB()
	// usage 缺失时保留 chatStat.toks 的 -1 哨兵（观测缺失 → 显示 "-"）。
	toks, hasUsage := stats.Tokens()
	if hasUsage {
		st.toks = toks
	}
	st.hasUsage = hasUsage
	st.prompt = stats.PromptTokens()
	st.cacheHit, st.cacheMiss, st.cacheWr = stats.CacheTokens()
	if credit, ok := stats.Credit(); ok {
		st.credit = credit
		st.hasCredit = true
	}
	if credit, ok := stats.Credit(); ok {
		h.cfg.Pool.NoteModelCost(at.Acct.UID, at.BareModel, credit, stats.TotalTokens())
	} else if hasUsage {
		log.Printf("WARN: [server] stream usage without credit acct=%s model=%s (no cost observation)",
			logfmt.Label(at.Acct.UID, at.Acct.Nickname), at.BareModel)
	}
	// 按日聚合（daily_stats.go，面板统计页长期趋势）：与成本账本同观测点，
	// usage 缺失不记（缺失≠0）。
	if hasUsage {
		h.daily.Record(at.BareModel, st.prompt, st.toks, st.cacheHit, st.credit)
	}
}

// noteSyncObservation 非流式（Aggregate）响应的观测收敛（与 chatCompletions
// 1131-1139 同口径）。
func (h *Handler) noteSyncObservation(at relayAttempt, resp map[string]any, st *chatStat) {
	st.status = http.StatusOK
	st.toks = completionTokens(resp)
	if credit, total, ok := usageCreditTotal(resp); ok {
		h.cfg.Pool.NoteModelCost(at.Acct.UID, at.BareModel, credit, total)
	}
	fillStatFromUsage(st, resp)
	// 按日聚合（daily_stats.go）：与流式观测同口径，usage 缺失不记。
	if st.hasUsage {
		h.daily.Record(at.BareModel, st.prompt, st.toks, st.cacheHit, st.credit)
	}
}

// mapInt 从 map 取整数字段（JSON 数字 → float64）；缺失或类型不符返回 0。
func mapInt(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	return 0
}

// mapString 从 map 取字符串字段；缺失或类型不符返回空串。
func mapString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, _ := m[key].(string)
	return v
}

// aliasModel 查 config model_alias 别名表（多协议入站用：客户端硬编码他方模型名
// 时映射到网关实际模型）。空表/未命中 = 原样透传。
func (h *Handler) aliasModel(name string) string {
	if name == "" || len(h.cfg.ModelAlias) == 0 {
		return name
	}
	if v, ok := h.cfg.ModelAlias[name]; ok && v != "" {
		return v
	}
	return name
}
