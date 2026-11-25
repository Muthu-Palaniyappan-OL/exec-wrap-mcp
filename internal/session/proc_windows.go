//go:build windows

package session

import (
	"os"
	"os/exec"
	"strconv"
)

func setPipeAttr(cmd *exec.Cmd) {}

func interruptPipe(p *os.Process) error { return errUnsupported }

func terminateTree(p *os.Process) error {
	return exec.Command("taskkill", "/T", "/PID", strconv.Itoa(p.Pid)).Run()
}

func killTree(p *os.Process) error {
	if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(p.Pid)).Run(); err != nil {
		return p.Kill()
	}
	return nil
}
