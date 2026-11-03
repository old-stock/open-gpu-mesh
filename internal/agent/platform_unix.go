//go:build !windows

package agent

import (
	"os"
	"os/exec"
	"syscall"
)

func setProcessGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// terminateGroup signals the whole process group so child processes die too.
func terminateGroup(p *os.Process, kill bool) {
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	_ = syscall.Kill(-p.Pid, sig)
}

func diskGB(path string) int {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 50
	}
	return int(st.Bavail * uint64(st.Bsize) >> 30)
}
