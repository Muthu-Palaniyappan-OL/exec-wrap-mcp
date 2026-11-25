package session

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Done reasons reported by wait operations.
const (
	ReasonPrompt  = "prompt"
	ReasonIdle    = "idle"
	ReasonUntil   = "until"
	ReasonTimeout = "timeout"
	ReasonExited  = "exited"
	ReasonPending = "pending" // read with no waiting
	ReasonNoWait  = "no_wait"
)

const (
	promptQuiet       = 25 * time.Millisecond // quiet time required after a prompt match
	defaultIdle       = 300 * time.Millisecond
	defaultPromptIdle = 5 * time.Second // safety net when a prompt is known
	tailBytes         = 8192
)

// WaitOpts controls when a send/read call returns.
type WaitOpts struct {
	Idle    time.Duration // 0 = default
	Timeout time.Duration // <=0 = do not wait
	Prompt  *regexp.Regexp
	Until   *regexp.Regexp
}

// OutputOpts controls how output is rendered.
type OutputOpts struct {
	StripANSI bool
	MaxBytes  int
}

// Result is the outcome of a send/read/signal call.
type Result struct {
	Output         string
	Reason         string
	Exited         bool
	ExitCode       *int
	LastExitStatus *int
	Truncated      bool
	DroppedBytes   int
	Elapsed        time.Duration
}

// Info is a snapshot of a session for listings.
type Info struct {
	ID          string
	Command     string
	Args        []string
	PID         int
	Mode        string
	Preset      string
	State       string
	ExitCode    *int
	Age         time.Duration
	Idle        time.Duration
	UnreadBytes int
}

// Session is one long-lived child process with buffered output.
type Session struct {
	ID      string
	Command string
	Args    []string
	Preset  string

	proc    process
	prompt  *regexp.Regexp
	marker  bool
	tty     bool
	created time.Time
	maxBuf  int

	opMu sync.Mutex // serialises send/read/signal; TryLock => "busy"

	mu       sync.Mutex
	buf      []byte
	dropped  int
	total    uint64
	lastByte time.Time
	lastUsed time.Time
	exited   bool
	exitCode int
	notify   chan struct{}

	readDone chan struct{}
	waitDone chan struct{}
}

func newSession(id string, sp startSpec, command string, args []string, preset string, prompt *regexp.Regexp, marker bool, maxBuf int) (*Session, error) {
	p, err := startProcess(sp)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	s := &Session{
		ID: id, Command: command, Args: args, Preset: preset,
		proc: p, prompt: prompt, marker: marker, tty: p.Mode() != "pipe",
		created: now, lastUsed: now, maxBuf: maxBuf,
		notify:   make(chan struct{}, 1),
		readDone: make(chan struct{}),
		waitDone: make(chan struct{}),
	}
	go s.readLoop()
	go s.waitLoop()
	return s, nil
}

func (s *Session) poke() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *Session) readLoop() {
	defer close(s.readDone)
	chunk := make([]byte, 32*1024)
	for {
		n, err := s.proc.Read(chunk)
		if n > 0 {
			s.mu.Lock()
			s.buf = append(s.buf, chunk[:n]...)
			if over := len(s.buf) - s.maxBuf; over > 0 {
				s.buf = append([]byte(nil), s.buf[over:]...)
				s.dropped += over
			}
			s.total += uint64(n)
			s.lastByte = time.Now()
			s.mu.Unlock()
			s.poke()
		}
		if err != nil {
			return
		}
	}
}

func (s *Session) waitLoop() {
	defer close(s.waitDone)
	code, _ := s.proc.Wait()
	s.proc.Drain()
	select {
	case <-s.readDone:
	case <-time.After(300 * time.Millisecond):
	}
	_ = s.proc.Close()
	select {
	case <-s.readDone:
	case <-time.After(200 * time.Millisecond):
	}
	s.mu.Lock()
	s.exited, s.exitCode = true, code
	s.mu.Unlock()
	s.poke()
}

func (s *Session) touch() {
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

// Info returns a snapshot of the session.
func (s *Session) Info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	in := Info{
		ID: s.ID, Command: s.Command, Args: s.Args, PID: s.proc.PID(), Mode: s.proc.Mode(),
		Preset: s.Preset, State: "running", Age: time.Since(s.created),
		Idle: time.Since(s.lastUsed), UnreadBytes: len(s.buf),
	}
	if s.exited {
		in.State = "exited"
		c := s.exitCode
		in.ExitCode = &c
	}
	return in
}

// PID of the child.
func (s *Session) PID() int { return s.proc.PID() }

// Mode is pty, conpty or pipe.
func (s *Session) Mode() string { return s.proc.Mode() }

