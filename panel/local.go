package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------- 账号文件（auths/ 与 auths-paused/） ----------

// AccountFile 是 auths\workbuddy-<uid>.json 的对外视图（不含任何 token）。
type AccountFile struct {
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	EnterpriseID string `json:"enterprise_id"`
	Realm        string `json:"realm"`
	Domain       string `json:"domain"`
	ExpiresAt    int64  `json:"expires_at"`
	File         string `json:"file"`
	Dir          string `json:"dir"` // active | paused
	Size         int64  `json:"size"`
	ModTime      int64  `json:"mod_time"`
	Broken       string `json:"broken,omitempty"` // 解析失败原因
}

var uidFromName = regexp.MustCompile(`([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`)

func scanAuthDir(dir, kind string) []AccountFile {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]AccountFile, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		// 只认 workbuddy-*.json 形态，避免把别的文件当账号。
		if !strings.HasPrefix(strings.ToLower(e.Name()), "workbuddy-") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		af := AccountFile{File: e.Name(), Dir: kind}
		if st, err := e.Info(); err == nil {
			af.Size = st.Size()
			af.ModTime = st.ModTime().Unix()
		}
		raw, err := os.ReadFile(full)
		if err != nil {
			af.Broken = err.Error()
			out = append(out, af)
			continue
		}
		var doc struct {
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
			Auth struct {
				Realm     string `json:"realm"`
				Domain    string `json:"domain"`
				ExpiresAt int64  `json:"expiresAt"`
				Access    string `json:"accessToken"`
			} `json:"auth"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			af.Broken = "JSON 解析失败: " + err.Error()
		} else {
			af.UID = doc.Account.UID
			af.Nickname = doc.Account.Nickname
			af.EnterpriseID = doc.Account.EnterpriseID
			af.Realm = doc.Auth.Realm
			af.Domain = doc.Auth.Domain
			af.ExpiresAt = doc.Auth.ExpiresAt
			if doc.Auth.Access == "" {
				af.Broken = "缺少 auth.accessToken"
			}
		}
		if af.UID == "" {
			if m := uidFromName.FindStringSubmatch(e.Name()); m != nil {
				af.UID = m[1]
			}
		}
		out = append(out, af)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

// LoginOutput 是 login.exe poll 打印的 JSON。
type LoginOutput struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	EnterpriseID string `json:"enterprise_id"`
	Domain       string `json:"domain"`
	Realm        string `json:"realm"`
	DeviceToken  string `json:"device_token"`
}

// WriteAccountFile 按 internal/auth 期望的格式落盘账号文件。
func WriteAccountFile(dir string, lo *LoginOutput, realm string) (string, error) {
	if lo.UID == "" {
		return "", fmt.Errorf("登录返回缺少 uid")
	}
	if lo.AccessToken == "" {
		return "", fmt.Errorf("登录返回缺少 access_token")
	}
	if realm == "" {
		realm = lo.Realm
	}
	exp := time.Now().Add(time.Duration(lo.ExpiresIn) * time.Second).Unix()
	doc := map[string]any{
		"account": map[string]any{
			"uid":          lo.UID,
			"enterpriseId": lo.EnterpriseID,
			"nickname":     lo.Nickname,
		},
		"auth": map[string]any{
			"accessToken":  lo.AccessToken,
			"refreshToken": lo.RefreshToken,
			"expiresAt":    exp,
			"domain":       lo.Domain,
			"realm":        realm,
		},
	}
	if lo.DeviceToken != "" {
		doc["device_token"] = lo.DeviceToken
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := "workbuddy-" + lo.UID + ".json"
	full := filepath.Join(dir, name)
	if err := os.WriteFile(full, append(b, '\n'), 0o600); err != nil {
		return "", err
	}
	return full, nil
}

// MoveAccount 在 auths/ 与 auths-paused/ 之间移动账号文件（uid 或文件名均可定位）。
func MoveAccount(fromDir, toDir, uid string) (string, error) {
	path, err := findAccountFile(fromDir, uid)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(toDir, 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(toDir, filepath.Base(path))
	if _, err := os.Stat(dst); err == nil {
		return "", fmt.Errorf("目标已存在同名文件：%s", filepath.Base(path))
	}
	if err := os.Rename(path, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func findAccountFile(dir, uid string) (string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("目录不可读 %s: %w", dir, err)
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasPrefix(strings.ToLower(n), "workbuddy-") || !strings.HasSuffix(strings.ToLower(n), ".json") {
			continue
		}
		if n == uid || strings.Contains(n, uid) {
			return filepath.Join(dir, n), nil
		}
	}
	return "", fmt.Errorf("未找到账号 %s（目录 %s）", uid, dir)
}

func DeleteAccountFile(dir, uid string) (string, error) {
	path, err := findAccountFile(dir, uid)
	if err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	return path, nil
}

// ---------- data/state.json ----------

type StateAccount struct {
	Credits        json.Number `json:"credits"`
	Disabled       bool        `json:"disabled"`
	Reason         string      `json:"reason"`
	ManualDisabled bool        `json:"manual_disabled"`
	ManualReason   string      `json:"manual_reason"`
	// Until/CoolKind：state.json 里 cool_kind 是**数字枚举**（pool.CoolKind：
	// 0=hard_credit 1=soft_rate），而 /status 里同名字段是字符串——两者不同构，
	// 这里必须按数字读，否则整个 state.json 解析失败（教训）。
	Until            string          `json:"until"`
	CoolKind         json.Number     `json:"cool_kind"`
	SuccessCount     int64           `json:"success_count"`
	ErrTotal         int64           `json:"err_total"`
	ErrCount         int64           `json:"err_count"`
	LastSuccess      string          `json:"last_success"`
	LastErr          string          `json:"last_err"`
	SoftStreak       int64           `json:"soft_streak"`
	SessionDeadFails int64           `json:"session_dead_fails"`
	ConsecutiveFails int64           `json:"consecutive_fails"`
	DegradeUntil     string          `json:"degrade_until"`
	BreakerUntil     string          `json:"breaker_until"`
	RetryCount       int64           `json:"retry_count"`
	CreditsExpiring  int64           `json:"credits_expiring"`
	ModelCooldowns   json.RawMessage `json:"model_cooldowns"`
	ModelCosts       json.RawMessage `json:"model_costs"`
}

// CoolKindText 把 state.json 的数字枚举翻成与 /status 一致的可读文案。
func (s StateAccount) CoolKindText() string {
	switch s.CoolKind.String() {
	case "0":
		return "hard_credit"
	case "1":
		return "soft_rate"
	}
	return ""
}

type StateDoc struct {
	Updated  string                  `json:"updated"`
	Accounts map[string]StateAccount `json:"accounts"`
	Raw      json.RawMessage         `json:"-"`
}

func readState(path string) (*StateDoc, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s StateDoc
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("state.json 解析失败: %w", err)
	}
	s.Raw = raw
	return &s, nil
}

// ---------- config.json ----------

type ConfigView struct {
	Path    string       `json:"path"`
	Size    int64        `json:"size"`
	ModTime int64        `json:"mod_time"`
	Text    string       `json:"text"`
	Valid   bool         `json:"valid"`
	Error   string       `json:"error,omitempty"`
	Backups []BackupInfo `json:"backups"`
}

type BackupInfo struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
}

func readConfigView(path string) (*ConfigView, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	st, _ := os.Stat(path)
	v := &ConfigView{Path: path, Text: string(raw)}
	if st != nil {
		v.Size = st.Size()
		v.ModTime = st.ModTime().Unix()
	}
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		v.Error = err.Error()
	} else {
		v.Valid = true
	}
	// 备份列表（config.json.bak-*）
	if ents, err := os.ReadDir(filepath.Dir(path)); err == nil {
		base := filepath.Base(path)
		for _, e := range ents {
			if e.IsDir() || !strings.HasPrefix(e.Name(), base+".bak-") {
				continue
			}
			b := BackupInfo{Name: e.Name()}
			if st, err := e.Info(); err == nil {
				b.Size = st.Size()
				b.ModTime = st.ModTime().Unix()
			}
			v.Backups = append(v.Backups, b)
		}
		sort.Slice(v.Backups, func(i, j int) bool { return v.Backups[i].ModTime > v.Backups[j].ModTime })
	}
	return v, nil
}

// writeConfigFile 校验 JSON → 备份原文件 → 原子写入。
func writeConfigFile(path, text string) (backup string, err error) {
	var probe map[string]any
	if err := json.Unmarshal([]byte(text), &probe); err != nil {
		return "", fmt.Errorf("不是合法 JSON：%w", err)
	}
	old, err := os.ReadFile(path)
	if err == nil {
		backup = fmt.Sprintf("%s.bak-%s", path, time.Now().Format("20060102-150405"))
		if err := os.WriteFile(backup, old, 0o644); err != nil {
			return "", fmt.Errorf("写备份失败：%w", err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return backup, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return backup, err
	}
	return backup, nil
}

// ---------- 日志解析 ----------

type LogEntry struct {
	Line      int     `json:"line"`
	Kind      string  `json:"kind"` // chat | app | banner
	Time      string  `json:"time"`
	Level     string  `json:"level"` // INFO/WARN/ERROR（app 行）
	Text      string  `json:"text"`
	Model     string  `json:"model,omitempty"`
	Mode      string  `json:"mode,omitempty"`
	Status    int     `json:"status,omitempty"`
	User      string  `json:"user,omitempty"`
	UID8      string  `json:"uid8,omitempty"`
	TTFBMS    int     `json:"ttfb_ms,omitempty"`
	Tokens    int     `json:"tokens,omitempty"`
	TokPerSec float64 `json:"tok_per_sec,omitempty"`
	TotalS    float64 `json:"total_s,omitempty"`
	ReqID     int     `json:"req_id,omitempty"`
	Raw       string  `json:"raw"`
}

var chatRowRe = regexp.MustCompile(`^\|\s*#(\d+)\s*\|\s*([0-9:]+)\s*\|\s*([^|]*?)\s*\|\s*(\w+)\s*\|\s*(\d{3})\s*\|\s*([^(|]*)\((\w+)\)\s*\|(.*)$`)
var appLineRe = regexp.MustCompile(`^(\d{4}/\d{1,2}/\d{1,2} \d{1,2}:\d{2}:\d{2})\s*(.*)$`)
var kvRe = regexp.MustCompile(`([A-Za-z_/]+)=([0-9.]+)`)

// 网关把生成速度写作「155.2tok/s」——值在前、没有等号，kvRe 抓不到，单独匹配。
var tokPerSecRe = regexp.MustCompile(`([0-9.]+)\s*tok/s`)

func parseLogLine(lineNo int, raw string) LogEntry {
	s := strings.TrimRight(raw, "\r\n")
	e := LogEntry{Line: lineNo, Raw: s, Kind: "app", Level: "INFO"}
	if m := chatRowRe.FindStringSubmatch(s); m != nil {
		e.Kind = "chat"
		e.ReqID, _ = strconv.Atoi(m[1])
		e.Time = m[2]
		e.Model = strings.TrimSpace(m[3])
		e.Mode = m[4]
		e.Status, _ = strconv.Atoi(m[5])
		e.User = strings.TrimSpace(m[6])
		e.UID8 = m[7]
		tail := m[8]
		for _, kv := range kvRe.FindAllStringSubmatch(tail, -1) {
			v, _ := strconv.ParseFloat(kv[2], 64)
			switch kv[1] {
			case "TTFB":
				e.TTFBMS = int(v)
			case "tok":
				e.Tokens = int(v)
			case "tok/s":
				e.TokPerSec = v
			case "total":
				e.TotalS = v
			}
		}
		if tps := tokPerSecRe.FindStringSubmatch(tail); tps != nil {
			e.TokPerSec, _ = strconv.ParseFloat(tps[1], 64)
		}
		e.Text = strings.TrimSpace(s)
		if e.Status >= 400 {
			e.Level = "ERROR"
		}
		return e
	}
	if strings.HasPrefix(s, "====") || strings.HasPrefix(s, "----") {
		e.Kind = "banner"
		e.Text = strings.TrimSpace(s)
		return e
	}
	if m := appLineRe.FindStringSubmatch(s); m != nil {
		e.Time = m[1]
		e.Text = strings.TrimSpace(m[2])
	} else {
		e.Text = strings.TrimSpace(s)
	}
	up := strings.ToUpper(e.Text)
	switch {
	case strings.Contains(up, "ERROR"), strings.Contains(e.Text, "失败"), strings.Contains(up, "panic"), strings.Contains(up, "FAIL"):
		e.Level = "ERROR"
	case strings.Contains(up, "WARN"), strings.Contains(e.Text, "跳过"):
		e.Level = "WARN"
	}
	return e
}

// tailLines 读文件末尾最多 maxBytes，返回最后 tailN 行（带全局行号估算）。
func tailLines(path string, tailN int, maxBytes int64) ([]LogEntry, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := st.Size()
	start := int64(0)
	if size > maxBytes {
		start = size - maxBytes
	}
	if _, err := f.Seek(start, 0); err != nil {
		return nil, 0, err
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	// 估算起始行号：按平均行长推算（仅用于展示，不保证精确）。
	// 全量行号在需要时由 countLines 提供。
	total := 0
	if start == 0 {
		total = len(lines)
	} else {
		avg := 120.0
		if len(lines) > 0 {
			avg = float64(size-start) / float64(len(lines))
		}
		total = int(float64(size)/avg) + 1
	}
	if len(lines) > tailN {
		lines = lines[len(lines)-tailN:]
	}
	first := total - len(lines) + 1
	if first < 1 {
		first = 1
	}
	out := make([]LogEntry, 0, len(lines))
	for i, l := range lines {
		out = append(out, parseLogLine(first+i, l))
	}
	return out, total, nil
}

func countLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	n := 0
	for sc.Scan() {
		n++
	}
	return n
}

// ---------- admin 审计日志（JSONL） ----------

func tailAudit(path string, limit int) ([]json.RawMessage, error) {
	if path == "" {
		return nil, fmt.Errorf("未配置审计文件")
	}
	lines, _, err := tailLines(path, limit, 2<<20)
	if err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(lines))
	for _, l := range lines {
		t := strings.TrimSpace(l.Raw)
		if t == "" {
			continue
		}
		var probe any
		if json.Unmarshal([]byte(t), &probe) == nil {
			out = append(out, json.RawMessage(t))
		}
	}
	return out, nil
}

// fileInfo 给「服务」页展示 exe 元信息。
type fileInfo struct {
	Path    string `json:"path"`
	Exists  bool   `json:"exists"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
}

func statFile(path string) fileInfo {
	fi := fileInfo{Path: path}
	if st, err := os.Stat(path); err == nil {
		fi.Exists = true
		fi.Size = st.Size()
		fi.ModTime = st.ModTime().Unix()
	}
	return fi
}
