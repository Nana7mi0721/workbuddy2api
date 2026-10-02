//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func hideWindow(cmd *exec.Cmd) {}

// lookupPortPID 在非 Windows 上用 `lsof -t -i:PORT` / `ss` 探测（尽力而为）。
func lookupPortPID(port int) int {
	if _, err := exec.LookPath("lsof"); err == nil {
		out, _, err := runCaptured("lsof", []string{"-t", "-i", ":" + strconv.Itoa(port), "-sTCP:LISTEN"}, "", 5*time.Second)
		if err == nil {
			for _, l := range strings.Split(out, "\n") {
				if pid, err := strconv.Atoi(strings.TrimSpace(l)); err == nil && pid > 0 {
					return pid
				}
			}
		}
	}
	// 回退：扫 /proc/*/comm 找不到端口，这里直接返回 0（面板在非 Windows 上仅做展示）。
	return 0
}

func killPID(pid int) error {
	cmd := exec.Command("kill", "-TERM", strconv.Itoa(pid))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kill 失败: %w", err)
	}
	return nil
}

func startDetached(name string, args []string, dir string) error {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func startDetachedLog(exe string, args []string, dir, logPath string) error {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	fmt.Fprintf(f, "\n================ %s start（panel 直接启动） ================\n", time.Now().Format("2006/01/02 15:04:05"))
	cmd := exec.Command(exe, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdout = f
	cmd.Stderr = f
	if err := cmd.Start(); err != nil {
		f.Close()
		return err
	}
	go func() {
		_ = cmd.Wait()
		f.Close()
	}()
	return nil
}
