//go:build !windows

package session

import (
	"os"
	"os/exec"
	"syscall"
)

func setPipeAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// Every child is a process-group leader (setsid for ptys, setpgid for pipes),
// so signalling -pid reaches its whole job.
func signalTree(p *os.Process, sig syscall.Signal) error {
	if err := syscall.Kill(-p.Pid, sig); err != nil {
		return p.Signal(sig)
	}
	return nil
}

func interruptPipe(p *os.Process) error { return signalTree(p, syscall.SIGINT) }

// Interactive shells ignore SIGTERM but exit on SIGHUP, so send both.
func terminateTree(p *os.Process) error {
	_ = signalTree(p, syscall.SIGHUP)
	return signalTree(p, syscall.SIGTERM)
}

func killTree(p *os.Process) error { return signalTree(p, syscall.SIGKILL) }
