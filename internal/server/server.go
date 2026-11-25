// Package server exposes the session manager as MCP tools.
package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Muthu-Palaniyappan-OL/exec-wrap-mcp/internal/session"
)

const (
	defaultMaxBytes = 64 << 10
	hardMaxBytes    = 1 << 20
	maxTimeout      = 600 * time.Second
)

// Options configure the MCP server.
type Options struct {
	Version string
	LogIO   bool
	Logger  *log.Logger
}

// WaitArgs controls when a call returns.
type WaitArgs struct {
	IdleMS      int    `json:"idle_ms,omitempty" jsonschema:"return after this many ms without new output (default 300; 5000 when a prompt is known)"`
	TimeoutMS   *int   `json:"timeout_ms,omitempty" jsonschema:"hard limit in ms (send default 30000, read default 0 = return pending output now, max 600000)"`
	PromptRegex string `json:"prompt_regex,omitempty" jsonschema:"override the session prompt regexp for this call"`
	UntilRegex  string `json:"until_regex,omitempty" jsonschema:"return as soon as the output matches this regexp"`
}

// LaunchIn is the input of the launch tool.
type LaunchIn struct {
	Command       string            `json:"command" jsonschema:"executable to run, e.g. /bin/bash, gdb, python3 (exec'd directly, never through a shell)"`
	Args          []string          `json:"args,omitempty" jsonschema:"arguments for the executable"`
	Cwd           string            `json:"cwd,omitempty" jsonschema:"working directory"`
	Env           map[string]string `json:"env,omitempty" jsonschema:"extra environment variables"`
	Preset        string            `json:"preset,omitempty" jsonschema:"prompt preset: bash zsh sh python gdb lldb psql node cmd powershell none (default: inferred from command)"`
	PromptRegex   string            `json:"prompt_regex,omitempty" jsonschema:"regexp that matches the end of the output when the program awaits input; overrides the preset"`
	TTY           *bool             `json:"tty,omitempty" jsonschema:"allocate a pseudo-terminal (default true); false uses plain pipes"`
	Rows          int               `json:"rows,omitempty" jsonschema:"terminal rows (default 40)"`
	Cols          int               `json:"cols,omitempty" jsonschema:"terminal columns (default 200)"`
	StartupWaitMS int               `json:"startup_wait_ms,omitempty" jsonschema:"how long to wait for the first prompt/banner (default 1000; shells wait up to 5000)"`
}

// LaunchOut is the output of the launch tool.
type LaunchOut struct {
	SessionID   string `json:"session_id"`
	PID         int    `json:"pid"`
	TTYMode     string `json:"tty_mode" jsonschema:"pty, conpty or pipe"`
	Preset      string `json:"preset"`
	PromptRegex string `json:"prompt_regex,omitempty"`
	OutputOut
}

// OutputOut is shared by send, read and signal.
type OutputOut struct {
	Output         string `json:"output"`
	DoneReason     string `json:"done_reason" jsonschema:"prompt, idle, until, timeout, exited, pending or no_wait"`
	Exited         bool   `json:"exited"`
	ExitCode       *int   `json:"exit_code,omitempty"`
	LastExitStatus *int   `json:"last_exit_status,omitempty" jsonschema:"exit status of the last shell command (shell presets only)"`
	Truncated      bool   `json:"truncated"`
	DroppedBytes   int    `json:"dropped_bytes"`
	ElapsedMS      int64  `json:"elapsed_ms"`
}

// SendIn is the input of the send tool.
type SendIn struct {
	SessionID string    `json:"session_id"`
	Input     string    `json:"input" jsonschema:"text to write to the program"`
	Newline   *bool     `json:"newline,omitempty" jsonschema:"append a newline (default true); set false for raw keystrokes"`
	Wait      *WaitArgs `json:"wait,omitempty"`
	StripANSI *bool     `json:"strip_ansi,omitempty" jsonschema:"remove terminal escape sequences (default true)"`
	MaxBytes  int       `json:"max_bytes,omitempty" jsonschema:"cap on returned bytes (default 65536, max 1048576); keeps head 25% and tail 75%"`
	NoWait    bool      `json:"no_wait,omitempty" jsonschema:"write and return immediately; fetch output later with read"`
}

// ReadIn is the input of the read tool.
type ReadIn struct {
	SessionID string    `json:"session_id"`
	Wait      *WaitArgs `json:"wait,omitempty"`
	StripANSI *bool     `json:"strip_ansi,omitempty"`
	MaxBytes  int       `json:"max_bytes,omitempty"`
}

