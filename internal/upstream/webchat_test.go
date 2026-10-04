// webchat_test.go 网页通道每日打卡的全链路桩测：console（建会话 / session /
// 状态轮询）与沙箱 ACP（SSE + JSON-RPC POST）都在一个假 RoundTripper 里扮演。
// 契约钉住点：出站头（Bearer/X-User-Id/Origin/UA）、ACP 三步顺序、
// completed 才算完成、failed/error 立即失败、CN 账号拒绝。
package upstream

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// blockingBody SSE 模拟体：读完预置事件后阻塞，Close 即释放（对应真实连接被关）。
type blockingBody struct {
	mu     sync.Mutex
	lines  []string
	closed chan struct{}
}

func newBlockingBody(lines ...string) *blockingBody {
	return &blockingBody{lines: lines, closed: make(chan struct{})}
}

func (b *blockingBody) Read(p []byte) (int, error) {
	if len(b.lines) > 0 {
		n := copy(p, b.lines[0])
		b.lines = b.lines[1:]
		return n, nil
	}
	<-b.closed
	return 0, io.EOF
}

func (b *blockingBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

const (
	sseEventLine = "data: {\"method\":\"session/update\",\"params\":{\"update\":{\"sessionUpdate\":\"agent_message_chunk\"}}}\n\n"
	acpLink      = "https://sandbox.test/acp"
)

// webchatStub 一次打卡链路的桩状态。
type webchatStub struct {
	mu       sync.Mutex
	acpPosts []string // ACP POST 收到的 method 顺序
	sessions int      // /session 查询次数
	statuses int      // 状态轮询次数
	prompted bool     // 已收到 session/prompt
	failMode string   // ""=正常；"nosandbox"=session 缺 link/token；"failed"=状态 failed
}

func (st *webchatStub) transport(t *testing.T) rtFunc {
	t.Helper()
	return func(r *http.Request) (*http.Response, error) {
		host := r.URL.Host
		switch {
		case host == "global.test" && r.Method == http.MethodPost && r.URL.Path == "/console/as/conversations/":
			if r.Header.Get("Authorization") != "Bearer at" || r.Header.Get("X-User-Id") != "g1" {
				return nil, errors.New("missing web credentials on create")
			}
			if r.Header.Get("Origin") != "https://global.test" || !strings.HasPrefix(r.Header.Get("Referer"), "https://global.test/app") {
				return nil, errors.New("missing web Origin/Referer")
			}
			body, _ := io.ReadAll(r.Body)
			for _, want := range []string{`"model":"deepseek-v4.1-flash"`, `"prompt":"Hi"`, `"conversationOrigin":"workbuddy-app"`} {
				if !strings.Contains(string(body), want) {
					return nil, errors.New("create body missing " + want)
				}
			}
			return jsonResp(200, `{"code":0,"msg":"OK","data":{"id":"conv1"}}`), nil

		case host == "global.test" && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/session"):
			st.mu.Lock()
			st.sessions++
			failMode := st.failMode
			st.mu.Unlock()
			if failMode == "nosandbox" {
				return jsonResp(200, `{"code":0,"msg":"OK","data":{"link":"","token":""}}`), nil
			}
			return jsonResp(200, `{"code":0,"msg":"OK","data":{"link":"`+acpLink+`","token":"stok","sessionId":"sess1","cwd":"/workspace"}}`), nil

		case host == "global.test" && r.Method == http.MethodGet && r.URL.Path == "/console/as/conversations/conv1":
			st.mu.Lock()
			st.statuses++
			status := "running"
			if st.failMode == "failed" || st.prompted {
				status = "completed"
				if st.failMode == "failed" {
					status = "failed"
				}
			}
			st.mu.Unlock()
			return jsonResp(200, `{"code":0,"msg":"OK","data":{"status":"`+status+`"}}`), nil

		case host == "sandbox.test" && r.Method == http.MethodGet:
			// SSE 通道：必须带事件流 Accept + 沙箱 token，返回 Acp-Connection-Id。
			if r.Header.Get("Accept") != "text/event-stream" {
				return nil, errors.New("SSE open missing event-stream Accept")
			}
			if r.Header.Get("Authorization") != "Bearer stok" {
				return nil, errors.New("SSE open missing sandbox token")
			}
			resp := jsonResp(200, "")
			resp.Header.Set("Acp-Connection-Id", "conn1")
			resp.Body = newBlockingBody(sseEventLine, sseEventLine)
			return resp, nil

		case host == "sandbox.test" && r.Method == http.MethodPost:
			if r.Header.Get("Acp-Connection-Id") != "conn1" {
				return nil, errors.New("ACP post missing connection id")
			}
			if r.Header.Get("Authorization") != "Bearer stok" {
				return nil, errors.New("ACP post missing sandbox token")
			}
			body, _ := io.ReadAll(r.Body)
			text := string(body)
			method := ""
			for _, m := range []string{"initialize", "session/load", "session/prompt"} {
				if strings.Contains(text, `"`+m+`"`) {
					method = m
				}
			}
			if method == "" {
				return nil, errors.New("ACP post unknown method: " + text)
			}
			st.mu.Lock()
			st.acpPosts = append(st.acpPosts, method)
			if method == "session/prompt" {
				st.prompted = true
			}
			st.mu.Unlock()
			return jsonResp(202, `{}`), nil
		}
		return nil, errors.New("unexpected request: " + r.Method + " " + r.URL.String())
	}
}

func webchatTestClient(t *testing.T, st *webchatStub) *Client {
	t.Helper()
	oldInterval := webPollInterval
	webPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { webPollInterval = oldInterval })
	return &Client{
		HTTP:           &http.Client{Transport: st.transport(t)},
		ChatBaseCN:     "https://chat.example",
		BillingBaseCN:  "https://billing.example",
		ChatBaseGlobal: "https://global.test",
		GlobalEnabled:  true,
	}
}

