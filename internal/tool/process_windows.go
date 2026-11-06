//go:build windows

package tool

import (
	"os/exec"
	"strconv"
)

func setProcessGroup(cmd *exec.Cmd) {
	// Windows 没有进程组信号；用 taskkill /T 终止子树。
}

func terminateProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	_ = exec.Command("taskkill", "/T", "/F", "/PID", pid).Run()
}

func killProcessTree(cmd *exec.Cmd) {
	terminateProcess(cmd)
}
