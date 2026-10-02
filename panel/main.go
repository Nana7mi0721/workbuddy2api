package main

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

const panelVersion = "1.2.1"

type App struct {
	cfg      *PanelConfig
	gateway  *Gateway
	runner   *ServiceRunner
	gwCfg    *GatewayConfig
	gwCfgErr string
	gwCfgMod time.Time // config.json 上次读取时的 mtime（变化才重读）
	sessKey  string
	started  time.Time
	lastErr  string
	panelLog *os.File
}

func main() {
	cfg, err := loadPanelConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "面板启动失败:", err)
		os.Exit(1)
	}

	// 面板自身日志：<网关目录>/logs/panel.log
	logPath := filepath.Join(cfg.BaseDir(), "logs", "panel.log")
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		log.SetOutput(io.MultiWriter(os.Stderr, f))
	}
	log.SetFlags(log.LstdFlags)
	log.Printf("=== panel %s 启动，listen=%s gateway=%s dir=%s", panelVersion, cfg.Listen, cfg.Gateway.BaseURL, cfg.BaseDir())

	app := &App{
		cfg:     cfg,
		runner:  &ServiceRunner{cfg: cfg},
		started: time.Now(),
		panelLog: f,
	}
	sum := sha256.Sum256([]byte("wbpanel:" + cfg.Auth.Password))
	app.sessKey = hex.EncodeToString(sum[:])
	app.reloadGatewayConfig()

	mux := http.NewServeMux()
	app.routes(mux)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           app.withLogging(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	url := "http://" + cfg.Listen
	if strings.HasPrefix(cfg.Listen, ":") {
		url = "http://127.0.0.1" + cfg.Listen
	}
	log.Printf("面板已就绪：%s", url)
	fmt.Printf("workbuddy2api 面板已启动: %s\n", url)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("监听 %s 失败: %v", cfg.Listen, err)
	}
}

// reloadGatewayConfig 读取网关 config.json（api_key / listen / admin.enabled）。
func (a *App) reloadGatewayConfig() {
	gc, err := a.cfg.readGatewayConfig()
	if err != nil {
		a.gwCfgErr = err.Error()
		log.Printf("读取网关配置失败: %v", err)
		gc = &GatewayConfig{}
	} else {
		a.gwCfgErr = ""
	}
	a.gwCfg = gc
	if fi, err := os.Stat(a.cfg.ConfigPath()); err == nil {
		a.gwCfgMod = fi.ModTime()
	} else {
		a.gwCfgMod = time.Time{}
	}
	a.gateway = NewGateway(a.cfg.Gateway.BaseURL, gc.APIKey)
}

// ensureGatewayConfig 懒加载：config.json 的 mtime 变了才重新读取。
// 面板进程可能比网关 config.json 活得久（外部改配置 / 配置页保存 / 网关重启换配置），
// 缓存一次用到底会让 admin.enabled、api_key 等状态全部过时——每个 /api 请求前做一次
// os.Stat 的代价可以忽略，换来配置状态永远与磁盘一致。
func (a *App) ensureGatewayConfig() {
	fi, err := os.Stat(a.cfg.ConfigPath())
	if err != nil {
		return // 读不到就沿用缓存（readGatewayConfig 的错误路径已有兜底）
	}
	if !fi.ModTime().Equal(a.gwCfgMod) {
		log.Printf("检测到 config.json 变化（%s → %s），重新读取", a.gwCfgMod.Format("15:04:05"), fi.ModTime().Format("15:04:05"))
		a.reloadGatewayConfig()
	}
}

func (a *App) routes(mux *http.ServeMux) {
	// ---- 面板自身 ----
	mux.HandleFunc("GET /api/panel/info", a.hPanelInfo)
	mux.HandleFunc("POST /api/panel/login", a.hLogin)
	mux.HandleFunc("POST /api/panel/logout", a.hLogout)
	mux.HandleFunc("GET /api/panel/config", a.hPanelConfigGet)
	mux.HandleFunc("PUT /api/panel/config", a.hPanelConfigPut)

	// ---- 网关透传 ----
	mux.HandleFunc("GET /api/overview", a.hOverview)
	mux.HandleFunc("GET /api/health", a.hHealth)
	mux.HandleFunc("GET /api/status", a.hStatus)
	mux.HandleFunc("GET /api/stats", a.hStats)
	mux.HandleFunc("POST /api/stats/reset", a.hStatsReset)
	mux.HandleFunc("GET /api/models", a.hModels)
	mux.HandleFunc("GET /api/metrics", a.hMetrics)

	// ---- API 接入（API 配置页：接入信息/密钥列表，密钥默认掩码） ----
	mux.HandleFunc("GET /api/access", a.hAccess)
	mux.HandleFunc("POST /api/access/reveal", a.hAccessReveal)
	// 概览页快捷操作：面板代触发网关全量签到（服务端注入 api_key）。
	mux.HandleFunc("POST /api/checkin-proxy", a.hCheckinProxy)

	// ---- 账号 ----
	mux.HandleFunc("GET /api/accounts", a.hAccounts)
	mux.HandleFunc("POST /api/accounts/login/start", a.hLoginStart)
	mux.HandleFunc("POST /api/accounts/login/poll", a.hLoginPoll)
	mux.HandleFunc("POST /api/accounts/{uid}/{action}", a.hAccountAction)
	mux.HandleFunc("DELETE /api/accounts/{uid}", a.hAccountDelete)

	// ---- 日志 ----
	mux.HandleFunc("GET /api/logs", a.hLogs)
	mux.HandleFunc("GET /api/logs/stream", a.hLogStream)
	mux.HandleFunc("GET /api/logs/download", a.hLogDownload)

	// ---- 配置 / 审计 ----
	mux.HandleFunc("GET /api/config", a.hConfigGet)
	mux.HandleFunc("PUT /api/config", a.hConfigPut)
	mux.HandleFunc("GET /api/audit", a.hAudit)

	// ---- 服务 ----
	mux.HandleFunc("GET /api/service", a.hService)
	mux.HandleFunc("POST /api/service/{action}", a.hServiceAction)

	// ---- 任务 ----
	mux.HandleFunc("GET /api/tasks", a.hTasks)
	mux.HandleFunc("POST /api/tasks/{name}/run", a.hTaskRun)

	// ---- 静态资源 ----
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("内嵌资源不可用: %v", err)
	}
	mux.Handle("/", spaHandler(sub))
}