// PromptPattern returns the effective prompt regexp ("" if none).
func (s *Session) PromptPattern() string {
	if s.prompt == nil {
		return ""
	}
	return s.prompt.String()
}

// Exited reports whether the child has exited.
func (s *Session) Exited() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exited
}

func (s *Session) lock() error {
	if !s.opMu.TryLock() {
		return Errorf(CodeBusy, "session %s is busy with another call", s.ID)
	}
	return nil
}

func (s *Session) snapshotTotal() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

// await blocks until a completion condition holds and returns the reason.
func (s *Session) await(ctx context.Context, o WaitOpts, startTotal uint64) string {
	start := time.Now()
	idle := o.Idle
	if idle <= 0 {
		if o.Prompt != nil {
			idle = defaultPromptIdle
		} else {
			idle = defaultIdle
		}
	}
	var deadline time.Time
	if o.Timeout > 0 {
		deadline = start.Add(o.Timeout)
	}
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		s.mu.Lock()
		exited := s.exited
		last := s.lastByte
		fresh := s.total > startTotal
		var tail, all []byte
		if n := len(s.buf); n > 0 {
			from := 0
			if n > tailBytes {
				from = n - tailBytes
			}
			tail = append([]byte(nil), s.buf[from:]...)
			if o.Until != nil {
				all = append([]byte(nil), s.buf...)
			}
		}
		s.mu.Unlock()

		if exited {
			return ReasonExited
		}
		if o.Timeout <= 0 {
			return ReasonPending
		}
		now := time.Now()
		if last.Before(start) {
			last = start
		}
		wake := start.Add(idle)
		if t := last.Add(idle); t.After(wake) {
			wake = t
		}
		if fresh && o.Until != nil && o.Until.MatchString(Clean(all, true)) {
			return ReasonUntil
		}
		if fresh && o.Prompt != nil && o.Prompt.MatchString(Clean(tail, true)) {
			ready := last.Add(promptQuiet)
			if !now.Before(ready) {
				return ReasonPrompt
			}
			if ready.Before(wake) {
				wake = ready
			}
		}
		if !now.Before(last.Add(idle)) {
			return ReasonIdle
		}
		if !deadline.IsZero() {
			if !now.Before(deadline) {
				return ReasonTimeout
			}
			if deadline.Before(wake) {
				wake = deadline
			}
		}
		timer.Reset(time.Until(wake))
		select {
		case <-s.notify:
		case <-timer.C:
		case <-ctx.Done():
			return ReasonTimeout
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}

func (s *Session) take() ([]byte, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, d := s.buf, s.dropped
	s.buf, s.dropped = nil, 0
	return b, d
}

func (s *Session) result(reason, input string, prompt *regexp.Regexp, oo OutputOpts, started time.Time) Result {
	raw, dropped := s.take()
	text := Clean(raw, oo.StripANSI)
	r := Result{Reason: reason, DroppedBytes: dropped, Elapsed: time.Since(started)}
	if s.marker {
		if ms := markerRe.FindAllStringSubmatch(text, -1); len(ms) > 0 {
			if n, err := strconv.Atoi(ms[len(ms)-1][1]); err == nil {
				r.LastExitStatus = &n
			}
			text = markerRe.ReplaceAllString(text, "")
		}
	}
	if reason == ReasonPrompt && prompt != nil {
		if loc := prompt.FindStringIndex(text); loc != nil && loc[1] == len(text) {
			text = text[:loc[0]]
		}
	}
	if input != "" && s.tty && !s.marker { // marker shells run with echo off
		text = stripEcho(text, input)
	}
	text = strings.TrimRight(text, "\n")
	r.Output, r.Truncated = Truncate(text, oo.MaxBytes)
	s.mu.Lock()
	if s.exited {
		r.Exited = true
		c := s.exitCode
		r.ExitCode = &c
	}
	s.mu.Unlock()
	return r
}

// stripEcho removes leading lines that merely echo the submitted input.
func stripEcho(out, input string) string {
	for _, in := range strings.Split(strings.TrimRight(input, "\r\n"), "\n") {
		in = strings.TrimRight(in, "\r")
		if in == "" {
			continue
		}
		line, rest, found := strings.Cut(out, "\n")
		if strings.HasSuffix(strings.TrimRight(line, " "), in) {
			if found {
				out = rest
			} else {
				out = ""
			}
			continue
		}
		break
	}
	return out
}

// SendOpts are the parameters for Send.
type SendOpts struct {
	Newline bool
	NoWait  bool
	Wait    WaitOpts
	Output  OutputOpts
}

func (s *Session) effective(o WaitOpts) WaitOpts {
	if o.Prompt == nil {
		o.Prompt = s.prompt
	}
	return o
}

// Send writes input to the process and waits for the response.
func (s *Session) Send(ctx context.Context, input string, o SendOpts) (Result, error) {
	if err := s.lock(); err != nil {
		return Result{}, err
	}
	defer s.opMu.Unlock()
	s.touch()
	if s.Exited() {
		r := s.result(ReasonExited, "", nil, o.Output, time.Now())
		return r, Errorf(CodeSessionExited, "session %s has exited (code %d)", s.ID, *r.ExitCode).withResult(&r)
	}
	started := time.Now()
	if o.Newline && !strings.HasSuffix(input, "\n") {
		input += eol(s.tty)
	}
	startTotal := s.snapshotTotal()
	if _, err := s.proc.Write([]byte(input)); err != nil {
		if s.Exited() {
			r := s.result(ReasonExited, "", nil, o.Output, started)
			return r, Errorf(CodeSessionExited, "session %s has exited", s.ID).withResult(&r)
		}
		return Result{}, Errorf(CodeInternal, "write failed: %v", err)
	}
	if o.NoWait {
		return Result{Reason: ReasonNoWait, Elapsed: time.Since(started)}, nil
	}
	w := s.effective(o.Wait)
	reason := s.await(ctx, w, startTotal)
	return s.result(reason, input, w.Prompt, o.Output, started), nil
}

// Read returns pending output, optionally waiting for more.
func (s *Session) Read(ctx context.Context, w WaitOpts, oo OutputOpts) (Result, error) {
	if err := s.lock(); err != nil {
		return Result{}, err
	}
	defer s.opMu.Unlock()
	s.touch()
	started := time.Now()
	w = s.effective(w)
	reason := s.await(ctx, w, s.snapshotTotal())
	return s.result(reason, "", w.Prompt, oo, started), nil
}

// Signal kinds.
const (
	SigInterrupt = "interrupt"
	SigTerminate = "terminate"
	SigKill      = "kill"
	SigEOF       = "eof"
	SigSuspend   = "suspend"
)

// Signal delivers a signal-like action and collects the resulting output.
func (s *Session) Signal(ctx context.Context, kind string, oo OutputOpts) (Result, error) {
	if err := s.lock(); err != nil {
		return Result{}, err
	}
	defer s.opMu.Unlock()
	s.touch()
	started := time.Now()
	startTotal := s.snapshotTotal()
	var err error
	switch kind {
	case SigInterrupt:
		err = s.proc.Interrupt()
	case SigTerminate:
		err = s.proc.Terminate()
	case SigKill:
		err = s.proc.Kill()
	case SigEOF:
		err = s.proc.EOF()
	case SigSuspend:
		err = s.proc.Suspend()
	default:
		return Result{}, Errorf(CodeInvalidArgument, "unknown signal %q", kind)
	}
	if err != nil {
		if err == errUnsupported {
			return Result{}, Errorf(CodeUnsupported, "signal %q is not supported for this session", kind)
		}
		if s.Exited() {
			return Result{}, Errorf(CodeSessionExited, "session %s has exited", s.ID)
		}
		return Result{}, Errorf(CodeInternal, "signal failed: %v", err)
	}
	w := s.effective(WaitOpts{Idle: 300 * time.Millisecond, Timeout: 3 * time.Second})
	reason := s.await(ctx, w, startTotal)
	return s.result(reason, "", w.Prompt, oo, started), nil
}

// Resize changes the terminal size.
func (s *Session) Resize(rows, cols int) error {
	if err := s.proc.Resize(rows, cols); err != nil {
		if err == errUnsupported {
			return Errorf(CodeUnsupported, "resize is not supported in %s mode", s.proc.Mode())
		}
		return Errorf(CodeInternal, "resize failed: %v", err)
	}
	return nil
}

// Kill terminates the process (gracefully first) and returns its exit code.
func (s *Session) Kill(grace time.Duration) (code int, forced bool, final Result) {
	started := time.Now()
	if !s.Exited() {
		_ = s.proc.Terminate()
		select {
		case <-s.waitDone:
		case <-time.After(grace):
			forced = true
			_ = s.proc.Kill()
			select {
			case <-s.waitDone:
			case <-time.After(3 * time.Second):
			}
		}
	}
	s.opMu.Lock() // an in-flight call returns once the process is gone
	defer s.opMu.Unlock()
	final = s.result(ReasonExited, "", nil, OutputOpts{StripANSI: true, MaxBytes: 65536}, started)
	s.mu.Lock()
	code = s.exitCode
	s.mu.Unlock()
	return code, forced, final
}
