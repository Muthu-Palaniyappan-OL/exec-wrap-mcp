package session

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	pty "github.com/aymanbagabas/go-pty"
)

// process is a running child attached to a pty, ConPTY or plain pipes.
type process interface {
	io.Writer
	Read(p []byte) (int, error)
	Wait() (int, error)
	PID() int
	Mode() string
	Resize(rows, cols int) error
	Interrupt() error
	Suspend() error
	EOF() error
	Terminate() error
	Kill() error
	// Drain is called after the child exited so the reader sees EOF once
	// buffered output has been consumed.
	Drain()
	Close() error
}

type startSpec struct {
	command string
	args    []string
	dir     string
	env     []string
	tty     bool
	rows    int
	cols    int
}

var errUnsupported = errors.New("unsupported on this platform or mode")

func startProcess(sp startSpec) (process, error) {
	if sp.tty {
		p, err := startPTY(sp)
		if err == nil {
			return p, nil
		}
		if runtime.GOOS != "windows" {
			return nil, err
		}
		// ConPTY unavailable (old Windows): fall back to pipes.
	}
	return startPipe(sp)
}

// ---- pty / ConPTY ----

type ptyProc struct {
	p    pty.Pty
	cmd  *pty.Cmd
	mode string
}

func startPTY(sp startSpec) (*ptyProc, error) {
	p, err := pty.New()
	if err != nil {
		return nil, err
	}
	if err := p.Resize(sp.cols, sp.rows); err != nil {
		_ = p.Close()
		return nil, err
	}
	cmd := p.Command(sp.command, sp.args...)
	cmd.Env = sp.env
	cmd.Dir = sp.dir
	if err := cmd.Start(); err != nil {
		_ = p.Close()
		return nil, err
	}
	mode := "pty"
	if runtime.GOOS == "windows" {
		mode = "conpty"
	}
	return &ptyProc{p: p, cmd: cmd, mode: mode}, nil
}

func (p *ptyProc) Write(b []byte) (int, error) { return p.p.Write(b) }
func (p *ptyProc) Read(b []byte) (int, error)  { return p.p.Read(b) }
func (p *ptyProc) PID() int                    { return p.cmd.Process.Pid }
func (p *ptyProc) Mode() string                { return p.mode }
func (p *ptyProc) Resize(rows, cols int) error { return p.p.Resize(cols, rows) }
func (p *ptyProc) Interrupt() error            { _, err := p.p.Write([]byte{0x03}); return err }
func (p *ptyProc) EOF() error                  { _, err := p.p.Write([]byte{0x04}); return err }
func (p *ptyProc) Suspend() error {
	if runtime.GOOS == "windows" {
		return errUnsupported
	}
	_, err := p.p.Write([]byte{0x1a})
	return err
}
func (p *ptyProc) Terminate() error { return terminateTree(p.cmd.Process) }
func (p *ptyProc) Kill() error      { return killTree(p.cmd.Process) }
func (p *ptyProc) Close() error     { return p.p.Close() }
func (p *ptyProc) Wait() (int, error) {
	err := p.cmd.Wait()
	return exitCode(p.cmd.ProcessState, err), nil
}

func (p *ptyProc) Drain() {
	if u, ok := p.p.(pty.UnixPty); ok {
		if s := u.Slave(); s != nil {
			_ = s.Close()
		}
	}
}

// ---- pipes ----

type pipeProc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *os.File
}

func startPipe(sp startSpec) (*pipeProc, error) {
	cmd := exec.Command(sp.command, sp.args...)
	cmd.Env = sp.env
	cmd.Dir = sp.dir
	setPipeAttr(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = w, w
	if err := cmd.Start(); err != nil {
		_ = r.Close()
		_ = w.Close()
		return nil, err
	}
	_ = w.Close()
	return &pipeProc{cmd: cmd, stdin: stdin, out: r}, nil
}

func (p *pipeProc) Write(b []byte) (int, error) { return p.stdin.Write(b) }
func (p *pipeProc) Read(b []byte) (int, error)  { return p.out.Read(b) }
func (p *pipeProc) PID() int                    { return p.cmd.Process.Pid }
func (p *pipeProc) Mode() string                { return "pipe" }
func (p *pipeProc) Resize(int, int) error       { return errUnsupported }
func (p *pipeProc) Suspend() error              { return errUnsupported }
func (p *pipeProc) EOF() error                  { return p.stdin.Close() }
func (p *pipeProc) Interrupt() error            { return interruptPipe(p.cmd.Process) }
func (p *pipeProc) Terminate() error            { return terminateTree(p.cmd.Process) }
func (p *pipeProc) Kill() error                 { return killTree(p.cmd.Process) }
func (p *pipeProc) Drain()                      {}
func (p *pipeProc) Close() error {
	_ = p.stdin.Close()
	return p.out.Close()
}
func (p *pipeProc) Wait() (int, error) {
	err := p.cmd.Wait()
	return exitCode(p.cmd.ProcessState, err), nil
}

func exitCode(ps *os.ProcessState, err error) int {
	if ps != nil {
		return ps.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// mergeEnv overlays overrides on base, de-duplicating keys (case-insensitive
// on Windows).
func mergeEnv(base []string, overrides map[string]string) []string {
	norm := func(k string) string {
		if runtime.GOOS == "windows" {
			return strings.ToUpper(k)
		}
		return k
	}
	idx := map[string]int{}
	var out []string
	set := func(k, v string) {
		kv := k + "=" + v
		if i, ok := idx[norm(k)]; ok {
			out[i] = kv
			return
		}
		idx[norm(k)] = len(out)
		out = append(out, kv)
	}
	for _, e := range base {
		if k, v, ok := strings.Cut(e, "="); ok && k != "" {
			set(k, v)
		}
	}
	for k, v := range overrides {
		set(k, v)
	}
	return out
}