// spaHandler 提供内嵌前端；未知路径回落到 index.html（前端路由）。
// 资源全部 no-store：前端内嵌在二进制里，重建即换内容，缓存只会造成改版后浏览器
// 仍跑旧脚本（本次趋势图 clamp 修复就先撞过一次缓存）。
func spaHandler(sub fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(sub, p); err != nil {
			r = r.Clone(r.Context())
			r.URL.Path = "/index.html"
		}
		fileServer.ServeHTTP(w, r)
	})
}

// ---------- 中间件 ----------

func (a *App) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// 配置懒加载：mtime 变了才重读（成本一次 os.Stat）。
		if strings.HasPrefix(r.URL.Path, "/api/") {
			a.ensureGatewayConfig()
		}
		// 鉴权：配置了密码才校验（默认仅本机监听）。
		if a.cfg.Auth.Password != "" && strings.HasPrefix(r.URL.Path, "/api/") &&
			r.URL.Path != "/api/panel/login" && r.URL.Path != "/api/panel/info" {
			if !a.authed(r) {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "未登录或会话已过期", "code": "unauthorized"})
				return
			}
		}
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			log.Printf("%s %s → %d (%s)", r.Method, r.URL.Path, sw.code, time.Since(start).Round(time.Millisecond))
		}
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(c int) {
	w.code = c
	w.ResponseWriter.WriteHeader(c)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (a *App) authed(r *http.Request) bool {
	c, err := r.Cookie("wbpanel")
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(a.sessKey)) == 1
}

// ---------- 响应helper ----------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func ok(w http.ResponseWriter, v map[string]any) {
	if v == nil {
		v = map[string]any{}
	}
	v["ok"] = true
	writeJSON(w, http.StatusOK, v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}

func readBody(r *http.Request, v any) error {
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

// ---------- 面板自身 ----------

func (a *App) hPanelInfo(w http.ResponseWriter, r *http.Request) {
	info := map[string]any{
		"version":       panelVersion,
		"listen":        a.cfg.Listen,
		"base_dir":      a.cfg.BaseDir(),
		"config_file":   a.cfg.path,
		"gateway":       a.cfg.Gateway.BaseURL,
		"has_api_key":   a.gwCfg != nil && a.gwCfg.APIKey != "",
		"admin_enabled": a.gwCfg != nil && a.gwCfg.Admin.Enabled,
		"metrics_on":    a.gwCfg != nil && a.gwCfg.Metrics.Enabled,
		"auth_required": a.cfg.Auth.Password != "",
		"authed":        a.cfg.Auth.Password == "" || a.authed(r),
		"started_at":    a.started,
		"ui":            a.cfg.UI,
		"config_error":  a.gwCfgErr,
		"gateway_config": a.gatewayConfigSanitized(),
	}
	ok(w, info)
}

// gatewayConfigSanitized 把网关 config.json 原样交给前端（剥掉 api_key，换成掩码）。
func (a *App) gatewayConfigSanitized() map[string]any {
	raw, err := os.ReadFile(a.cfg.ConfigPath())
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]any{"error": err.Error()}
	}
	if k, isStr := m["api_key"].(string); isStr {
		m["api_key_masked"] = maskKey(k)
		delete(m, "api_key")
	}
	return m
}

// ---------- API 接入（API 配置页） ----------

// accessKey 一把网关鉴权密钥的展示形态：默认只出掩码，明文经 /api/access/reveal
// 按名单把取用（取用落服务端日志）。与配置页直接看明文的现状相比，这里是收口而非放权。
type accessKey struct {
	Name   string   `json:"name"`
	Masked string   `json:"masked"`
	Groups []string `json:"groups,omitempty"`
	Main   bool     `json:"main"`
}

// hAccess GET /api/access：API 配置页的接入信息（base_url / 协议清单 / 密钥掩码列表）。
func (a *App) hAccess(w http.ResponseWriter, r *http.Request) {
	gc := a.gwCfg
	if gc == nil {
		fail(w, http.StatusBadGateway, "网关配置不可读: "+a.gwCfgErr)
		return
	}
	keys := make([]accessKey, 0, 1+len(gc.APIKeys))
	if gc.APIKey != "" {
		keys = append(keys, accessKey{Name: "主密钥（不限分组）", Masked: maskKey(gc.APIKey), Main: true})
	}
	for _, k := range gc.APIKeys {
		if k.Key == "" {
			continue
		}
		keys = append(keys, accessKey{Name: k.Name, Masked: maskKey(k.Key), Groups: k.Groups})
	}
	ok(w, map[string]any{
		"base_url":      accessBaseURL(gc.Listen),
		"listen":        gc.Listen,
		"protocols":     accessProtocols(),
		"keys":          keys,
		"admin_enabled": gc.Admin.Enabled,
	})
}

