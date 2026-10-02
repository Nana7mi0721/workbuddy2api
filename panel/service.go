package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ---------- 进程/端口探测 ----------

// portOfListen 把 ":7863" / "0.0.0.0:7863" / "127.0.0.1:7863" 解析成端口号。
func portOfListen(listen string) int {
	if listen == "" {
		return 0
	}
	i := strings.LastIndex(listen, ":")
	if i < 0 {
		return 0
	}
	p, _ := strconv.Atoi(strings.TrimSpace(listen[i+1:]))
	return p
}

// findPIDByPort 返回监听该端口的进程 PID（0 = 未监听）。
func findPIDByPort(port int) int {
	return lookupPortPID(port)
}

// ---------- 启停 ----------

type ServiceRunner struct {
	cfg *PanelConfig
}

func (s *ServiceRunner) Listen() string { return s.cfg.Gateway.BaseURL }

// Status 汇总服务运行态。
type ServiceStatus struct {
	Running    bool      `json:"running"`
	PID        int       `json:"pid"`
	Port       int       `json:"port"`
	Listen     string    `json:"listen"`
	BaseURL    string    `json:"base_url"`
	ServerExe  fileInfo  `json:"server_exe"`
	LoginExe   fileInfo  `json:"login_exe"`
	ConfigFile fileInfo  `json:"config_file"`
	LogFile    fileInfo  `json:"log_file"`
	StartScr   string    `json:"start_script"`
	StartScrOK bool      `json:"start_script_exists"`
	BaseDir    string    `json:"base_dir"`
	LogLines   int       `json:"log_lines"`
	Now        time.Time `json:"now"`
}

func (s *ServiceRunner) Status(gw *GatewayConfig) *ServiceStatus {
	port := portOfListen(gw.Listen)
	if port == 0 {
		port = portOfListen(s.cfg.Gateway.BaseURL)
	}
	st := &ServiceStatus{
		Port:       port,
		Listen:     gw.Listen,
		BaseURL:    s.cfg.Gateway.BaseURL,
		PID:        findPIDByPort(port),
		ServerExe:  statFile(s.cfg.ServerExe()),
		LoginExe:   statFile(s.cfg.LoginExe()),
		ConfigFile: statFile(s.cfg.ConfigPath()),
		LogFile:    statFile(s.cfg.LogPath()),
		StartScr:   s.cfg.Gateway.StartScript,
		BaseDir:    s.cfg.BaseDir(),
		Now:        time.Now(),
	}
	st.Running = st.PID > 0
	if st.StartScr != "" {
		if _, err := os.Stat(st.StartScr); err == nil {
			st.StartScrOK = true
		}
	}
	return st
}

// Start 启动网关（优先用配置里的启动脚本，失败则直接起 exe）。
func (s *ServiceRunner) Start(gw *GatewayConfig) (string, error) {
	port := portOfListen(gw.Listen)
	if port > 0 && findPIDByPort(port) > 0 {
		return "", fmt.Errorf("端口 %d 已被占用，服务已在运行", port)
	}
	script := s.cfg.Gateway.StartScript
	if script != "" {
		if _, err := os.Stat(script); err == nil {
			if err := startDetached("cmd", []string{"/c", script}, filepath0(script)); err != nil {
				return "", fmt.Errorf("执行启动脚本失败: %w", err)
			}
			return "已通过启动脚本拉起：" + script, nil
		}
	}
	exe := s.cfg.ServerExe()
	if _, err := os.Stat(exe); err != nil {
		return "", fmt.Errorf("找不到网关可执行文件：%s", exe)
	}
	args := strings.Fields(s.cfg.Gateway.ConfigArgs)
	logPath := s.cfg.LogPath()
	_ = os.MkdirAll(dirOf(logPath), 0o755)
	if err := startDetachedLog(exe, args, s.cfg.BaseDir(), logPath); err != nil {
		return "", fmt.Errorf("启动 %s 失败: %w", exe, err)
	}
	return "已直接启动：" + exe + " " + strings.Join(args, " "), nil
}

