package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// 添加账号：驱动 login.exe 的两个一次性子命令（url → 用户浏览器授权 → poll）。
// 两者都是一次性进程，面板不需要长时间持有子进程。
type LoginFlow struct {
	mu        sync.Mutex
	Realm     string    `json:"realm"`
	URL       string    `json:"url"`
	StartedAt time.Time `json:"started_at"`
	Output    string    `json:"output"`
}

var loginFlow = &LoginFlow{}

var linkRe = regexp.MustCompile(`https?://[^\s"'<>）)]+`)

// loginStatePath 面板托管的登录 state 文件路径（按 realm 分文件，落在网关 data 目录）。
//
// 为什么不用 login.exe 默认的 os.TempDir() 路径：temp 里的残留文件可能带只读属性
// 或提权终端留下的 ACL（实测 "write state: open …Access is denied"，授权链接拿
// 不到、前端无法添加账号），也可能撞上安全软件对 temp 目录的行为规则；data 目录
// 与网关 state.json 同目录，权限确定可控。按 realm 分文件，cn/global 两次流程
// 互不覆盖。目录不存在时顺手创建（首次部署 data/ 可能还没被网关建出来）。
func loginStatePath(cfg *PanelConfig, realm string) string {
	dir := filepath.Join(cfg.BaseDir(), "data")
	_ = os.MkdirAll(dir, 0o755)
	return filepath.Join(dir, "login-state-"+realm+".json")
}

func normalizeRealm(r string) string {
	switch strings.ToLower(strings.TrimSpace(r)) {
	case "", "1", "cn", "china", "tencent":
		return "cn"
	case "2", "global", "intl", "international", "ai":
		return "global"
	}
	return strings.ToLower(strings.TrimSpace(r))
}

// StartLogin 执行 login.exe url，解析出授权链接。
func StartLogin(cfg *PanelConfig, realm string) (*LoginFlow, error) {
	loginFlow.mu.Lock()
	defer loginFlow.mu.Unlock()

	exe := cfg.LoginExe()
	if _, err := statFileStrict(exe); err != nil {
		return nil, fmt.Errorf("找不到 login 程序：%s", exe)
	}
	realm = normalizeRealm(realm)
	statePath := loginStatePath(cfg, realm)
	args := []string{"--realm=" + realm, "--state=" + statePath, "url"}
	out, errOut, err := runCaptured(exe, args, cfg.BaseDir(), 60*time.Second)
	combined := strings.TrimSpace(out + "\n" + errOut)
	if err != nil && !linkRe.MatchString(out) {
		return nil, fmt.Errorf("login url 失败：%v；输出：%s", err, truncate(combined, 400))
	}
	url := linkRe.FindString(out)
	if url == "" {
		url = linkRe.FindString(combined)
	}
	if url == "" {
		return nil, fmt.Errorf("未从 login 输出里解析到授权链接，原始输出：%s", truncate(combined, 400))
	}
	loginFlow.Realm = realm
	loginFlow.URL = url
	loginFlow.StartedAt = time.Now()
	loginFlow.Output = truncate(combined, 2000)
	return loginFlow, nil
}

// PollLogin 执行 login.exe poll，成功后落盘账号文件。
func PollLogin(cfg *PanelConfig, realm string, save bool) (*LoginOutput, string, error) {
	loginFlow.mu.Lock()
	defer loginFlow.mu.Unlock()

	exe := cfg.LoginExe()
	realm = normalizeRealm(firstNonEmpty(realm, loginFlow.Realm))
	args := []string{"--realm=" + realm, "--state=" + loginStatePath(cfg, realm), "poll"}
	out, errOut, err := runCaptured(exe, args, cfg.BaseDir(), 60*time.Second)
	combined := strings.TrimSpace(out + "\n" + errOut)
	lo := extractLoginJSON(out)
	if lo == nil {
		lo = extractLoginJSON(combined)
	}
	if lo == nil {
		msg := truncate(combined, 400)
		if err != nil {
			return nil, "", fmt.Errorf("尚未完成授权或轮询失败：%v；输出：%s", err, msg)
		}
		return nil, "", fmt.Errorf("未解析到账号信息（可能还没在浏览器里完成授权）：%s", msg)
	}
	if lo.Realm == "" {
		lo.Realm = realm
	}
	var file string
	if save {
		f, err := WriteAccountFile(cfg.AuthDir(), lo, realm)
		if err != nil {
			return lo, "", err
		}
		file = f
		loginFlow.URL = ""
	}
	return lo, file, nil
}

// extractLoginJSON 从命令输出里抠出第一个完整 JSON 对象（login.exe 可能先打日志再打 JSON）。
func extractLoginJSON(s string) *LoginOutput {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return nil
	}
	cand := s[start : end+1]
	var lo LoginOutput
	if err := json.Unmarshal([]byte(cand), &lo); err != nil {
		return nil
	}
	if lo.UID == "" && lo.AccessToken == "" {
		return nil
	}
	return &lo
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}

func statFileStrict(path string) (int64, error) {
	fi := statFile(path)
	if !fi.Exists {
		return 0, fmt.Errorf("文件不存在")
	}
	return fi.Size, nil
}