// accessBaseURL 从网关 listen（":7863" / "0.0.0.0:7863" / "127.0.0.1:7863"）拼
// 本机访问地址；listen 空/异常回落部署默认 :7863。
func accessBaseURL(listen string) string {
	if listen == "" {
		listen = ":7863"
	}
	i := strings.LastIndex(listen, ":")
	host, port := listen[:i+1], listen[i+1:]
	host = strings.TrimSuffix(host, ":")
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + host + ":" + port
}

// accessProtocols 三协议端点清单（静态：与网关路由一致）。
func accessProtocols() []map[string]any {
	return []map[string]any{
		{"protocol": "OpenAI Chat", "path": "/v1/chat/completions", "desc": "OpenAI 兼容对话（绝大多数工具/客户端）"},
		{"protocol": "OpenAI Responses", "path": "/v1/responses", "desc": "Codex CLI 及 Responses 协议客户端"},
		{"protocol": "Anthropic", "path": "/v1/messages", "desc": "Claude Code / Anthropic SDK（x-api-key 头）"},
		{"protocol": "通用", "path": "/v1/models", "desc": "模型列表（带 anthropic-version 头时返回 Anthropic 形状）"},
		{"protocol": "Anthropic", "path": "/v1/messages/count_tokens", "desc": "输入 token 估算（不打上游）"},
	}
}

// hAccessReveal POST /api/access/reveal {name:"main"|<分组密钥名>}：按名返回单把
// 密钥明文。鉴权由 withLogging 统一处理（配置了密码必须已登录）；取用留痕。
func (a *App) hAccessReveal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	_ = readBody(r, &body)
	gc := a.gwCfg
	if gc == nil {
		fail(w, http.StatusBadGateway, "网关配置不可读: "+a.gwCfgErr)
		return
	}
	var key, label string
	switch {
	case body.Name == "main":
		key, label = gc.APIKey, "主密钥"
	default:
		for _, k := range gc.APIKeys {
			if k.Name != "" && k.Name == body.Name {
				key, label = k.Key, k.Name
				break
			}
		}
	}
	if key == "" {
		fail(w, http.StatusNotFound, "密钥不存在或为空")
		return
	}
	log.Printf("密钥明文取用: name=%q label=%q from=%s", body.Name, label, r.RemoteAddr)
	ok(w, map[string]any{"key": key, "name": body.Name})
}

// hCheckinProxy POST /api/checkin-proxy：面板代触发网关全量签到（/v1/checkin）。
// 网关侧带 30s 冷却与 api_key 鉴权（此处服务端注入）；返回体透传签到报告。
func (a *App) hCheckinProxy(w http.ResponseWriter, r *http.Request) {
	resp := a.gateway.Checkin()
	if resp.Err != nil {
		fail(w, http.StatusBadGateway, resp.Err.Error())
		return
	}
	if resp.Status != http.StatusOK {
		fail(w, resp.Status, gatewayError(resp))
		return
	}
	var rep struct {
		Total   int `json:"total"`
		OK      int `json:"ok"`
		Already int `json:"already"`
		Fail    int `json:"fail"`
		Skipped int `json:"skipped"`
	}
	_ = json.Unmarshal(resp.Body, &rep)
	ok(w, map[string]any{"report": rep, "elapsed_ms": resp.Elapsed.Milliseconds()})
}

// scheduleEnabled 读出各任务开关（供任务页显示「已启用/已停用」）。
func (a *App) scheduleEnabled() map[string]bool {
	out := map[string]bool{}
	cfgMap := a.gatewayConfigSanitized()
	sch, _ := cfgMap["schedule"].(map[string]any)
	for _, n := range knownTasks {
		v, ok := sch[n+"_enabled"].(bool)
		out[n] = !ok || v // 缺省 true（与网关 Default() 一致）
	}
	return out
}

func (a *App) hLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	_ = readBody(r, &body)
	if a.cfg.Auth.Password == "" {
		ok(w, map[string]any{"note": "未配置密码，无需登录"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Password), []byte(a.cfg.Auth.Password)) != 1 {
		fail(w, http.StatusUnauthorized, "密码不正确")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "wbpanel", Value: a.sessKey, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 7 * 24 * 3600})
	ok(w, nil)
}

func (a *App) hLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "wbpanel", Value: "", Path: "/", MaxAge: -1})
	ok(w, nil)
}

func (a *App) hPanelConfigGet(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"config": a.cfg})
}

func (a *App) hPanelConfigPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Listen     *string `json:"listen"`
		BaseURL    *string `json:"base_url"`
		Password   *string `json:"password"`
		RefreshMS  *int    `json:"refresh_ms"`
		StartScr   *string `json:"start_script"`
		GatewayDir *string `json:"dir"`
	}
	if err := readBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if body.Listen != nil && strings.TrimSpace(*body.Listen) != "" {
		a.cfg.Listen = strings.TrimSpace(*body.Listen)
	}
	if body.BaseURL != nil && strings.TrimSpace(*body.BaseURL) != "" {
		a.cfg.Gateway.BaseURL = strings.TrimSpace(*body.BaseURL)
	}
	if body.Password != nil {
		a.cfg.Auth.Password = *body.Password
		sum := sha256.Sum256([]byte("wbpanel:" + *body.Password))
		a.sessKey = hex.EncodeToString(sum[:])
	}
	if body.RefreshMS != nil && *body.RefreshMS >= 500 {
		a.cfg.UI.RefreshMS = *body.RefreshMS
	}
	if body.StartScr != nil {
		a.cfg.Gateway.StartScript = *body.StartScr
	}
	if body.GatewayDir != nil && strings.TrimSpace(*body.GatewayDir) != "" {
		a.cfg.Gateway.Dir = strings.TrimSpace(*body.GatewayDir)
	}
	if err := a.cfg.save(); err != nil {
		fail(w, http.StatusInternalServerError, "写入 panel.json 失败: "+err.Error())
		return
	}
	a.reloadGatewayConfig()
	ok(w, map[string]any{"note": "已保存（监听地址/网关地址的改动需重启面板生效）", "config": a.cfg})
}

