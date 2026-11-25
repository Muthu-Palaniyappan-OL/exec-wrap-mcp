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
	ReasonPrompt   = "prompt"
	ReasonIdle     = "idle"
	ReasonUntil    = "until"
	ReasonTimeout  = "timeout"
	ReasonExited   = "exited"
	ReasonPending  = "pending" // read with no waiting
	ReasonNoWait   = "no_wait"
	ReasonStarting = "starting" // the program has not reached its first prompt yet
)

const (
	promptQuiet       = 25 * time.Millisecond // quiet time required after a prompt match
	defaultIdle       = 300 * time.Millisecond
	defaultPromptIdle = 5 * time.Second // safety net when a prompt is known
	startIdleFallback = 3 * time.Second // quiet period after which a started-but-unrecognised program counts as ready
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
	ExitSignal     string
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
	State       string // starting, running or exited
	ExitCode    *int
	ExitSignal  string
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

	proc     process
	prompt   *regexp.Regexp
	readyRe  *regexp.Regexp // non-nil while the program may still be loading
	initText string         // shell setup line, scrubbed from output
	marker   bool
	tty      bool
	created  time.Time
	maxBuf   int

	opMu sync.Mutex // serialises send/read/signal; TryLock => "busy"

	mu          sync.Mutex
	buf         []byte
	dropped     int
	total       uint64
	lastByte    time.Time
	lastUsed    time.Time
	starting    bool
	startWaited bool
	preamble    string // banner captured before the first prompt
	exited      bool
	exitCode    int
	exitSig     string
	notify      chan struct{}

	readDone chan struct{}
	waitDone chan struct{}
}

type sessionSpec struct {
	id, command string
	args        []string
	preset      string
	prompt      *regexp.Regexp
	ready       *regexp.Regexp
	initText    string
	marker      bool
	maxBuf      int
}

func newSession(sp startSpec, ss sessionSpec) (*Session, error) {
	p, err := startProcess(sp)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	s := &Session{
		ID: ss.id, Command: ss.command, Args: ss.args, Preset: ss.preset,
		proc: p, prompt: ss.prompt, readyRe: ss.ready, initText: ss.initText,
		marker: ss.marker, tty: p.Mode() != "pipe",
		created: now, lastUsed: now, maxBuf: ss.maxBuf, starting: ss.ready != nil,
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
			if s.starting {
				s.checkReadyLocked()
			}
			s.mu.Unlock()
			s.poke()
		}
		if err != nil {
			return
		}
	}
}

// checkReadyLocked ends the starting state when the ready/prompt pattern
// shows up at the end of the output. The caller holds s.mu.
func (s *Session) checkReadyLocked() {
	from := 0
	if n := len(s.buf); n > tailBytes {
		from = n - tailBytes
	}
	if s.readyRe.MatchString(Clean(s.buf[from:], true)) {
		s.finishStartLocked()
	}
}

// finishStartLocked leaves the starting state and keeps any banner as the
// preamble of the next result. Shell setup noise is discarded.
func (s *Session) finishStartLocked() {
	s.starting = false
	text := Clean(s.buf, true)
	if s.marker {
		text = ""
	} else if loc := s.readyRe.FindStringIndex(text); loc != nil && loc[1] == len(text) {
		text = text[:loc[0]]
	}
	if s.initText != "" {
		text = strings.ReplaceAll(text, s.initText, "")
	}
	s.preamble = strings.TrimSpace(text)
	s.buf, s.dropped = nil, 0
}

