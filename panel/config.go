package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PanelConfig 是面板自身的配置（panel.json），与网关的 config.json 分离。
// 所有网关相关路径默认相对于 panel.json 所在目录（= 网关部署目录）。
type PanelConfig struct {
	Listen  string      `json:"listen"`
	Gateway GatewayCfg  `json:"gateway"`
	Auth    AuthCfg     `json:"auth"`
	UI      UICfg       `json:"ui"`
	path    string      // panel.json 绝对路径（不序列化）
	baseDir string      // 网关部署目录（= panel.json 所在目录）
}

type GatewayCfg struct {
	BaseURL     string `json:"base_url"`
	Dir         string `json:"dir"`
	ConfigFile  string `json:"config_file"`
	AuthDir     string `json:"auth_dir"`
	PausedDir   string `json:"paused_dir"`
	StateFile   string `json:"state_file"`
	LogFile     string `json:"log_file"`
	AuditFile   string `json:"audit_file"`
	ServerExe   string `json:"server_exe"`
	LoginExe    string `json:"login_exe"`
	StartScript string `json:"start_script"`
	StopScript  string `json:"stop_script"`
	ConfigArgs  string `json:"config_args"`
}

type AuthCfg struct {
	Password string `json:"password"`
}

type UICfg struct {
	RefreshMS int `json:"refresh_ms"`
	LogTail   int `json:"log_tail"`
}

func defaultPanelConfig() *PanelConfig {
	return &PanelConfig{
		Listen: "127.0.0.1:7864",
		Gateway: GatewayCfg{
			BaseURL:     "http://127.0.0.1:7863",
			Dir:         ".",
			ConfigFile:  "config.json",
			AuthDir:     "auths",
			PausedDir:   "auths-paused",
			StateFile:   "data/state.json",
			LogFile:     "logs/console.log",
			AuditFile:   "data/admin_audit.log",
			ServerExe:   "wb2api.exe",
			LoginExe:    "login.exe",
			StartScript: `D:\Program\autostart\run-workbuddy2api.bat`,
			StopScript:  "",
			ConfigArgs:  "-config config.json",
		},
		Auth: AuthCfg{Password: ""},
		UI:   UICfg{RefreshMS: 5000, LogTail: 500},
	}
}

// loadPanelConfig 读取 panel.json；不存在则按默认值生成一份。
func loadPanelConfig() (*PanelConfig, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	exeDir := filepath.Dir(exe)
	path := filepath.Join(exeDir, "panel.json")
	if v := os.Getenv("WB_PANEL_CONFIG"); v != "" {
		if abs, err := filepath.Abs(v); err == nil {
			path = abs
		}
	}

	cfg := defaultPanelConfig()
	raw, rerr := os.ReadFile(path)
	if rerr == nil {
		if err := json.Unmarshal(raw, cfg); err != nil {
			return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
		}
	} else if !os.IsNotExist(rerr) {
		return nil, fmt.Errorf("读取 %s 失败: %w", path, rerr)
	}

	cfg.path = path
	base := filepath.Dir(path)
	if cfg.Gateway.Dir != "" && cfg.Gateway.Dir != "." {
		if abs, err := filepath.Abs(cfg.Gateway.Dir); err == nil {
			base = abs
		}
	}
	cfg.baseDir = base

	if rerr != nil {
		// 首次运行：落盘默认配置，便于用户直接改。
		if b, err := json.MarshalIndent(cfg, "", "  "); err == nil {
			_ = os.WriteFile(path, append(b, '\n'), 0o644)
		}
	}
	return cfg, nil
}

// save 写回 panel.json（原子替换）。
func (c *PanelConfig) save() error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// resolve 把配置里的相对路径解析成绝对路径（相对网关部署目录）。
func (c *PanelConfig) resolve(p string) string {
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(c.baseDir, p))
}

func (c *PanelConfig) ConfigPath() string { return c.resolve(c.Gateway.ConfigFile) }
func (c *PanelConfig) AuthDir() string    { return c.resolve(c.Gateway.AuthDir) }
func (c *PanelConfig) PausedDir() string  { return c.resolve(c.Gateway.PausedDir) }
func (c *PanelConfig) StatePath() string  { return c.resolve(c.Gateway.StateFile) }
func (c *PanelConfig) LogPath() string    { return c.resolve(c.Gateway.LogFile) }
func (c *PanelConfig) AuditPath() string {
	if c.Gateway.AuditFile == "" {
		return ""
	}
	return c.resolve(c.Gateway.AuditFile)
}
func (c *PanelConfig) ServerExe() string { return c.resolve(c.Gateway.ServerExe) }
func (c *PanelConfig) LoginExe() string  { return c.resolve(c.Gateway.LoginExe) }
func (c *PanelConfig) BaseDir() string   { return c.baseDir }

// GatewayConfig 读取网关 config.json 的关键字段（api_key / listen / admin / metrics）。
// 面板用它来代填鉴权头，浏览器端永远拿不到密钥。
type GatewayConfig struct {
	Listen string `json:"listen"`
	APIKey string `json:"api_key"`
	Admin  struct {
		Enabled      bool   `json:"enabled"`
		AuditEnabled bool   `json:"audit_enabled"`
		AuditFile    string `json:"audit_file"`
	} `json:"admin"`
	Metrics struct {
		Enabled bool `json:"enabled"`
	} `json:"metrics"`
}

func (c *PanelConfig) readGatewayConfig() (*GatewayConfig, error) {
	raw, err := os.ReadFile(c.ConfigPath())
	if err != nil {
		return nil, err
	}
	var gc GatewayConfig
	if err := json.Unmarshal(raw, &gc); err != nil {
		return nil, fmt.Errorf("网关 config.json 解析失败: %w", err)
	}
	// 网关监听地址 → 面板默认探测地址（listen 形如 ":7863" 或 "0.0.0.0:7863"）。
	if c.Gateway.BaseURL == "" {
		host := "127.0.0.1"
		port := "7863"
		if i := strings.LastIndex(gc.Listen, ":"); i >= 0 {
			port = gc.Listen[i+1:]
		}
		c.Gateway.BaseURL = "http://" + host + ":" + port
	}
	return &gc, nil
}
