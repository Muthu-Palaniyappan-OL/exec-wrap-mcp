//go:build !windows

package session

import (
	"os"
	"os/exec"
	"strconv"
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

var sigNames = map[syscall.Signal]string{
	syscall.SIGHUP: "SIGHUP", syscall.SIGINT: "SIGINT", syscall.SIGQUIT: "SIGQUIT",
	syscall.SIGABRT: "SIGABRT", syscall.SIGKILL: "SIGKILL", syscall.SIGSEGV: "SIGSEGV",
	syscall.SIGPIPE: "SIGPIPE", syscall.SIGTERM: "SIGTERM", syscall.SIGBUS: "SIGBUS",
}

// exitStatus returns the exit code, using the shell convention 128+N and the
// signal name when the process was killed by a signal.
func exitStatus(ps *os.ProcessState, err error) (int, string) {
	if ps == nil {
		return -1, ""
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		name := sigNames[ws.Signal()]
		if name == "" {
			name = "signal " + strconv.Itoa(int(ws.Signal()))
		}
		return 128 + int(ws.Signal()), name
	}
	return ps.ExitCode(), ""
}
