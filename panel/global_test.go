// global_test.go global 后置流程（注册激活/地区完善/trial）的桩测。
// 钉住 scripts/global_region.py 的逆向契约：register 的 500="region required"、
// area 族双层信封（data 是 JSON 字符串）、trial 幂等码 14051、
// 「未检测到地区且未选择时绝不擅自提交地区」。
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
)

type globalStub struct {
	mu           sync.Mutex
	regionSet    bool   // 已提交地区
	regionIOS2   string // 检测端点回的地区（"" = 未设置）
	trialBody    string // trial 端点响应体（"" = 默认成功）
	registerHits int
}

func (st *globalStub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/realms/copilot/overseas/user/register", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		st.registerHits++
		set := st.regionSet
		st.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if set {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "msg": "ok"})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 500, "msg": "region required"})
	})
	mux.HandleFunc("POST /billing/area/get-country-code", func(w http.ResponseWriter, r *http.Request) {
		// data 是 JSON 字符串的双层信封（真实上游形态）。
		inner := map[string]any{"code": 0, "data": map[string]any{"list": []map[string]any{
			{"IOS2": "SG", "Code": "702", "EnName": "Singapore"},
			{"IOS2": "HK", "Code": "344", "EnName": "Hong Kong"},
		}}}
		raw, _ := json.Marshal(inner)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": string(raw)})
	})
	mux.HandleFunc("POST /billing/area/get-user-area-info", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		ios2 := st.regionIOS2
		st.mu.Unlock()
		payload := map[string]any{"data": map[string]any{"IOS2": ios2, "enName": ""}}
		raw, _ := json.Marshal(payload)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK", "data": string(raw)})
	})
	mux.HandleFunc("POST /console/login/account", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Attributes map[string][]string `json:"attributes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		st.mu.Lock()
		st.regionSet = true
		st.regionIOS2 = body.Attributes["countryName"][0]
		st.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "OK"})
	})
	mux.HandleFunc("POST /billing/ide/trial", func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		body := st.trialBody
		st.mu.Unlock()
		if body == "" {
			body = `{"code":0,"msg":"OK"}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestGlobalSignupFullFlow(t *testing.T) {
	st := &globalStub{regionIOS2: ""} // 新号：无地区 → 用下拉选择 SG
	srv := st.serve(t)
	res := completeGlobalSignup(srv.URL, "tok", "uid-1", "SG")
	if res.Register != "ok" {
		t.Fatalf("register = %q, want ok (detail=%s)", res.Register, res.Detail)
	}
	if !res.RegionSet || res.Region != "SG" {
		t.Errorf("region = %q set=%v, want SG/true", res.Region, res.RegionSet)
	}
	if res.Trial != "ok" {
		t.Errorf("trial = %q, want ok", res.Trial)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.registerHits != 2 { // 前置探测 + 提交地区后验证
		t.Errorf("register hits = %d, want 2", st.registerHits)
	}
}

func TestGlobalSignupDetectedRegionWins(t *testing.T) {
	st := &globalStub{regionIOS2: "HK"} // 上游已有地区：不得用下拉值覆盖
	srv := st.serve(t)
	res := completeGlobalSignup(srv.URL, "tok", "uid-2", "SG")
	if res.Region != "HK" || !res.RegionSet {
		t.Errorf("region = %q set=%v, want HK/true（检测到的地区优先）", res.Region, res.RegionSet)
	}
	if res.Register != "ok" || res.Trial != "ok" {
		t.Errorf("register=%s trial=%s, want ok/ok", res.Register, res.Trial)
	}
}

func TestGlobalSignupRegionRequiredNoGuess(t *testing.T) {
	st := &globalStub{regionIOS2: ""} // 无检测地区 + 无选择：必须止步，不擅改账号
	srv := st.serve(t)
	res := completeGlobalSignup(srv.URL, "tok", "uid-3", "")
	if res.Register != "region_required" {
		t.Errorf("register = %q, want region_required", res.Register)
	}
	if res.RegionSet {
		t.Error("不得在未选择地区时擅自提交")
	}
	if res.Trial != "skipped" {
		t.Errorf("trial = %q, want skipped", res.Trial)
	}
	st.mu.Lock()
	set := st.regionSet
	st.mu.Unlock()
	if set {
		t.Error("stub 收到了地区提交（应零写请求）")
	}
}

func TestGlobalSignupRegisterOkImmediate(t *testing.T) {
	st := &globalStub{regionSet: true, regionIOS2: "SG"} // 已激活老号：直接 trial
	srv := st.serve(t)
	res := completeGlobalSignup(srv.URL, "tok", "uid-4", "")
	if res.Register != "ok" || res.RegionSet {
		t.Errorf("register=%s set=%v, want ok/false（不该重复提交地区）", res.Register, res.RegionSet)
	}
	if res.Trial != "ok" {
		t.Errorf("trial = %q, want ok", res.Trial)
	}
}

func TestGlobalSignupTrialIdempotent(t *testing.T) {
	st := &globalStub{regionSet: true, regionIOS2: "SG", trialBody: `{"code":14051,"msg":"already claimed"}`}
	srv := st.serve(t)
	res := completeGlobalSignup(srv.URL, "tok", "uid-5", "")
	if res.Trial != "already" {
		t.Errorf("trial = %q, want already（14051 幂等码）", res.Trial)
	}
}

// TestGlobalSignupLive 真实上游联调入口（默认跳过）：
//
//	WB_GLOBAL_TOKEN=<该号 access_token> WB_GLOBAL_UID=<uid> [WB_GLOBAL_REGION=SG] go test -run TestGlobalSignupLive -v
//
// 不给 WB_GLOBAL_REGION 时绝不擅自提交地区——只在上游检测到地区/已激活时推进。
func TestGlobalSignupLive(t *testing.T) {
	tok := os.Getenv("WB_GLOBAL_TOKEN")
	uid := os.Getenv("WB_GLOBAL_UID")
	if tok == "" || uid == "" {
		t.Skip("需要 WB_GLOBAL_TOKEN / WB_GLOBAL_UID 环境变量")
	}
	res := completeGlobalSignup(globalSignupBase, tok, uid, os.Getenv("WB_GLOBAL_REGION"))
	b, _ := json.Marshal(res)
	t.Logf("live activation: %s", b)
}
