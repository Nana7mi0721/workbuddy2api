// webchat_test.go 国际版每日打卡任务的门控与幂等测试。
//
// 钉住四件事：
//  1. CN 账号零上游调用（webchat 是国际版专属）；
//  2. global 账号走完整链路（建会话 → 沙箱 → ACP → completed）并落当日闸门；
//  3. 当日第二次运行被闸门拦住（already），不再打上游——打卡真实烧积分；
//  4. 闸门**跨进程**有效：新 Scheduler 实例从落盘文件恢复状态后同样拦住
//     （WebchatStateFile 的存在意义，与 rewardClaimed 内存级刻意不同）。
package scheduler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// webchatUpstream 扮演国际版网页通道：console 三件套 + 沙箱 ACP。
// createCalls 统计建会话次数（闸门是否拦住的唯一可信信号）。
func webchatUpstream(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var createCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /console/as/conversations/", func(w http.ResponseWriter, r *http.Request) {
		createCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{"id": "conv1"}})
	})
	mux.HandleFunc("GET /console/as/conversations/conv1/session", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK",
			"data": map[string]any{"link": "http://" + r.Host + "/acp", "token": "stok", "sessionId": "sess1"}})
	})
	mux.HandleFunc("GET /console/as/conversations/conv1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": map[string]any{"status": "completed"}})
	})
	// 沙箱 ACP：SSE 通道 + 三个 JSON-RPC POST（本测试只关心链路走通与计数）。
	mux.HandleFunc("/acp", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Acp-Connection-Id", "conn1")
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"method\":\"session/update\"}\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done() // 客户端关流（打卡收尾）才返回
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &createCalls
}

// fastWebchat 打包测试加速项：账号间限速与轮询间隔归零。
func fastWebchat(t *testing.T) {
	t.Helper()
	old := webchatAccountDelay
	webchatAccountDelay = 0
	t.Cleanup(func() { webchatAccountDelay = old })
}

func webchatTestScheduler(t *testing.T, stateFile string) (*Scheduler, *atomic.Int32) {
	t.Helper()
	fastWebchat(t)
	srv, createCalls := webchatUpstream(t)
	up := &upstream.Client{
		HTTP:                srv.Client(),
		ChatBaseCN:          srv.URL,
		BillingBaseCN:       srv.URL,
		ChatBaseGlobal:      srv.URL,
		BillingBaseGlobal:   srv.URL,
		GlobalEnabled:       true,
		WebChatPollInterval: 5 * time.Millisecond,
		WebChatTurnTimeout:  10 * time.Second,
	}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "c1", AccessToken: "at-cn", RefreshToken: "rt", ExpiresAt: 9999999999}) // CN
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at-g", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"}) // global
	return New(Config{Pool: p, Upstream: up, WebchatStateFile: stateFile}), createCalls
}

func TestWebchatGatingAndPersistence(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "webchat-state.json")
	s, createCalls := webchatTestScheduler(t, stateFile)

	s.RunWebchatNow()
	if n := createCalls.Load(); n != 1 {
		t.Fatalf("create calls=%d want 1（仅 global 账号；CN 零调用）", n)
	}
	raw, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatalf("state file: %v", err)
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("state parse: %v", err)
	}
	if m["g1"] != travelDay(time.Now()) {
		t.Errorf("state[g1]=%q want 当日 %s", m["g1"], travelDay(time.Now()))
	}
	if _, ok := m["c1"]; ok {
		t.Error("CN 账号不应进打卡台账")
	}

	// 同一实例第二次跑：闸门拦住（already），不再打上游。
	s.RunWebchatNow()
	if n := createCalls.Load(); n != 1 {
		t.Errorf("second run create calls=%d want 1（当日幂等）", n)
	}

	// 跨进程：新实例从落盘文件恢复 → 同样拦住（这是 WebchatStateFile 的存在意义）。
	s2, calls2 := webchatTestScheduler(t, stateFile)
	s2.RunWebchatNow()
	if n := calls2.Load(); n != 0 {
		t.Errorf("fresh instance create calls=%d want 0（重启不失忆，台账落盘生效）", n)
	}
}

func TestWebchatDisabledNoUpstream(t *testing.T) {
	fastWebchat(t)
	srv, createCalls := webchatUpstream(t)
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL,
		ChatBaseGlobal: srv.URL, GlobalEnabled: true, WebChatPollInterval: 5 * time.Millisecond}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	s := New(Config{Pool: p, Upstream: up, WebchatDisabled: true})
	// 禁用闸在 nextWake 排程层（与六类任务一致，dispatch 本身不复查）：
	// 唤醒点集合里不得再出现 webchat。
	_, kinds := s.nextWake(time.Date(2026, 9, 11, 8, 0, 0, 0, time.Local))
	for _, k := range kinds {
		if k == taskWebchat {
			t.Error("WebchatDisabled=true 时 nextWake 仍排了 webchat")
		}
	}
	// 对照：不禁用时同一时点应排上 webchat（默认 [9,21]）。
	s2 := New(Config{Pool: p, Upstream: up})
	_, kinds2 := s2.nextWake(time.Date(2026, 9, 11, 8, 0, 0, 0, time.Local))
	found := false
	for _, k := range kinds2 {
		if k == taskWebchat {
			found = true
		}
	}
	if !found {
		t.Errorf("webchat enabled 时 nextWake 未排上：kinds=%v", kinds2)
	}
	if n := createCalls.Load(); n != 0 {
		t.Errorf("nextWake 层不应触发上游调用，create calls=%d", n)
	}
}

func TestWebchatStateFileCorruptStartsEmpty(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "webchat-state.json")
	if err := os.WriteFile(stateFile, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := loadWebchatStateFile(stateFile)
	if len(m) != 0 {
		t.Errorf("corrupt state should start empty, got %v", m)
	}
	if _, createCalls := webchatTestScheduler(t, stateFile); createCalls == nil {
		t.Fatal("scheduler build failed")
	}
	if !strings.Contains(stateFile, "webchat-state.json") {
		t.Error("sanity")
	}
}
