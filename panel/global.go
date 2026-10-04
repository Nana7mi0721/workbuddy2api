// global.go global 账号登录后置流程：注册激活 → 注册地区完善 → trial 领取。
//
// 背景：新 global 账号直接对话会被上游 14017（"The trial version is not yet
// activated"）拒绝；login.sh 在 global 登录后会自动补这套流程，但面板的添加
// 账号此前只跑 login url/poll，跳过了后续步骤——这正是「面板加的 global 号
// 不能用」的根因。本文件把 scripts/global_region.py 的逆向契约移植为 Go：
//
//	GET  {base}/auth/realms/copilot/overseas/user/register?userId=<uid>
//	     激活/查询（HTTP 200 + code=200 成功；code=500 或 "region required" → 需补地区）
//	POST {base}/billing/area/get-country-code      {filterForbidden:1}
//	     地区列表（data 是 JSON 字符串，内层 {"code":0,"data":{"list":[{EnName,Name,IOS2,IOS3,Code}]}}）
//	POST {base}/billing/area/get-user-area-info    {action:"getUserAreaInfo"}
//	     检测当前地区（data 同为 JSON 字符串，内层 {data:{IOS2,enName}}）
//	POST {base}/console/login/account              {"attributes":{countryCode:[Code],countryFullName:[EnName],countryName:[IOS2]}}
//	     提交地区（幂等；Bearer 必需，成功 code=0）
//	POST {base}/billing/ide/trial                  {}
//	     一次性试用包（幂等码 14051，200 或 4xx 两种形态都算「已领」）
//
// 出站头与 web 端对齐：Bearer accessToken + 浏览器 UA + Origin/Referer（register
// 额外带 X-User-Id）。任何一步失败都不回滚已落盘的账号文件，结果逐项回报给
// 前端展示（activation 字段），可用 ./trial.sh 事后重试。
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

// globalSignupBase 默认上游 base（与网关 defaultGlobalBase 同值；config.json 的
// global.chat_base 覆盖优先，见 App.globalBase）。
const globalSignupBase = "https://www.workbuddy.ai"

// globalSignupUA 网页端出站 UA（与 upstream.webUserAgent 同形）。
const globalSignupUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36 Edg/140.0.0.0"

// regionWhitelist 国际版 web 白名单（RegisterRegion 页只展示这七个）。
var regionWhitelist = map[string]bool{"HK": true, "MO": true, "SG": true, "TH": true, "PH": true, "MY": true, "ID": true}

// ActivationResult 一次 global 后置流程的逐项结果（前端展示与日志用）。
type ActivationResult struct {
	Register  string `json:"register"`         // ok | region_required | fail: …
	Region    string `json:"region,omitempty"` // 最终生效的 IOS2（提交的或检测到的）
	RegionSet bool   `json:"region_set"`       // 是否实际提交了地区（幂等写）
	Trial     string `json:"trial"`            // ok | already | skipped | fail: …
	Detail    string `json:"detail,omitempty"` // 供排错的一句话摘要
}

// globalSignupHTTP 独立客户端：面板自己的出站（不经网关），单步 20s 封顶。
var globalSignupHTTP = &http.Client{Timeout: 20 * time.Second}

// globalBase 生效的 global 上游 base：网关 config.json 的 global.chat_base 覆盖，
// 读取失败回落内置默认（面板不该因为读不了网关配置而拒绝激活）。
func (a *App) globalBase() string {
	if gc, err := a.cfg.readGatewayConfig(); err == nil && gc.Global.ChatBase != "" {
		return strings.TrimRight(gc.Global.ChatBase, "/")
	}
	return globalSignupBase
}

// globalReq 发一次带 Bearer 的 JSON 请求并解外层 {code,msg,data} 信封。
// data 可能是 JSON 字符串（area 族端点），由调用方二次解。非 2xx 也返回 body
// 交给调用方按业务码判断（register 的 500、trial 的 4xx 都带合法 JSON）。
func globalReq(base, method, path, token string, body any, extra map[string]string) (int, map[string]any, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, base+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", base)
	req.Header.Set("Referer", base+"/")
	req.Header.Set("User-Agent", globalSignupUA)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := globalSignupHTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		m = map[string]any{"_raw": string(raw)}
	}
	return resp.StatusCode, m, nil
}

// unwrapArea 解 area 族端点：外层 {code,msg,data} 的 data 是 JSON 字符串，
// 解出内层信封（{"code":0,"data":{...}} 或 {"data":{...}}）；解析失败返回 nil。
func unwrapArea(m map[string]any) map[string]any {
	d, _ := m["data"].(string)
	if d == "" {
		return nil
	}
	var inner map[string]any
	if json.Unmarshal([]byte(d), &inner) != nil {
		return nil
	}
	return inner
}

// areaPayload 返回 area 内层载荷（list / IOS2 所在层）；无嵌套 data 时回落内层自身。
func areaPayload(m map[string]any) map[string]any {
	inner := unwrapArea(m)
	if inner == nil {
		return nil
	}
	if dm, ok := inner["data"].(map[string]any); ok {
		return dm
	}
	return inner
}

// registerActivate GET register 激活/查询。返回 (ok, needsRegion, msg)。
func registerActivate(base, token, uid string) (bool, bool, string) {
	status, m, err := globalReq(base, http.MethodGet,
		"/auth/realms/copilot/overseas/user/register?userId="+uid, token, nil,
		map[string]string{"X-User-Id": uid})
	if err != nil {
		return false, false, err.Error()
	}
	// 上游对 4xx/5xx 也会给 JSON body（code 字段承载业务码），以 body 为准。
	code, _ := m["code"].(float64)
	msg, _ := m["msg"].(string)
	if status >= 400 && code == 0 {
		return false, false, fmt.Sprintf("http %d", status)
	}
	switch {
	case code == 200:
		return true, false, "register success"
	case code == 500 || strings.Contains(strings.ToLower(msg), "region required"):
		return false, true, firstNonEmpty(msg, fmt.Sprintf("code=%.0f", code))
	default:
		return false, false, firstNonEmpty(msg, fmt.Sprintf("code=%.0f", code))
	}
}