// ---------- 网关透传 ----------

func (a *App) hHealth(w http.ResponseWriter, r *http.Request) {
	resp := a.gateway.Health()
	out := map[string]any{"http_status": resp.Status, "elapsed_ms": resp.Elapsed.Milliseconds()}
	if resp.Err != nil {
		out["reachable"] = false
		out["error"] = resp.Err.Error()
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": resp.Err.Error()})
		return
	}
	var body map[string]any
	_ = json.Unmarshal(resp.Body, &body)
	out["reachable"] = true
	out["body"] = body
	out["healthy"] = resp.Status == http.StatusOK
	ok(w, out)
}

func (a *App) hStatus(w http.ResponseWriter, r *http.Request) {
	resp := a.gateway.Status()
	if resp.Err != nil {
		fail(w, http.StatusBadGateway, "无法连接网关："+resp.Err.Error())
		return
	}
	if resp.Status != http.StatusOK {
		fail(w, resp.Status, gatewayError(resp))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(resp.Body)
}

func (a *App) hStats(w http.ResponseWriter, r *http.Request) {
	resp := a.gateway.Stats()
	if resp.Err != nil {
		fail(w, http.StatusBadGateway, "无法连接网关："+resp.Err.Error())
		return
	}
	if resp.Status != http.StatusOK {
		fail(w, resp.Status, gatewayError(resp))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(resp.Body)
}

func (a *App) hStatsReset(w http.ResponseWriter, r *http.Request) {
	resp := a.gateway.ResetStats()
	if resp.Err != nil {
		fail(w, http.StatusBadGateway, resp.Err.Error())
		return
	}
	if resp.Status >= 300 {
		fail(w, resp.Status, gatewayError(resp))
		return
	}
	ok(w, map[string]any{"note": "统计已重置"})
}

func (a *App) hModels(w http.ResponseWriter, r *http.Request) {
	resp := a.gateway.Models()
	if resp.Err != nil {
		fail(w, http.StatusBadGateway, resp.Err.Error())
		return
	}
	if resp.Status != http.StatusOK {
		fail(w, resp.Status, gatewayError(resp))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(resp.Body)
}

func (a *App) hMetrics(w http.ResponseWriter, r *http.Request) {
	resp := a.gateway.Metrics()
	if resp.Err != nil {
		fail(w, http.StatusBadGateway, resp.Err.Error())
		return
	}
	if resp.Status != http.StatusOK {
		fail(w, resp.Status, gatewayError(resp))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(resp.Body)
}

// hOverview 一次拉齐首页需要的所有数据（并行）。
func (a *App) hOverview(w http.ResponseWriter, r *http.Request) {
	type result struct {
		key  string
		resp gwResp
	}
	ch := make(chan result, 3)
	go func() { ch <- result{"health", a.gateway.Health()} }()
	go func() { ch <- result{"status", a.gateway.Status()} }()
	go func() { ch <- result{"stats", a.gateway.Stats()} }()
	out := map[string]any{}
	raw := map[string]json.RawMessage{}
	for i := 0; i < 3; i++ {
		res := <-ch
		if res.resp.Err != nil {
			out[res.key+"_error"] = res.resp.Err.Error()
			continue
		}
		if res.resp.Status >= 300 {
			out[res.key+"_error"] = gatewayError(res.resp)
			continue
		}
		raw[res.key] = json.RawMessage(res.resp.Body)
	}
	for k, v := range raw {
		out[k] = v
	}
	svc := a.runner.Status(a.gwCfg)
	out["service"] = svc
	out["panel"] = map[string]any{"version": panelVersion, "started_at": a.started, "now": time.Now()}
	if s, err := readState(a.cfg.StatePath()); err == nil {
		out["state_accounts"] = len(s.Accounts)
	}
	ok(w, out)
}

// ---------- 账号 ----------

type MergedAccount struct {
	UID             string          `json:"uid"`
	Nickname        string          `json:"nickname"`
	Realm           string          `json:"realm"`
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
	ModelCosts      json.RawMessage `json:"model_costs"`
	RateLimited     json.RawMessage `json:"rate_limited_models"`
	CreditsExpiring json.RawMessage `json:"credits_expiring"`
	// 本地文件信息
	InPool       bool   `json:"in_pool"`
	InConfig     bool   `json:"in_config"`
	FileActive   bool   `json:"file_active"`
	FilePaused   bool   `json:"file_paused"`
	FileName     string `json:"file_name,omitempty"`
	FileModTime  int64  `json:"file_modtime,omitempty"`
	TokenExpiry  int64  `json:"token_expires_at,omitempty"`
	TokenExpired bool   `json:"token_expired"`
	Broken       string `json:"broken,omitempty"`
	// state.json 里的持久化数据
	StateCredits    *int64 `json:"state_credits,omitempty"`
	LastStateUpdate string `json:"state_updated,omitempty"`
	StateCoolKind   string `json:"state_cool_kind,omitempty"`
	StateCoolUntil  string `json:"state_cool_until,omitempty"`
}

// isZeroTime 判断 RFC3339 时间串是否为零值/空（state.json 里未冷却时写 0001-01-01…）。
func isZeroTime(s string) bool {
	if s == "" {
		return true
	}
	return strings.HasPrefix(s, "0001-01-01")
}

func (a *App) hAccounts(w http.ResponseWriter, r *http.Request) {
	active := scanAuthDir(a.cfg.AuthDir(), "active")
	paused := scanAuthDir(a.cfg.PausedDir(), "paused")

	local := map[string]*AccountFile{}
	for i := range active {
		local[active[i].UID] = &active[i]
	}
	for i := range paused {
		if _, dup := local[paused[i].UID]; !dup {
			local[paused[i].UID] = &paused[i]
		}
	}

	var pool map[string]*StatusAccount
	poolErr := ""
	var inFlightFull int
	var poolCounts = map[string]int{}
	var realmTotals json.RawMessage
	var sticky json.RawMessage
	var ledger json.RawMessage
	var budget json.RawMessage
	resp := a.gateway.Status()
	if resp.Err != nil {
		poolErr = "无法连接网关: " + resp.Err.Error()
	} else if resp.Status != http.StatusOK {
		poolErr = gatewayError(resp)
	} else if st, err := parseStatus(resp.Body); err == nil {
		pool = map[string]*StatusAccount{}
		for i := range st.Accounts {
			pool[st.Accounts[i].UID] = &st.Accounts[i]
		}
		inFlightFull = st.InFlightFull
		poolCounts = map[string]int{"total": st.Total, "healthy": st.Healthy, "cooling": st.Cooling, "disabled": st.Disabled, "in_flight_full": st.InFlightFull}
		realmTotals = st.RealmTotals
		sticky = st.Sticky
		ledger = st.TaskLedger
		budget = st.DailyBudget
	} else {
		poolErr = "解析 /status 失败: " + err.Error()
	}

	var stateAcc map[string]StateAccount
	var stateUpdated string
	var stateErr string
	if s, err := readState(a.cfg.StatePath()); err == nil {
		stateAcc = s.Accounts
		stateUpdated = s.Updated
		if stateUpdated == "" {
			// 上游 state.json 没有 updated 键（顶层只有 accounts），
			// 退化为文件 mtime——运维真正关心的是「这份落盘状态有多新」。
			if fi, err2 := os.Stat(a.cfg.StatePath()); err2 == nil {
				stateUpdated = fi.ModTime().Format(time.RFC3339)
			}
		}
	} else {
		stateErr = err.Error()
	}

	now := time.Now().Unix()
	seen := map[string]bool{}
	out := make([]*MergedAccount, 0, len(local)+len(pool))
	add := func(uid string) {
		if seen[uid] {
			return
		}
		seen[uid] = true
		m := &MergedAccount{UID: uid}
		if p, ok := pool[uid]; ok {
			m.InPool = true
			m.Nickname = p.Nickname
			m.Realm = p.Realm
			m.Credits = p.Credits
			m.Cooling = p.Cooling
			m.CoolKind = p.CoolKind
			m.CoolRemaining = p.CoolRemaining
			m.Until = p.Until
			m.Reason = p.Reason
			m.Disabled = p.Disabled
			m.DisabledReason = p.DisabledReason
			m.ManualDisabled = p.ManualDisabled
			m.ManualReason = p.ManualReason
			m.SuccessCount = p.SuccessCount
			m.ErrTotal = p.ErrTotal
			m.LastSuccess = p.LastSuccess
			m.LastErr = p.LastErr
			m.ConsecutiveFail = p.ConsecutiveFail
			m.InFlight = p.InFlight
			m.BreakerFails = p.BreakerFails
			m.BreakerUntil = p.BreakerUntil
			m.DegradeUntil = p.DegradeUntil
			m.ModelCosts = p.ModelCosts
			m.RateLimited = p.RateLimited
			m.CreditsExpiring = p.CreditsExpiring
		}
		if l, ok := local[uid]; ok {
			m.InConfig = true
			if m.Nickname == "" {
				m.Nickname = l.Nickname
			}
			if m.Realm == "" {
				m.Realm = l.Realm
			}
			m.FileActive = l.Dir == "active"
			m.FilePaused = l.Dir == "paused"
			m.FileName = l.File
			m.FileModTime = l.ModTime
			m.TokenExpiry = l.ExpiresAt
			m.TokenExpired = l.ExpiresAt > 0 && l.ExpiresAt < now
			m.Broken = l.Broken
		}
		if stateAcc != nil {
			if sa, ok := stateAcc[uid]; ok {
				if v, err := sa.Credits.Int64(); err == nil {
					vv := v
					m.StateCredits = &vv
				}
				m.LastStateUpdate = sa.LastSuccess
				if m.CoolKind == "" {
					if k := sa.CoolKindText(); k != "" && !isZeroTime(sa.Until) {
						m.StateCoolKind = k
						m.StateCoolUntil = sa.Until
					}
				}
				// Status 不含快过期积分，只有 state.json 有。
				if len(m.CreditsExpiring) == 0 && sa.CreditsExpiring > 0 {
					if b, err := json.Marshal(sa.CreditsExpiring); err == nil {
						m.CreditsExpiring = b
					}
				}
				if len(m.ModelCosts) == 0 && len(sa.ModelCosts) > 0 {
					m.ModelCosts = sa.ModelCosts
				}
			}
		}
		out = append(out, m)
	}
	for uid := range pool {
		add(uid)
	}
	for uid := range local {
		add(uid)
	}
	for uid := range stateAcc {
		if _, hasLocal := local[uid]; !hasLocal {
			if _, hasPool := pool[uid]; !hasPool {
				continue // state.json 里的历史账号，不单独列出
			}
		}
		add(uid)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].InPool != out[j].InPool {
			return out[i].InPool
		}
		return out[i].Credits > out[j].Credits
	})

	summary := map[string]any{
		"total":          len(out),
		"pool":           len(pool),
		"pool_counts":    poolCounts,
		"active_files":   len(active),
		"paused_files":   len(paused),
		"in_flight_full": inFlightFull,
		"realm_totals":   realmTotals,
		"sticky":         sticky,
		"task_ledger":    ledger,
		"daily_budget":   budget,
		"state_updated":  stateUpdated,
		"state_error":    stateErr,
		"state_path":     a.cfg.StatePath(),
	}
	if poolErr != "" {
		summary["error"] = poolErr
	}
	ok(w, map[string]any{"accounts": out, "summary": summary})
}

func (a *App) hAccountAction(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	action := r.PathValue("action")
	var body struct {
		Reason string `json:"reason"`
		Realm  string `json:"realm"`
	}
	_ = readBody(r, &body)

	switch action {
	case "disable", "enable", "revive":
		if !a.gwCfg.Admin.Enabled {
			fail(w, http.StatusConflict, "网关 config.json 里 admin.enabled=false，/admin/* 路由未注册；请在「配置」页开启后重启网关")
			return
		}
		resp := a.gateway.AdminAccount(uid, action, body.Reason)
		if resp.Err != nil {
			fail(w, http.StatusBadGateway, resp.Err.Error())
			return
		}
		if resp.Status >= 300 {
			fail(w, resp.Status, gatewayError(resp))
			return
		}
		var v map[string]any
		_ = json.Unmarshal(resp.Body, &v)
		ok(w, map[string]any{"result": v, "note": adminActionNote(action)})
	case "pause":
		dst, err := MoveAccount(a.cfg.AuthDir(), a.cfg.PausedDir(), uid)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		ok(w, map[string]any{"path": dst, "note": "已移出 auths\\（网关 5 秒内热加载移除该账号，无需重启）"})
	case "resume":
		dst, err := MoveAccount(a.cfg.PausedDir(), a.cfg.AuthDir(), uid)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		ok(w, map[string]any{"path": dst, "note": "已放回 auths\\，网关 5 秒内自动加载"})
	default:
		fail(w, http.StatusNotFound, "未知操作: "+action)
	}
}

func adminActionNote(action string) string {
	switch action {
	case "disable":
		return "已手动禁用（清手动位需 enable）"
	case "enable":
		return "已清除手动禁用标记"
	case "revive":
		return "已清除自动禁用/熔断标记"
	}
	return ""
}

func (a *App) hAccountDelete(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	// 安全策略：只允许彻底删除「已暂停」目录里的文件，运行中的账号必须先暂停。
	if p, err := DeleteAccountFile(a.cfg.PausedDir(), uid); err == nil {
		ok(w, map[string]any{"deleted": p, "note": "已从 auths-paused\\ 删除"})
		return
	}
	if p, err := findAccountFile(a.cfg.AuthDir(), uid); err == nil {
		fail(w, http.StatusConflict, "该账号仍在使用中（"+filepath.Base(p)+"）。请先「暂停」，再从暂停区删除。")
		return
	}
	fail(w, http.StatusNotFound, "未找到账号文件: "+uid)
}

func (a *App) hLoginStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Realm string `json:"realm"`
	}
	_ = readBody(r, &body)
	flow, err := StartLogin(a.cfg, body.Realm)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	ok(w, map[string]any{"url": flow.URL, "realm": flow.Realm, "started_at": flow.StartedAt, "raw": flow.Output})
}

