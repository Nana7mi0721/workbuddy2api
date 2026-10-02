package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Gateway 是到 workbuddy2api 网关的 HTTP 客户端。
// 面板在服务端注入 api_key，浏览器端不会看到密钥（也没有 CORS 问题）。
type Gateway struct {
	base string
	key  string
	hc   *http.Client
}

func NewGateway(base, key string) *Gateway {
	return &Gateway{
		base: strings.TrimRight(base, "/"),
		key:  key,
		hc:   &http.Client{Timeout: 30 * time.Second},
	}
}

type gwResp struct {
	Status  int
	Body    []byte
	Elapsed time.Duration
	Err     error
}

func (r gwResp) String() string {
	if r.Err != nil {
		return r.Err.Error()
	}
	return fmt.Sprintf("http %d", r.Status)
}

// do 发一次带上游鉴权的请求。body 为 nil 时不发请求体。
func (g *Gateway) do(method, path string, body any) gwResp {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return gwResp{Err: err}
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, g.base+path, rdr)
	if err != nil {
		return gwResp{Err: err}
	}
	if g.key != "" {
		req.Header.Set("Authorization", "Bearer "+g.key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	resp, err := g.hc.Do(req)
	if err != nil {
		return gwResp{Err: err, Elapsed: time.Since(start)}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return gwResp{Err: err, Status: resp.StatusCode, Elapsed: time.Since(start)}
	}
	return gwResp{Status: resp.StatusCode, Body: b, Elapsed: time.Since(start)}
}

func (g *Gateway) Health() gwResp { return g.do(http.MethodGet, "/healthz", nil) }

// ---- 透传接口 ----

func (g *Gateway) Status() gwResp  { return g.do(http.MethodGet, "/status", nil) }
func (g *Gateway) Stats() gwResp   { return g.do(http.MethodGet, "/v1/stats", nil) }
func (g *Gateway) Models() gwResp  { return g.do(http.MethodGet, "/v1/models", nil) }
func (g *Gateway) Metrics() gwResp { return g.do(http.MethodGet, "/metrics", nil) }

// StatsHistory 按日聚合统计（网关 /v1/stats/history）：面板统计页长期趋势数据源。
func (g *Gateway) StatsHistory(days int) gwResp {
	return g.do(http.MethodGet, fmt.Sprintf("/v1/stats/history?days=%d", days), nil)
}

func (g *Gateway) ResetStats() gwResp {
	return g.do(http.MethodPost, "/v1/stats/reset", nil)
}

// Checkin 调 /v1/checkin（全量签到 + 余额刷新）。逐号 2~3 次上游调用，账号多时
// 会超过通用 30s 客户端超时，这里用独立 3 分钟长超时客户端。
func (g *Gateway) Checkin() gwResp {
	c := &http.Client{Timeout: 3 * time.Minute}
	req, err := http.NewRequest(http.MethodPost, g.base+"/v1/checkin", nil)
	if err != nil {
		return gwResp{Err: err}
	}
	if g.key != "" {
		req.Header.Set("Authorization", "Bearer "+g.key)
	}
	start := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return gwResp{Err: err, Elapsed: time.Since(start)}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return gwResp{Err: err, Status: resp.StatusCode, Elapsed: time.Since(start)}
	}
	return gwResp{Status: resp.StatusCode, Body: b, Elapsed: time.Since(start)}
}

type adminReq struct {
	Reason string `json:"reason,omitempty"`
}

// AdminAccount 调 /admin/accounts/{uid}/{action}，action ∈ disable|enable|revive。
func (g *Gateway) AdminAccount(uid, action, reason string) gwResp {
	p := "/admin/accounts/" + urlPathEscape(uid) + "/" + action
	var body any
	if action == "disable" {
		body = adminReq{Reason: reason}
	}
	return g.do(http.MethodPost, p, body)
}

// RunTask 调 /admin/tasks/{name}/run。
func (g *Gateway) RunTask(name string) gwResp {
	return g.do(http.MethodPost, "/admin/tasks/"+urlPathEscape(name)+"/run", nil)
}

func urlPathEscape(s string) string {
	// uid 只含 [0-9a-f-]，但保持通用。
	r := strings.NewReplacer("%", "%25", "/", "%2F", " ", "%20", "#", "%23", "?", "%3F")
	return r.Replace(s)
}

// gatewayError 把网关的错误信封翻译成一句话（失败时回退为原始文本）。
func gatewayError(resp gwResp) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(resp.Body, &e) == nil && e.Error.Message != "" {
		if e.Error.Code != "" {
			return fmt.Sprintf("%s (%s)", e.Error.Message, e.Error.Code)
		}
		return e.Error.Message
	}
	txt := strings.TrimSpace(string(resp.Body))
	if len(txt) > 200 {
		txt = txt[:200] + "…"
	}
	if txt == "" {
		txt = resp.String()
	}
	return txt
}

// ---- /status 解析（面板需要账号 uid 列表做本地文件关联） ----

type StatusAccount struct {
	UID             string          `json:"uid"`
	Realm           string          `json:"realm"`
	Nickname        string          `json:"nickname"`
	Credits         int64           `json:"credits"`
	Cooling         bool            `json:"cooling"`
	CoolKind        string          `json:"cool_kind"`
	CoolRemaining   int64           `json:"cool_remaining_sec"`
	Until           string          `json:"until"`
	Reason          string          `json:"reason"`
	Disabled        bool            `json:"disabled"`
	DisabledReason  string          `json:"disabled_reason"`
	ManualDisabled  bool            `json:"manual_disabled"`
	ManualReason    string          `json:"manual_reason"`
	SuccessCount    int64           `json:"success_count"`
	ErrTotal        int64           `json:"err_total"`
	LastSuccess     string          `json:"last_success"`
	LastErr         string          `json:"last_err"`
	ConsecutiveFail int64           `json:"consecutive_fails"`
	InFlight        int64           `json:"in_flight"`
	BreakerFails    int64           `json:"breaker_fails"`
	BreakerUntil    string          `json:"breaker_until"`
	DegradeUntil    string          `json:"degrade_until"`
	SoftStreak      int64           `json:"soft_streak"`
	CreditsExpiring json.RawMessage `json:"credits_expiring"`
	ModelCosts      json.RawMessage `json:"model_costs"`
	RateLimited     json.RawMessage `json:"rate_limited_models"`
}

type StatusPayload struct {
	Accounts     []StatusAccount `json:"accounts"`
	Total        int             `json:"total"`
	Healthy      int             `json:"healthy"`
	Cooling      int             `json:"cooling"`
	Disabled     int             `json:"disabled"`
	InFlightFull int             `json:"in_flight_full"` // 上游是计数(int)，不是 bool
	RealmTotals  json.RawMessage `json:"realm_totals"`
	Sticky       json.RawMessage `json:"sticky_sessions"`
	RedisMode    string          `json:"redis_mode"`
	CostExplore  json.RawMessage `json:"cost_explore"`
	DailyBudget  json.RawMessage `json:"daily_budget"`
	TaskLedger   json.RawMessage `json:"task_ledger"`
}

func parseStatus(b []byte) (*StatusPayload, error) {
	var s StatusPayload
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}