func (s *Session) waitLoop() {
	defer close(s.waitDone)
	code, sig := s.proc.Wait()
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
	s.exited, s.exitCode, s.exitSig = true, code, sig
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
	if s.starting {
		in.State = "starting"
	}
	if s.exited {
		in.State = "exited"
		c := s.exitCode
		in.ExitCode = &c
		in.ExitSignal = s.exitSig
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

func (s *Session) isBusy() bool {
	if !s.opMu.TryLock() {
		return true
	}
	s.opMu.Unlock()
	return false
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

// waitReady blocks while the session is starting. It returns ReasonPrompt
// when the program is ready, ReasonExited if it died, or ReasonStarting when
// the timeout elapsed first. After one timed-out attempt, a quiet program is
// also treated as ready so a wrong prompt pattern cannot wedge the session.
func (s *Session) waitReady(ctx context.Context, timeout time.Duration) string {
	s.mu.Lock()
	fallback := s.startWaited
	s.mu.Unlock()
	start := time.Now()
	deadline := start.Add(timeout)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		s.mu.Lock()
		if !s.starting {
			s.mu.Unlock()
			return ReasonPrompt
		}
		if s.exited {
			s.mu.Unlock()
			return ReasonExited
		}
		last := s.lastByte
		if last.Before(start) {
			last = start
		}
		now := time.Now()
		// Only a program that already printed something (so it is alive and
		// its prompt pattern is probably wrong) may be declared ready by silence.
		fallback := fallback && s.total > 0
		if fallback && !now.Before(last.Add(startIdleFallback)) {
			s.finishStartLocked()
			s.mu.Unlock()
			return ReasonPrompt
		}
		if !now.Before(deadline) {
			s.startWaited = true
			s.mu.Unlock()
			return ReasonStarting
		}
		wake := deadline
		if fallback && last.Add(startIdleFallback).Before(wake) {
			wake = last.Add(startIdleFallback)
		}
		s.mu.Unlock()
		timer.Reset(time.Until(wake))
		select {
		case <-s.notify:
		case <-timer.C:
		case <-ctx.Done():
			s.mu.Lock()
			s.startWaited = true
			s.mu.Unlock()
			return ReasonStarting
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}

// ensureReady waits for startup to finish. When it returns false the Result
// explains why the caller should stop (still starting, or exited).
func (s *Session) ensureReady(ctx context.Context, timeout time.Duration, oo OutputOpts, started time.Time) (Result, bool) {
	s.mu.Lock()
	starting := s.starting
	s.mu.Unlock()
	if !starting {
		return Result{}, true
	}
	switch s.waitReady(ctx, timeout) {
	case ReasonPrompt:
		return Result{}, true
	case ReasonExited:
		return s.result(ReasonExited, "", nil, oo, started), false
	}
	return s.result(ReasonStarting, "", nil, oo, started), false
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
		wake := last.Add(idle)
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
	if s.initText != "" {
		text = strings.ReplaceAll(text, s.initText, "")
	}
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
	s.mu.Lock()
	if s.preamble != "" {
		if text == "" {
			text = s.preamble
		} else {
			text = s.preamble + "\n" + text
		}
		s.preamble = ""
	}
	if s.exited {
		r.Exited = true
		c := s.exitCode
		r.ExitCode = &c
		r.ExitSignal = s.exitSig
	}
	s.mu.Unlock()
	r.Output, r.Truncated = Truncate(text, oo.MaxBytes)
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

// Send writes input to the process and waits for the response. If the
// program is still starting, it waits for readiness first and does not write
// anything when startup does not finish in time.
func (s *Session) Send(ctx context.Context, input string, o SendOpts) (Result, error) {
	if err := s.lock(); err != nil {
		return Result{}, err
	}
	defer s.opMu.Unlock()
	s.touch()
	started := time.Now()
	if s.Exited() {
		r := s.result(ReasonExited, "", nil, o.Output, started)
		return r, Errorf(CodeSessionExited, "session %s has exited (code %d)", s.ID, *r.ExitCode).withResult(&r)
	}
	if r, ready := s.ensureReady(ctx, o.Wait.Timeout, o.Output, started); !ready {
		return r, nil
	}
	if o.Wait.Timeout > 0 { // charge startup time against the caller's budget
		if left := o.Wait.Timeout - time.Since(started); left > time.Second {
			o.Wait.Timeout = left
		} else {
			o.Wait.Timeout = time.Second
		}
	}
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

// Read returns pending output, waiting for more up to w.Timeout (a Timeout
// of zero returns immediately). Output that is already buffered counts.
func (s *Session) Read(ctx context.Context, w WaitOpts, oo OutputOpts) (Result, error) {
	if err := s.lock(); err != nil {
		return Result{}, err
	}
	defer s.opMu.Unlock()
	s.touch()
	started := time.Now()
	if w.Timeout > 0 {
		if r, ready := s.ensureReady(ctx, w.Timeout, oo, started); !ready {
			return r, nil
		}
	}
	w = s.effective(w)
	reason := s.await(ctx, w, 0)
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
func (s *Session) Signal(ctx context.Context, kind string, wait time.Duration, oo OutputOpts) (Result, error) {
	if err := s.lock(); err != nil {
		return Result{}, err
	}
	defer s.opMu.Unlock()
	s.touch()
	started := time.Now()
	if s.Exited() {
		return Result{}, Errorf(CodeSessionExited, "session %s has already exited", s.ID)
	}
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
		return Result{}, Errorf(CodeInvalidArgument, "unknown key %q", kind)
	}
	if err != nil {
		if err == errUnsupported {
			return Result{}, Errorf(CodeUnsupported, "%s is not supported for this session", kind)
		}
		if s.Exited() {
			return Result{}, Errorf(CodeSessionExited, "session %s has already exited", s.ID)
		}
		return Result{}, Errorf(CodeInternal, "signal failed: %v", err)
	}
	if wait <= 0 {
		wait = 3 * time.Second
	}
	w := s.effective(WaitOpts{Idle: 300 * time.Millisecond, Timeout: wait})
	reason := s.await(ctx, w, startTotal)
	return s.result(reason, "", w.Prompt, oo, started), nil
}

// Kill terminates the process (gracefully first) and returns its exit status.
func (s *Session) Kill(grace time.Duration) (code int, sig string, forced bool, final Result) {
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
	code, sig = s.exitCode, s.exitSig
	s.mu.Unlock()
	return code, sig, forced, final
}