func (a *App) hLoginPoll(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Realm string `json:"realm"`
		Save  *bool  `json:"save"`
	}
	_ = readBody(r, &body)
	save := body.Save == nil || *body.Save
	lo, file, err := PollLogin(a.cfg, body.Realm, save)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	exp := time.Now().Add(time.Duration(lo.ExpiresIn) * time.Second).Unix()
	ok(w, map[string]any{
		"uid":              lo.UID,
		"nickname":         lo.Nickname,
		"realm":            firstNonEmpty(lo.Realm, normalizeRealm(body.Realm)),
		"enterprise_id":    lo.EnterpriseID,
		"domain":           lo.Domain,
		"expires_at":       exp,
		"file":             file,
		"note":             "账号已写入 auths\\，网关 5 秒内自动加载",
	})
}

// ---------- 日志 ----------

func (a *App) hLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 5000 {
		limit = a.cfg.UI.LogTail
	}
	kw := strings.ToLower(strings.TrimSpace(q.Get("q")))
	kind := q.Get("kind")
	level := q.Get("level")
	uid := q.Get("uid")
	model := q.Get("model")
	// 带 kind/level 等过滤时，「limit」指的是**过滤后**的条数：先把原始尾部多取
	// 20 倍再过滤，否则概览页要「最近 8 条请求行」会拿到文件最后 8 行（往往是
	// 启动 banner），过滤完为空。16MB 上限防超大日志拖垮内存。
	rawN := limit
	if (kind != "" && kind != "all") || (level != "" && level != "all") || uid != "" || model != "" || kw != "" {
		rawN = limit * 20
	}
	if rawN > 8000 {
		rawN = 8000
	}
	entries, total, err := tailLines(a.cfg.LogPath(), rawN, 16<<20)
	if err != nil {
		fail(w, http.StatusNotFound, "日志不可读: "+err.Error())
		return
	}
	filtered := make([]LogEntry, 0, len(entries))
	for _, e := range entries {
		if kind != "" && kind != "all" && e.Kind != kind {
			continue
		}
		if level != "" && level != "all" && e.Level != level {
			continue
		}
		if uid != "" && !strings.HasPrefix(e.UID8, uid) && !strings.Contains(e.Raw, uid) {
			continue
		}
		if model != "" && e.Model != model {
			continue
		}
		if kw != "" && !strings.Contains(strings.ToLower(e.Raw), kw) {
			continue
		}
		filtered = append(filtered, e)
	}
	if len(filtered) > limit {
		filtered = filtered[len(filtered)-limit:] // 文件序尾部 = 最新的 limit 条
	}
	ok(w, map[string]any{
		"entries":    filtered,
		"total_lines": total,
		"shown":      len(filtered),
		"file":       a.cfg.LogPath(),
		"file_size":  statFile(a.cfg.LogPath()).Size,
	})
}