// SignalIn is the input of the signal tool.
type SignalIn struct {
	SessionID string `json:"session_id"`
	Signal    string `json:"signal,omitempty" jsonschema:"interrupt (Ctrl-C, default), terminate, kill, eof (Ctrl-D) or suspend (Ctrl-Z, unix only)"`
}

// ResizeIn is the input of the resize tool.
type ResizeIn struct {
	SessionID string `json:"session_id"`
	Rows      int    `json:"rows"`
	Cols      int    `json:"cols"`
}

// OKOut is a bare acknowledgement.
type OKOut struct {
	OK bool `json:"ok"`
}

// ListIn has no fields.
type ListIn struct{}

// SessionInfo describes one session.
type SessionInfo struct {
	SessionID   string   `json:"session_id"`
	Command     string   `json:"command"`
	Args        []string `json:"args,omitempty"`
	PID         int      `json:"pid"`
	State       string   `json:"state" jsonschema:"running or exited"`
	ExitCode    *int     `json:"exit_code,omitempty"`
	AgeS        int64    `json:"age_s"`
	IdleS       int64    `json:"idle_s"`
	UnreadBytes int      `json:"unread_bytes"`
	TTYMode     string   `json:"tty_mode"`
	Preset      string   `json:"preset"`
}

// ListOut is the output of list.
type ListOut struct {
	Sessions []SessionInfo `json:"sessions"`
}

// KillIn is the input of the kill tool.
type KillIn struct {
	SessionID string `json:"session_id"`
	GraceMS   int    `json:"grace_ms,omitempty" jsonschema:"time to wait after a graceful terminate before force-killing (default 2000)"`
}

// KillOut is the output of kill.
type KillOut struct {
	ExitCode    int    `json:"exit_code"`
	FinalOutput string `json:"final_output"`
	Forced      bool   `json:"forced"`
}

type srv struct {
	m    *session.Manager
	opts Options
}

// New builds an MCP server with all tools registered.
func New(m *session.Manager, opts Options) *mcp.Server {
	if opts.Version == "" {
		opts.Version = "dev"
	}
	s := &srv{m: m, opts: opts}
	server := mcp.NewServer(&mcp.Implementation{Name: "exec-wrap-mcp", Version: opts.Version}, &mcp.ServerOptions{
		Instructions: "Persistent interactive sessions. launch a program (e.g. /bin/bash), send input to it repeatedly " +
			"(state such as environment variables, cwd and debugger context persists), then kill it. " +
			"If a send returns done_reason=timeout or idle while the program is still working, call read or signal.",
	})
	mcp.AddTool(server, &mcp.Tool{Name: "launch", Description: "Start an interactive program (shell, debugger, REPL...) as a persistent session and return its session_id plus the initial output."}, s.launch)
	mcp.AddTool(server, &mcp.Tool{Name: "send", Description: "Write input to a session, flush it, and return the output produced in response (waits for the prompt, idle time, or a timeout)."}, s.send)
	mcp.AddTool(server, &mcp.Tool{Name: "read", Description: "Return pending output of a session, optionally waiting for more. Use after a timeout or for background output."}, s.read)
	mcp.AddTool(server, &mcp.Tool{Name: "signal", Description: "Send Ctrl-C (interrupt), Ctrl-D (eof), terminate, kill or suspend to a session and return the resulting output."}, s.signal)
	mcp.AddTool(server, &mcp.Tool{Name: "resize", Description: "Resize the terminal of a pty session."}, s.resize)
	mcp.AddTool(server, &mcp.Tool{Name: "list", Description: "List all sessions."}, s.list)
	mcp.AddTool(server, &mcp.Tool{Name: "kill", Description: "Exit a session: terminate gracefully, force-kill after the grace period, and remove it."}, s.kill)
	return server
}

func (s *srv) logf(format string, a ...any) {
	if s.opts.LogIO && s.opts.Logger != nil {
		s.opts.Logger.Printf(format, a...)
	}
}

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func compile(name, expr string) (*regexp.Regexp, error) {
	if expr == "" {
		return nil, nil
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, session.Errorf(session.CodeInvalidArgument, "invalid %s: %v", name, err)
	}
	return re, nil
}

