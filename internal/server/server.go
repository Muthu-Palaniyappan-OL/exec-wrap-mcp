// Package server exposes the session manager as MCP tools. Schemas and
// results are deliberately terse: every byte here is paid for in LLM tokens.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Muthu-Palaniyappan-OL/exec-wrap-mcp/internal/session"
)

const (
	defaultWait = 30 * time.Second
	maxWait     = 600 * time.Second
)

// Options configure the MCP server.
type Options struct {
	Version    string
	LogIO      bool
	Logger     *log.Logger
	Structured bool // also return machine-readable structuredContent
}

// Tool argument schemas, written by hand to keep them small. Only
// non-obvious fields carry a description.
const (
	launchSchema = `{"type":"object","required":["command"],"properties":{
"command":{"type":"string","description":"program to run, e.g. /bin/bash, python3, lldb"},
"args":{"type":"array","items":{"type":"string"}},
"cwd":{"type":"string"},
"env":{"type":"object","additionalProperties":{"type":"string"}},
"input":{"type":"string","description":"first input, sent once the program is ready"},
"wait_ms":{"type":"integer","description":"max wait for readiness and first reply (default 30000)"},
"preset":{"type":"string","description":"bash zsh sh python gdb lldb psql node cmd powershell none; default inferred"},
"prompt_regex":{"type":"string","description":"regex for the end of output when the program awaits input"},
"ready_regex":{"type":"string","description":"regex that means startup finished (for programs without a prompt)"},
"pipe":{"type":"boolean","description":"use pipes instead of a terminal"},
"rows":{"type":"integer"},"cols":{"type":"integer"}}}`

	sendSchema = `{"type":"object","required":["session_id"],"properties":{
"session_id":{"type":"string"},
"input":{"type":"string","description":"text to send, newline added; omit to just read more output"},
"key":{"type":"string","enum":["ctrl-c","ctrl-d","ctrl-z","term","kill"],"description":"send a key/signal instead of text"},
"raw":{"type":"boolean","description":"do not append a newline"},
"wait_ms":{"type":"integer","description":"max wait (default 30000, max 600000)"},
"idle_ms":{"type":"integer","description":"return after this much silence (default 300; 5000 if a prompt is known)"},
"until":{"type":"string","description":"return as soon as output matches this regex"},
"no_wait":{"type":"boolean","description":"write and return at once"}}}`

	killSchema = `{"type":"object","required":["session_id"],"properties":{"session_id":{"type":"string"}}}`
	listSchema = `{"type":"object"}`
)

// LaunchIn is the input of launch.
type LaunchIn struct {
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Cwd         string            `json:"cwd"`
	Env         map[string]string `json:"env"`
	Input       string            `json:"input"`
	WaitMS      int               `json:"wait_ms"`
	Preset      string            `json:"preset"`
	PromptRegex string            `json:"prompt_regex"`
	ReadyRegex  string            `json:"ready_regex"`
	Pipe        bool              `json:"pipe"`
	Rows        int               `json:"rows"`
	Cols        int               `json:"cols"`
}

// SendIn is the input of send.
type SendIn struct {
	SessionID string `json:"session_id"`
	Input     string `json:"input"`
	Key       string `json:"key"`
	Raw       bool   `json:"raw"`
	WaitMS    int    `json:"wait_ms"`
	IdleMS    int    `json:"idle_ms"`
	Until     string `json:"until"`
	NoWait    bool   `json:"no_wait"`
}

// KillIn is the input of kill.
type KillIn struct {
	SessionID string `json:"session_id"`
}

// ListIn has no fields.
type ListIn struct{}

type srv struct {
	m    *session.Manager
	opts Options
}

func schema(src string) *jsonschema.Schema {
	var s jsonschema.Schema
	if err := json.Unmarshal([]byte(src), &s); err != nil {
		panic(err)
	}
	return &s
}

var keys = map[string]string{
	"ctrl-c": session.SigInterrupt, "ctrl-d": session.SigEOF, "ctrl-z": session.SigSuspend,
	"term": session.SigTerminate, "kill": session.SigKill,
}