func webchatAcct() *auth.Auth {
	return &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"}
}

func TestWebDailyCheckinHappyPath(t *testing.T) {
	st := &webchatStub{}
	c := webchatTestClient(t, st)
	res, err := c.WebDailyCheckin(webchatAcct())
	if err != nil {
		t.Fatalf("webchat: %v", err)
	}
	if res.Conversation != "conv1" || res.Status != "completed" {
		t.Errorf("res = %+v, want conv1/completed", res)
	}
	if res.Chunks != 2 || res.Updates != 2 {
		t.Errorf("updates=%d chunks=%d, want 2/2（SSE 事件计数）", res.Updates, res.Chunks)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if want := []string{"initialize", "session/load", "session/prompt"}; strings.Join(st.acpPosts, ",") != strings.Join(want, ",") {
		t.Errorf("acp posts = %v, want %v（顺序钉死）", st.acpPosts, want)
	}
	if st.statuses == 0 {
		t.Error("状态接口一次都没轮询")
	}
}

func TestWebDailyCheckinRejectsCN(t *testing.T) {
	c := webchatTestClient(t, &webchatStub{})
	cn := &auth.Auth{AccessToken: "at", UID: "c1"}
	if _, err := c.WebDailyCheckin(cn); err == nil {
		t.Fatal("CN 账号应被拒绝（globalOn 双闸）")
	}
	// 逃生门：GlobalEnabled=false 时 global 账号同样拒绝。
	c2 := webchatTestClient(t, &webchatStub{})
	c2.GlobalEnabled = false
	if _, err := c2.WebDailyCheckin(webchatAcct()); err == nil {
		t.Fatal("global.enabled=false 时应拒绝（D5 逃生门）")
	}
}

func TestWebDailyCheckinSandboxNotReady(t *testing.T) {
	st := &webchatStub{failMode: "nosandbox"}
	c := webchatTestClient(t, st)
	_, err := c.WebDailyCheckin(webchatAcct())
	if err == nil || !strings.Contains(err.Error(), "conv1") {
		t.Fatalf("want error carrying conversation id, got %v", err)
	}
}

func TestWebDailyCheckinFailedStatus(t *testing.T) {
	st := &webchatStub{failMode: "failed"}
	c := webchatTestClient(t, st)
	_, err := c.WebDailyCheckin(webchatAcct())
	if err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("want status=failed error, got %v", err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.acpPosts) != 3 {
		t.Errorf("acp posts = %d, want 3（失败前三步应已发完）", len(st.acpPosts))
	}
}