func buildWait(w *WaitArgs, defTimeout time.Duration) (session.WaitOpts, error) {
	o := session.WaitOpts{Timeout: defTimeout}
	if w == nil {
		return o, nil
	}
	o.Idle = ms(w.IdleMS)
	if w.TimeoutMS != nil {
		o.Timeout = ms(*w.TimeoutMS)
	}
	if o.Timeout > maxTimeout {
		o.Timeout = maxTimeout
	}
	var err error
	if o.Prompt, err = compile("prompt_regex", w.PromptRegex); err != nil {
		return o, err
	}
	if o.Until, err = compile("until_regex", w.UntilRegex); err != nil {
		return o, err
	}
	return o, nil
}

func buildOutput(strip *bool, maxBytes int) session.OutputOpts {
	o := session.OutputOpts{StripANSI: strip == nil || *strip, MaxBytes: maxBytes}
	if o.MaxBytes <= 0 {
		o.MaxBytes = defaultMaxBytes
	}
	if o.MaxBytes > hardMaxBytes {
		o.MaxBytes = hardMaxBytes
	}
	return o
}

func toOut(r session.Result) OutputOut {
	return OutputOut{
		Output: r.Output, DoneReason: r.Reason, Exited: r.Exited, ExitCode: r.ExitCode,
		LastExitStatus: r.LastExitStatus, Truncated: r.Truncated, DroppedBytes: r.DroppedBytes,
		ElapsedMS: r.Elapsed.Milliseconds(),
	}
}

// render produces the human/LLM-readable text form of a result.
func render(r session.Result) *mcp.CallToolResult {
	var b strings.Builder
	b.WriteString(r.Output)
	var notes []string
	switch {
	case r.Exited && r.ExitCode != nil:
		notes = append(notes, fmt.Sprintf("process exited with code %d", *r.ExitCode))
	case r.Reason == session.ReasonTimeout:
		notes = append(notes, "timeout: process is still running; call read to collect more output or signal to interrupt it")
	case r.Reason == session.ReasonIdle:
		notes = append(notes, "no output for a while and no prompt seen: the program may still be working; call read to check")
	}
	if r.LastExitStatus != nil && *r.LastExitStatus != 0 {
		notes = append(notes, fmt.Sprintf("last command exit status: %d", *r.LastExitStatus))
	}
	if r.Truncated {
		notes = append(notes, "output truncated")
	}
	if r.DroppedBytes > 0 {
		notes = append(notes, fmt.Sprintf("%d bytes of older output were dropped", r.DroppedBytes))
	}
	if len(notes) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		for _, n := range notes {
			b.WriteString("[" + n + "]\n")
		}
	}
	if b.Len() == 0 {
		b.WriteString("(no output)")
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.TrimRight(b.String(), "\n")}}}
}

func wrapErr(err error) error {
	var se *session.Error
	if errors.As(err, &se) {
		return se
	}
	return session.Errorf(session.CodeInternal, "%v", err)
}

func (s *srv) launch(ctx context.Context, _ *mcp.CallToolRequest, in LaunchIn) (*mcp.CallToolResult, LaunchOut, error) {
	tty := in.TTY == nil || *in.TTY
	sess, res, err := s.m.Launch(ctx, session.LaunchParams{
		Command: in.Command, Args: in.Args, Cwd: in.Cwd, Env: in.Env, Preset: in.Preset,
		PromptRegex: in.PromptRegex, TTY: tty, Rows: in.Rows, Cols: in.Cols, StartupWait: ms(in.StartupWaitMS),
	})
	if err != nil {
		return nil, LaunchOut{}, wrapErr(err)
	}
	s.logf("launch %s %v -> %s", in.Command, in.Args, sess.ID)
	out := LaunchOut{SessionID: sess.ID, PID: sess.PID(), TTYMode: sess.Mode(), Preset: sess.Preset,
		PromptRegex: sess.PromptPattern(), OutputOut: toOut(res)}
	r := render(res)
	r.Content[0].(*mcp.TextContent).Text = fmt.Sprintf("session_id: %s (pid %d, %s)\n%s", sess.ID, sess.PID(), sess.Mode(), r.Content[0].(*mcp.TextContent).Text)
	return r, out, nil
}

func (s *srv) send(ctx context.Context, _ *mcp.CallToolRequest, in SendIn) (*mcp.CallToolResult, OutputOut, error) {
	sess, err := s.m.Get(in.SessionID)
	if err != nil {
		return nil, OutputOut{}, wrapErr(err)
	}
	w, err := buildWait(in.Wait, 30*time.Second)
	if err != nil {
		return nil, OutputOut{}, wrapErr(err)
	}
	s.logf("[%s] <- %q", sess.ID, in.Input)
	res, err := sess.Send(ctx, in.Input, session.SendOpts{
		Newline: in.Newline == nil || *in.Newline, NoWait: in.NoWait, Wait: w, Output: buildOutput(in.StripANSI, in.MaxBytes),
	})
	if err != nil {
		return nil, OutputOut{}, withOutput(err, res)
	}
	s.logf("[%s] -> %q", sess.ID, res.Output)
	if in.NoWait {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "(written; call read to fetch output)"}}}, toOut(res), nil
	}
	return render(res), toOut(res), nil
}