// New builds an MCP server with all tools registered.
func New(m *session.Manager, opts Options) *mcp.Server {
	if opts.Version == "" {
		opts.Version = "dev"
	}
	s := &srv{m: m, opts: opts}
	server := mcp.NewServer(&mcp.Implementation{Name: "exec-wrap-mcp", Version: opts.Version}, &mcp.ServerOptions{
		Instructions: "Persistent interactive sessions: launch a program once, send inputs over many calls, state is kept.",
	})
	mcp.AddTool(server, &mcp.Tool{Name: "launch", InputSchema: schema(launchSchema),
		Description: "Start an interactive program (shell, debugger, REPL) as a session. Returns the session id and output."}, s.launch)
	mcp.AddTool(server, &mcp.Tool{Name: "send", InputSchema: schema(sendSchema),
		Description: "Send input (or a key) to a session and return its reply. No input = read more output."}, s.send)
	mcp.AddTool(server, &mcp.Tool{Name: "kill", InputSchema: schema(killSchema),
		Description: "End a session. Idle sessions also expire on their own."}, s.kill)
	mcp.AddTool(server, &mcp.Tool{Name: "list", InputSchema: schema(listSchema),
		Description: "List sessions."}, s.list)
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

func timeout(n int) time.Duration {
	d := ms(n)
	switch {
	case d <= 0:
		return defaultWait
	case d > maxWait:
		return maxWait
	}
	return d
}

func wrapErr(err error) error {
	var se *session.Error
	if errors.As(err, &se) {
		return se
	}
	return session.Errorf(session.CodeInternal, "%v", err)
}

// notes lists the one-line annotations that follow the output.
func notes(r session.Result) []string {
	var n []string
	switch {
	case r.Exited && r.ExitSignal != "":
		n = append(n, "killed by "+r.ExitSignal)
	case r.Exited && r.ExitCode != nil:
		n = append(n, fmt.Sprintf("exited %d", *r.ExitCode))
	case r.Reason == session.ReasonTimeout:
		n = append(n, "timeout: still running; send with no input to read more")
	case r.Reason == session.ReasonIdle:
		n = append(n, "idle: no prompt seen, may still be running")
	case r.Reason == session.ReasonStarting:
		n = append(n, "starting: still loading, input not sent; send again")
	}
	if r.LastExitStatus != nil && *r.LastExitStatus != 0 && !r.Exited {
		n = append(n, fmt.Sprintf("exit %d", *r.LastExitStatus))
	}
	if r.Truncated {
		n = append(n, "truncated")
	}
	if r.DroppedBytes > 0 {
		n = append(n, fmt.Sprintf("dropped %dB", r.DroppedBytes))
	}
	return n
}

func text(head string, r session.Result) string {
	var b strings.Builder
	b.WriteString(head)
	if r.Output != "" {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(r.Output)
	}
	for _, n := range notes(r) {
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString("[" + n + "]")
	}
	if b.Len() == 0 {
		return "ok"
	}
	return b.String()
}

func resultMap(r session.Result) map[string]any {
	m := map[string]any{
		"output": r.Output, "done_reason": r.Reason, "exited": r.Exited,
		"truncated": r.Truncated, "dropped_bytes": r.DroppedBytes, "elapsed_ms": r.Elapsed.Milliseconds(),
	}
	if r.ExitCode != nil {
		m["exit_code"] = *r.ExitCode
	}
	if r.ExitSignal != "" {
		m["exit_signal"] = r.ExitSignal
	}
	if r.LastExitStatus != nil {
		m["last_exit_status"] = *r.LastExitStatus
	}
	return m
}

func (s *srv) reply(txt string, data map[string]any) (*mcp.CallToolResult, any, error) {
	res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: txt}}}
	if s.opts.Structured {
		res.StructuredContent = data
	}
	return res, nil, nil
}

func (s *srv) launch(ctx context.Context, _ *mcp.CallToolRequest, in LaunchIn) (*mcp.CallToolResult, any, error) {
	sess, res, err := s.m.Launch(ctx, session.LaunchParams{
		Command: in.Command, Args: in.Args, Cwd: in.Cwd, Env: in.Env, Preset: in.Preset,
		PromptRegex: in.PromptRegex, ReadyRegex: in.ReadyRegex, Input: in.Input, TTY: !in.Pipe,
		Rows: in.Rows, Cols: in.Cols, Wait: ms(in.WaitMS),
	})
	if err != nil {
		return nil, nil, withOutput(err)
	}
	s.logf("launch %s %v -> %s", in.Command, in.Args, sess.ID)
	if in.Input != "" {
		s.logf("[%s] -> %q", sess.ID, res.Output)
	}
	data := resultMap(res)
	data["session_id"], data["pid"], data["tty_mode"], data["preset"] = sess.ID, sess.PID(), sess.Mode(), sess.Preset
	return s.reply(text(sess.ID, res), data)
}