// Stop 结束监听端口的进程。
func (s *ServiceRunner) Stop(gw *GatewayConfig) (int, error) {
	port := portOfListen(gw.Listen)
	pid := findPIDByPort(port)
	if pid == 0 {
		return 0, fmt.Errorf("端口 %d 上没有监听进程", port)
	}
	if err := killPID(pid); err != nil {
		return pid, err
	}
	// 等端口释放，最多 8s。
	for i := 0; i < 80; i++ {
		if findPIDByPort(port) == 0 {
			return pid, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return pid, fmt.Errorf("已请求结束 PID %d，但端口 %d 仍被占用", pid, port)
}

// Restart 先停后起。
func (s *ServiceRunner) Restart(gw *GatewayConfig) (string, error) {
	port := portOfListen(gw.Listen)
	var msg strings.Builder
	if findPIDByPort(port) > 0 {
		pid, err := s.Stop(gw)
		if err != nil {
			return "", fmt.Errorf("停止失败: %w", err)
		}
		fmt.Fprintf(&msg, "已停止 PID %d；", pid)
	}
	out, err := s.Start(gw)
	if err != nil {
		return msg.String(), err
	}
	msg.WriteString(out)
	// 等待端口就绪（最多 15s）。
	for i := 0; i < 150; i++ {
		if findPIDByPort(port) > 0 {
			fmt.Fprintf(&msg, "（端口 %d 已就绪，新 PID %d）", port, findPIDByPort(port))
			return msg.String(), nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return msg.String(), fmt.Errorf("已发起启动，但 %v 内端口 %d 未就绪", 15*time.Second, port)
}

// ---------- 小工具 ----------

func dirOf(p string) string {
	i := strings.LastIndexAny(p, `\/`)
	if i < 0 {
		return "."
	}
	return p[:i]
}

func filepath0(p string) string { return dirOf(p) }

// runCaptured 跑一个短命令并抓取输出（用于 login.exe、netstat 等）。
func runCaptured(name string, args []string, dir string, timeout time.Duration) (string, string, error) {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	hideWindow(cmd)
	pr, pw, err := os.Pipe()
	if err != nil {
		return "", "", err
	}
	pe, pw2, err := os.Pipe()
	if err != nil {
		pr.Close()
		pw.Close()
		return "", "", err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw2
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		pe.Close()
		pw2.Close()
		return "", "", err
	}
	pw.Close()
	pw2.Close()
	outCh := make(chan string, 1)
	errCh := make(chan string, 1)
	go func() { outCh <- readAll(pr) }()
	go func() { errCh <- readAll(pe) }()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return <-outCh, <-errCh, err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		return <-outCh, <-errCh, fmt.Errorf("命令超时（%v）：%s", timeout, name)
	}
}

func readAll(f *os.File) string {
	defer f.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := f.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return string(buf)
}

// netstatPID 从 `netstat -ano` 输出里找监听指定端口的 PID（Windows 与部分平台通用）。
func netstatPID(port int) int {
	out, _, err := runCaptured("netstat", []string{"-ano"}, "", 5*time.Second)
	if err != nil && out == "" {
		return 0
	}
	suffix := ":" + strconv.Itoa(port)
	re := regexp.MustCompile(`\s+(\d+)\s*$`)
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimRight(line, "\r")
		if !strings.Contains(l, "LISTEN") && !strings.Contains(l, "LISTENING") {
			continue
		}
		fields := strings.Fields(l)
		if len(fields) < 2 {
			continue
		}
		local := fields[1]
		if !strings.HasSuffix(local, suffix) {
			continue
		}
		if m := re.FindStringSubmatch(l); m != nil {
			pid, _ := strconv.Atoi(m[1])
			return pid
		}
		if pid, err := strconv.Atoi(fields[len(fields)-1]); err == nil {
			return pid
		}
	}
	return 0
}