func (a *App) hLogDownload(w http.ResponseWriter, r *http.Request) {
	f, err := os.Open(a.cfg.LogPath())
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="console.log"`)
	if st != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	}
	_, _ = io.Copy(w, f)
}

// hLogStream 用 SSE 实时追加（轮询文件增长；Windows 下比 inotify 稳）。
func (a *App) hLogStream(w http.ResponseWriter, r *http.Request) {
	flusher, canFlush := w.(http.Flusher)
	if !canFlush {
		fail(w, http.StatusInternalServerError, "当前连接不支持流式输出")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	path := a.cfg.LogPath()
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %q\n\n", err.Error())
		flusher.Flush()
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	var offset int64
	var lineNo int
	// 只跟到最后 N 行，避免首屏刷爆。
	if st != nil && st.Size() > 256<<10 {
		offset = st.Size() - 256<<10
	}
	if st != nil {
		lineNo = 0
	}
	if _, err := f.Seek(offset, 0); err != nil {
		return
	}
	reader := bufio.NewReaderSize(f, 1<<20)
	send := func(event string, payload any) bool {
		b, err := json.Marshal(payload)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	ticker := time.NewTicker(700 * time.Millisecond)
	defer ticker.Stop()
	// 首次把已有尾部推给前端（标记 initial，避免重复渲染）。
	_ = send("ready", map[string]any{"file": path, "offset": offset, "line": lineNo})
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			cur, err := f.Stat()
			if err != nil {
				return
			}
			if cur.Size() < offset { // 文件被截断/轮转
				offset = 0
				lineNo = 0
				_, _ = f.Seek(0, 0)
				reader.Reset(f)
				if !send("reset", map[string]any{"reason": "日志被截断"}) {
					return
				}
			}
			for {
				line, err := reader.ReadString('\n')
				if len(line) > 0 {
					if !strings.HasSuffix(line, "\n") {
						// 半行：退回去等下次
						if _, serr := f.Seek(-int64(len(line)), io.SeekCurrent); serr == nil {
							reader.Reset(f)
							break
						}
					} else {
						lineNo++
						entry := parseLogLine(lineNo, line)
						if !send("line", entry) {
							return
						}
						offset += int64(len(line))
						continue
					}
				}
				if err != nil {
					break
				}
			}
		}
	}
}

// ---------- 配置 / 审计 ----------

func (a *App) hConfigGet(w http.ResponseWriter, r *http.Request) {
	v, err := readConfigView(a.cfg.ConfigPath())
	if err != nil {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	ok(w, map[string]any{"config": v, "summary": summarizeGatewayConfig(a.gwCfg, a.gwCfgErr)})
}

func summarizeGatewayConfig(gc *GatewayConfig, errMsg string) map[string]any {
	m := map[string]any{"error": errMsg}
	if gc == nil {
		return m
	}
	m["listen"] = gc.Listen
	m["api_key_set"] = gc.APIKey != ""
	m["api_key_masked"] = maskKey(gc.APIKey)
	m["admin_enabled"] = gc.Admin.Enabled
	m["admin_audit_enabled"] = gc.Admin.AuditEnabled
	m["metrics_enabled"] = gc.Metrics.Enabled
	return m
}

func maskKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 8 {
		return strings.Repeat("*", len(k))
	}
	return k[:4] + strings.Repeat("*", 6) + k[len(k)-4:]
}

func (a *App) hConfigPut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := readBody(r, &body); err != nil {
		fail(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		fail(w, http.StatusBadRequest, "内容为空")
		return
	}
	backup, err := writeConfigFile(a.cfg.ConfigPath(), body.Text)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.reloadGatewayConfig()
	ok(w, map[string]any{
		"backup": backup,
		"note":   "已写入 config.json；配置改动需重启网关生效（服务页可一键重启）",
		"summary": summarizeGatewayConfig(a.gwCfg, a.gwCfgErr),
	})
}

func (a *App) hAudit(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 2000 {
		limit = 200
	}
	items, err := tailAudit(a.cfg.AuditPath(), limit)
	if err != nil {
		// 审计文件只有开启 admin.audit_enabled 且真发生过管理动作才存在；
		// 「还没有」是正常状态，不该让前端拿到 404。
		if os.IsNotExist(err) || strings.Contains(err.Error(), "no such file") {
			ok(w, map[string]any{
				"items": []json.RawMessage{},
				"file":  a.cfg.AuditPath(),
				"note":  "审计日志尚未生成：需在网关 config.json 里开启 admin.audit_enabled 并执行至少一次管理操作",
			})
			return
		}
		fail(w, http.StatusNotFound, "审计日志不可读: "+err.Error())
		return
	}
	ok(w, map[string]any{"items": items, "file": a.cfg.AuditPath()})
}

// ---------- 服务 ----------

func (a *App) hService(w http.ResponseWriter, r *http.Request) {
	st := a.runner.Status(a.gwCfg)
	h := a.gateway.Health()
	resp := map[string]any{
		"service":      st,
		"reachable":    h.Err == nil,
		"health_status": h.Status,
	}
	if h.Err != nil {
		resp["health_error"] = h.Err.Error()
	} else {
		var body map[string]any
		_ = json.Unmarshal(h.Body, &body)
		resp["health"] = body
	}
	resp["log_lines"] = countLines(a.cfg.LogPath())
	ok(w, resp)
}

func (a *App) hServiceAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	switch action {
	case "start":
		msg, err := a.runner.Start(a.gwCfg)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		ok(w, map[string]any{"message": msg})
	case "stop":
		pid, err := a.runner.Stop(a.gwCfg)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		ok(w, map[string]any{"message": fmt.Sprintf("已停止 PID %d", pid)})
	case "restart":
		msg, err := a.runner.Restart(a.gwCfg)
		if err != nil {
			fail(w, http.StatusInternalServerError, err.Error()+" / "+msg)
			return
		}
		ok(w, map[string]any{"message": msg})
	case "reload":
		a.reloadGatewayConfig()
		ok(w, map[string]any{"message": "已重新读取网关 config.json"})
	default:
		fail(w, http.StatusNotFound, "未知操作: "+action)
	}
}

// ---------- 任务 ----------

var knownTasks = []string{"checkin", "activity", "keepalive", "travel", "school", "cat"}

func (a *App) hTasks(w http.ResponseWriter, r *http.Request) {
	type taskInfo struct {
		Name      string `json:"name"`
		Label     string `json:"label"`
		Hint      string `json:"hint"`
		Enabled   bool   `json:"enabled"`
		LastState string `json:"last_state,omitempty"`
		LastRun   string `json:"last_run,omitempty"`
		Trigger   string `json:"trigger,omitempty"`
		Note      string `json:"note,omitempty"`
		Elapsed   string `json:"elapsed,omitempty"`
		OK        int    `json:"ok"`
		Already   int    `json:"already"`
		Fail      int    `json:"fail"`
		Total     int    `json:"total"`
	}
	labels := map[string][2]string{
		"checkin":   {"签到", "每日签到 + 余额查询解冻"},
		"activity":  {"活跃上报", "点亮连登、补满领猫对话门槛"},
		"keepalive": {"token 保活", "刷新账号 token，防掉线"},
		"travel":    {"猫猫旅行", "领养 / 派出 / 领奖"},
		"school":    {"开学季任务", "school_open_day_2026 脚本"},
		"cat":       {"夜猫子任务", "task_runner.py black_cat"},
	}
	ledger := map[string]any{}
	if resp := a.gateway.Status(); resp.Err == nil && resp.Status == 200 {
		if st, err := parseStatus(resp.Body); err == nil && len(st.TaskLedger) > 0 {
			_ = json.Unmarshal(st.TaskLedger, &ledger)
		}
	}
	runs, _ := ledger["runs"].(map[string]any)
	enabled := a.scheduleEnabled()
	out := make([]taskInfo, 0, len(knownTasks))
	for _, n := range knownTasks {
		ti := taskInfo{Name: n, Label: labels[n][0], Hint: labels[n][1], Enabled: enabled[n]}
		if runs != nil {
			if rv, okv := runs[n]; okv {
				if m, okm := rv.(map[string]any); okm {
					if v, ok2 := m["finished"].(string); ok2 {
						ti.LastRun = v
					}
					if v, ok2 := m["trigger"].(string); ok2 {
						ti.Trigger = v
					}
					if v, ok2 := m["all_failed"].(bool); ok2 && v {
						ti.LastState = "all_failed"
					} else if okm {
						if f, _ := m["fail"].(float64); f > 0 {
							ti.LastState = "partial"
						} else {
							ti.LastState = "ok"
						}
					}
					if v, ok2 := m["note"].(string); ok2 {
						ti.Note = v
					}
					if v, ok2 := m["elapsed"].(string); ok2 {
						ti.Elapsed = v
					}
					if v, ok2 := m["ok"].(float64); ok2 {
						ti.OK = int(v)
					}
					if v, ok2 := m["already"].(float64); ok2 {
						ti.Already = int(v)
					}
					if v, ok2 := m["fail"].(float64); ok2 {
						ti.Fail = int(v)
					}
					if v, ok2 := m["total"].(float64); ok2 {
						ti.Total = int(v)
					}
				}
			}
		}
		out = append(out, ti)
	}
	ok(w, map[string]any{"tasks": out, "ledger": ledger, "admin_enabled": a.gwCfg.Admin.Enabled})
}

func (a *App) hTaskRun(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	found := false
	for _, n := range knownTasks {
		if n == name {
			found = true
		}
	}
	if !found {
		fail(w, http.StatusNotFound, "未知任务: "+name)
		return
	}
	if !a.gwCfg.Admin.Enabled {
		fail(w, http.StatusConflict, "网关 config.json 里 admin.enabled=false，任务接口未注册；请在「配置」页开启后重启网关")
		return
	}
	resp := a.gateway.RunTask(name)
	if resp.Err != nil {
		fail(w, http.StatusBadGateway, resp.Err.Error())
		return
	}
	if resp.Status >= 300 {
		fail(w, resp.Status, gatewayError(resp))
		return
	}
	var v map[string]any
	_ = json.Unmarshal(resp.Body, &v)
	ok(w, map[string]any{"result": v, "note": "任务已异步受理，可在日志页观察执行过程"})
}

// 保证 sort 被使用（账号排序）
var _ = sort.Strings
