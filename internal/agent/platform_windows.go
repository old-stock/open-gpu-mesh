//go:build windows

package agent

import (
	"os"
	"os/exec"
)

// Native Windows only supports the dev process runtime; real GPU nodes run the agent under WSL2.
func setProcessGroup(cmd *exec.Cmd) {}

func terminateGroup(p *os.Process, kill bool) { _ = p.Kill() }

func diskGB(path string) int { return 50 }