// regionCountry 从地区列表里挑出 IOS2 对应的国家项（含 Code/EnName）。
func regionCountry(m map[string]any, ios2 string) map[string]any {
	list, _ := areaPayload(m)["list"].([]any)
	for _, it := range list {
		c, _ := it.(map[string]any)
		if c != nil && c["IOS2"] == ios2 {
			return c
		}
	}
	return nil
}

// detectRegion 检测账号当前注册地区，返回 IOS2（空 = 未设置）。
func detectRegion(base, token string) string {
	_, m, err := globalReq(base, http.MethodPost, "/billing/area/get-user-area-info", token,
		map[string]any{"action": "getUserAreaInfo"}, nil)
	if err != nil {
		return ""
	}
	if d := areaPayload(m); d != nil {
		if s, _ := d["IOS2"].(string); regionWhitelist[s] {
			return s
		}
	}
	return ""
}

// submitRegion 提交注册地区（幂等写）。
func submitRegion(base, token string, country map[string]any) (bool, string) {
	attrs := map[string]any{
		"countryCode":     []string{fmt.Sprintf("%v", country["Code"])},
		"countryFullName": []string{fmt.Sprintf("%v", country["EnName"])},
		"countryName":     []string{fmt.Sprintf("%v", country["IOS2"])},
	}
	_, m, err := globalReq(base, http.MethodPost, "/console/login/account", token,
		map[string]any{"attributes": attrs}, nil)
	if err != nil {
		return false, err.Error()
	}
	if code, _ := m["code"].(float64); code == 0 {
		return true, "ok"
	}
	msg, _ := m["msg"].(string)
	return false, firstNonEmpty(msg, "submit region failed")
}

// claimTrial 领取一次性试用包。幂等码 14051（200 body 或 4xx body 都算已领）。
func claimTrial(base, token string) (ok, already bool, msg string) {
	status, m, err := globalReq(base, http.MethodPost, "/billing/ide/trial", token,
		map[string]any{}, nil)
	if err != nil {
		return false, false, err.Error()
	}
	raw, _ := json.Marshal(m)
	if strings.Contains(string(raw), "14051") {
		return true, true, "已领取"
	}
	if code, _ := m["code"].(float64); code == 0 && status < 400 {
		return true, false, "ok"
	}
	msg, _ = m["msg"].(string)
	return false, false, firstNonEmpty(msg, fmt.Sprintf("http %d", status))
}

// completeGlobalSignup global 账号登录后的完整后置流程。preferRegion 为前端
// 下拉选择的 IOS2（仅当上游检测不到地区时使用）；空 = 不猜地区，只回报
// region_required（账号属性不该由面板擅自决定）。
func completeGlobalSignup(base, token, uid, preferRegion string) *ActivationResult {
	res := &ActivationResult{Register: "fail: register 调用失败", Trial: "skipped"}
	ok, needs, msg := registerActivate(base, token, uid)
	switch {
	case ok:
		res.Register = "ok"
	case needs:
		res.Register = "region_required"
	default:
		res.Register = "fail: " + msg
		res.Detail = "register 失败：" + msg + "（trial 未尝试）"
		return res
	}
	if needs {
		// 补地区：优先上游检测（账号 IP/资料已有地区就别覆盖），其次用户下拉选择。
		ios2 := detectRegion(base, token)
		source := "detected"
		if ios2 == "" {
			if !regionWhitelist[preferRegion] {
				res.Detail = "上游要求完善注册地区：未检测到已有地区，前端也未选择——请重新添加并选择地区（HK/MO/SG/TH/PH/MY/ID）"
				return res
			}
			ios2, source = preferRegion, "selected"
		}
		_, countries, err := func() (int, map[string]any, error) {
			return globalReq(base, http.MethodPost, "/billing/area/get-country-code", token,
				map[string]any{"filterForbidden": 1}, nil)
		}()
		if err != nil {
			res.Detail = "拉取地区列表失败：" + err.Error()
			return res
		}
		country := regionCountry(countries, ios2)
		if country == nil {
			res.Detail = "地区列表中找不到 " + ios2
			return res
		}
		if sok, smsg := submitRegion(base, token, country); !sok {
			res.Region = ios2
			res.Detail = "提交地区失败：" + smsg
			return res
		} else {
			res.RegionSet = true
		}
		res.Region = ios2
		// 提交后重新 register 验证（与 login.sh 同序）。
		ok2, _, msg2 := registerActivate(base, token, uid)
		if !ok2 {
			res.Register = "fail: " + msg2
			res.Detail = fmt.Sprintf("地区 %s（%s）已提交但 register 仍失败：%s", ios2, source, msg2)
			return res
		}
		res.Register = "ok"
		res.Detail = fmt.Sprintf("地区 %s（%s）已提交，register 成功", ios2, source)
	}
	if ok, already, tmsg := claimTrial(base, token); ok {
		if already {
			res.Trial = "already"
		} else {
			res.Trial = "ok"
		}
	} else {
		res.Trial = "fail: " + tmsg
	}
	if res.Detail == "" {
		res.Detail = "register ok, trial=" + res.Trial
	}
	return res
}
