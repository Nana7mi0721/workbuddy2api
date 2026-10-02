//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// CREATE_NO_WINDOW：子进程不弹控制台窗口（面板本身也是无窗口运行的）。
const createNoWindow = 0x08000000

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

func lookupPortPID(port int) int { return netstatPID(port) }

func killPID(pid int) error {
	out, errOut, err := runCaptured("taskkill", []string{"/PID", strconv.Itoa(pid), "/F"}, "", 10*time.Second)
	if err != nil {
		msg := strings.TrimSpace(out + " " + errOut)
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("taskkill 失败: %s", msg)
	}
	return nil
}

// startDetached 后台拉起一个命令并立即返回（不等待其结束）。
func startDetached(name string, args []string, dir string) error {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// startDetachedLog 后台拉起网关进程，把 stdout/stderr 追加进日志文件（等价于启动脚本的重定向）。
func startDetachedLog(exe string, args []string, dir, logPath string) error {
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
	hideWindow(cmd)
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