func (s *srv) send(ctx context.Context, _ *mcp.CallToolRequest, in SendIn) (*mcp.CallToolResult, any, error) {
	sess, err := s.m.Get(in.SessionID)
	if err != nil {
		return nil, nil, wrapErr(err)
	}
	if in.Key != "" && in.Input != "" {
		return nil, nil, session.Errorf(session.CodeInvalidArgument, "give either input or key, not both")
	}
	until, err := compile("until", in.Until)
	if err != nil {
		return nil, nil, wrapErr(err)
	}
	w := session.WaitOpts{Idle: ms(in.IdleMS), Timeout: timeout(in.WaitMS), Until: until}
	oo := s.m.OutputOpts()

	var res session.Result
	switch {
	case in.Key != "":
		kind, ok := keys[in.Key]
		if !ok {
			return nil, nil, session.Errorf(session.CodeInvalidArgument, "unknown key %q", in.Key)
		}
		var wait time.Duration
		if in.WaitMS > 0 {
			wait = w.Timeout
		}
		s.logf("[%s] <- key %s", sess.ID, in.Key)
		res, err = sess.Signal(ctx, kind, wait, oo)
	case in.Input == "":
		if in.NoWait {
			w.Timeout = 0
		}
		res, err = sess.Read(ctx, w, oo)
	default:
		s.logf("[%s] <- %q", sess.ID, in.Input)
		res, err = sess.Send(ctx, in.Input, session.SendOpts{Newline: !in.Raw, NoWait: in.NoWait, Wait: w, Output: oo})
		if in.NoWait && err == nil {
			return s.reply("ok", resultMap(res))
		}
	}
	if err != nil {
		return nil, nil, withOutput(err)
	}
	s.logf("[%s] -> %q", sess.ID, res.Output)
	return s.reply(text("", res), resultMap(res))
}

// withOutput appends remaining output to session_exited errors.
func withOutput(err error) error {
	var se *session.Error
	if errors.As(err, &se) {
		if se.Result != nil && se.Result.Output != "" {
			return session.Errorf(se.Code, "%s\n%s", se.Message, se.Result.Output)
		}
		return se
	}
	return session.Errorf(session.CodeInternal, "%v", err)
}

func (s *srv) kill(_ context.Context, _ *mcp.CallToolRequest, in KillIn) (*mcp.CallToolResult, any, error) {
	k, err := s.m.Kill(in.SessionID, 2*time.Second)
	if err != nil {
		return nil, nil, wrapErr(err)
	}
	s.logf("kill %s", in.SessionID)
	txt := "ok"
	if k.Final.Output != "" {
		txt = k.Final.Output
	}
	return s.reply(txt, map[string]any{"exit_code": k.Code, "exit_signal": k.Signal, "forced": k.Forced, "final_output": k.Final.Output})
}

func (s *srv) list(_ context.Context, _ *mcp.CallToolRequest, _ ListIn) (*mcp.CallToolResult, any, error) {
	infos := s.m.List()
	var b strings.Builder
	rows := make([]map[string]any, 0, len(infos))
	for _, i := range infos {
		state := i.State
		if i.ExitCode != nil {
			state = fmt.Sprintf("exited %d", *i.ExitCode)
		}
		fmt.Fprintf(&b, "%s %s %s", i.ID, i.Command, state)
		if i.UnreadBytes > 0 {
			fmt.Fprintf(&b, " unread=%dB", i.UnreadBytes)
		}
		b.WriteByte('\n')
		row := map[string]any{"session_id": i.ID, "command": i.Command, "args": i.Args, "pid": i.PID, "state": i.State,
			"unread_bytes": i.UnreadBytes, "tty_mode": i.Mode, "age_s": int64(i.Age.Seconds()), "idle_s": int64(i.Idle.Seconds())}
		if i.ExitCode != nil {
			row["exit_code"] = *i.ExitCode
		}
		rows = append(rows, row)
	}
	txt := strings.TrimRight(b.String(), "\n")
	if txt == "" {
		txt = "no sessions"
	}
	return s.reply(txt, map[string]any{"sessions": rows})
}