// withOutput appends remaining output to session_exited errors.
func withOutput(err error, r session.Result) error {
	var se *session.Error
	if errors.As(err, &se) && se.Result != nil && se.Result.Output != "" {
		return session.Errorf(se.Code, "%s\n%s", se.Message, se.Result.Output)
	}
	_ = r
	return wrapErr(err)
}

func (s *srv) read(ctx context.Context, _ *mcp.CallToolRequest, in ReadIn) (*mcp.CallToolResult, OutputOut, error) {
	sess, err := s.m.Get(in.SessionID)
	if err != nil {
		return nil, OutputOut{}, wrapErr(err)
	}
	w, err := buildWait(in.Wait, 0)
	if err != nil {
		return nil, OutputOut{}, wrapErr(err)
	}
	res, err := sess.Read(ctx, w, buildOutput(in.StripANSI, in.MaxBytes))
	if err != nil {
		return nil, OutputOut{}, wrapErr(err)
	}
	s.logf("[%s] -> %q", sess.ID, res.Output)
	return render(res), toOut(res), nil
}

func (s *srv) signal(ctx context.Context, _ *mcp.CallToolRequest, in SignalIn) (*mcp.CallToolResult, OutputOut, error) {
	sess, err := s.m.Get(in.SessionID)
	if err != nil {
		return nil, OutputOut{}, wrapErr(err)
	}
	kind := in.Signal
	if kind == "" {
		kind = session.SigInterrupt
	}
	res, err := sess.Signal(ctx, kind, buildOutput(nil, 0))
	if err != nil {
		return nil, OutputOut{}, wrapErr(err)
	}
	return render(res), toOut(res), nil
}

func (s *srv) resize(_ context.Context, _ *mcp.CallToolRequest, in ResizeIn) (*mcp.CallToolResult, OKOut, error) {
	if in.Rows <= 0 || in.Cols <= 0 {
		return nil, OKOut{}, session.Errorf(session.CodeInvalidArgument, "rows and cols must be positive")
	}
	sess, err := s.m.Get(in.SessionID)
	if err != nil {
		return nil, OKOut{}, wrapErr(err)
	}
	if err := sess.Resize(in.Rows, in.Cols); err != nil {
		return nil, OKOut{}, wrapErr(err)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, OKOut{OK: true}, nil
}

func (s *srv) list(_ context.Context, _ *mcp.CallToolRequest, _ ListIn) (*mcp.CallToolResult, ListOut, error) {
	infos := s.m.List()
	out := ListOut{Sessions: make([]SessionInfo, 0, len(infos))}
	var b strings.Builder
	for _, i := range infos {
		out.Sessions = append(out.Sessions, SessionInfo{
			SessionID: i.ID, Command: i.Command, Args: i.Args, PID: i.PID, State: i.State, ExitCode: i.ExitCode,
			AgeS: int64(i.Age.Seconds()), IdleS: int64(i.Idle.Seconds()), UnreadBytes: i.UnreadBytes, TTYMode: i.Mode, Preset: i.Preset,
		})
		fmt.Fprintf(&b, "%s  %s %s  pid=%d  %s  unread=%dB\n", i.ID, i.Command, strings.Join(i.Args, " "), i.PID, i.State, i.UnreadBytes)
	}
	if b.Len() == 0 {
		b.WriteString("(no sessions)")
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.TrimRight(b.String(), "\n")}}}, out, nil
}

func (s *srv) kill(_ context.Context, _ *mcp.CallToolRequest, in KillIn) (*mcp.CallToolResult, KillOut, error) {
	grace := ms(in.GraceMS)
	if grace <= 0 {
		grace = 2 * time.Second
	}
	code, forced, res, err := s.m.Kill(in.SessionID, grace)
	if err != nil {
		return nil, KillOut{}, wrapErr(err)
	}
	s.logf("kill %s", in.SessionID)
	text := fmt.Sprintf("session %s ended (exit code %d, forced=%v)", in.SessionID, code, forced)
	if res.Output != "" {
		text += "\n" + res.Output
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, KillOut{ExitCode: code, FinalOutput: res.Output, Forced: forced}, nil
}
