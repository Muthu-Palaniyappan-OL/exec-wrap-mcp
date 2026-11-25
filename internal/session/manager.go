package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"sync"
	"time"
)

const maxTombstones = 64

// Config holds server-wide limits and policy.
type Config struct {
	MaxSessions   int
	IdleTTL       time.Duration // 0 disables reaping
	MaxBuffer     int           // unread bytes retained per session
	MaxOutput     int           // bytes returned per call
	AllowCommands []string      // globs; empty = allow all
	CleanEnv      bool
}

// DefaultConfig returns the documented defaults.
func DefaultConfig() Config {
	return Config{MaxSessions: 16, IdleTTL: 30 * time.Minute, MaxBuffer: 4 << 20, MaxOutput: 64 << 10}
}

// LaunchParams describes a program to start.
type LaunchParams struct {
	Command     string
	Args        []string
	Cwd         string
	Env         map[string]string
	Preset      string
	PromptRegex string
	ReadyRegex  string
	Input       string // optional first input, sent once the program is ready
	TTY         bool
	Rows, Cols  int
	Wait        time.Duration // how long to wait for readiness and the first response
}

// Manager owns all sessions.
type Manager struct {
	cfg      Config
	mu       sync.Mutex
	sessions map[string]*Session
	closed   map[string]string // tombstones: id -> why it is gone
	closedQ  []string
	stop     chan struct{}
	once     sync.Once
}

// NewManager creates a manager and starts the idle reaper.
func NewManager(cfg Config) *Manager {
	if cfg.MaxOutput <= 0 {
		cfg.MaxOutput = 64 << 10
	}
	m := &Manager{cfg: cfg, sessions: map[string]*Session{}, closed: map[string]string{}, stop: make(chan struct{})}
	if cfg.IdleTTL > 0 {
		go m.reaper()
	}
	return m
}

// OutputOpts is the output rendering used for tool results.
func (m *Manager) OutputOpts() OutputOpts {
	return OutputOpts{StripANSI: true, MaxBytes: m.cfg.MaxOutput}
}

// buryLocked records why a session id is gone. The caller holds m.mu.
func (m *Manager) buryLocked(id, why string) {
	if _, ok := m.closed[id]; !ok {
		m.closedQ = append(m.closedQ, id)
		if len(m.closedQ) > maxTombstones {
			delete(m.closed, m.closedQ[0])
			m.closedQ = m.closedQ[1:]
		}
	}
	m.closed[id] = why
}

func (m *Manager) reaper() {
	every := m.cfg.IdleTTL / 4
	if every > 30*time.Second {
		every = 30 * time.Second
	}
	if every < 50*time.Millisecond {
		every = 50 * time.Millisecond
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			var stale []*Session
			m.mu.Lock()
			for _, s := range m.sessions {
				if s == nil || s.isBusy() { // never reap a session mid-call
					continue
				}
				s.mu.Lock()
				idle := time.Since(s.lastUsed)
				s.mu.Unlock()
				if idle > m.cfg.IdleTTL {
					stale = append(stale, s)
				}
			}
			for _, s := range stale {
				delete(m.sessions, s.ID)
				m.buryLocked(s.ID, fmt.Sprintf("was closed after %s without use; its state is lost, launch a new one", m.cfg.IdleTTL))
			}
			m.mu.Unlock()
			for _, s := range stale {
				go s.Kill(time.Second)
			}
		}
	}
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "s_" + hex.EncodeToString(b)
}

func (m *Manager) allowed(command string) error {
	if len(m.cfg.AllowCommands) == 0 {
		return nil
	}
	abs := command
	if p, err := exec.LookPath(command); err == nil {
		if a, err := filepath.Abs(p); err == nil {
			abs = a
		}
	}
	for _, g := range m.cfg.AllowCommands {
		for _, cand := range []string{abs, command, filepath.ToSlash(abs)} {
			if ok, _ := path.Match(filepath.ToSlash(g), filepath.ToSlash(cand)); ok {
				return nil
			}
		}
	}
	return Errorf(CodeNotAllowed, "command %q is not permitted by --allow-command", command)
}

func baseEnv(clean bool) []string {
	if !clean {
		return os.Environ()
	}
	keep := []string{"PATH", "HOME", "USER", "LANG", "TERM"}
	if runtime.GOOS == "windows" {
		keep = append(keep, "SystemRoot", "ComSpec", "USERPROFILE", "TEMP", "TMP", "PATHEXT", "USERNAME")
	}
	var out []string
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// evictExitedLocked removes the oldest exited session to free a slot.
func (m *Manager) evictExitedLocked() bool {
	var victim *Session
	for _, s := range m.sessions {
		if s != nil && s.Exited() && (victim == nil || s.created.Before(victim.created)) {
			victim = s
		}
	}
	if victim == nil {
		return false
	}
	victim.mu.Lock()
	code := victim.exitCode
	victim.mu.Unlock()
	delete(m.sessions, victim.ID)
	m.buryLocked(victim.ID, fmt.Sprintf("had exited (code %d) and was cleaned up to free a slot", code))
	return true
}

// Launch starts a program and waits until it is ready (or the wait elapses).
func (m *Manager) Launch(ctx context.Context, lp LaunchParams) (*Session, Result, error) {
	if lp.Command == "" {
		return nil, Result{}, Errorf(CodeInvalidArgument, "command is required")
	}
	if err := m.allowed(lp.Command); err != nil {
		return nil, Result{}, err
	}
	presetName := lp.Preset
	if presetName == "" {
		presetName = InferPreset(lp.Command)
	}
	pre, ok := lookupPreset(presetName)
	if !ok {
		return nil, Result{}, Errorf(CodeInvalidArgument, "unknown preset %q (valid: %v)", presetName, PresetNames())
	}
	compile := func(name, src string) (*regexp.Regexp, error) {
		if src == "" {
			return nil, nil
		}
		re, err := regexp.Compile(src)
		if err != nil {
			return nil, Errorf(CodeInvalidArgument, "invalid %s: %v", name, err)
		}
		return re, nil
	}
	prompt, err := compile("prompt_regex", firstNonEmpty(lp.PromptRegex, pre.Prompt))
	if err != nil {
		return nil, Result{}, err
	}
	ready, err := compile("ready_regex", lp.ReadyRegex)
	if err != nil {
		return nil, Result{}, err
	}
	// A shell marker only works on a tty and when the caller did not override the prompt.
	useMarker := pre.Init != "" && lp.TTY && lp.PromptRegex == ""
	if pre.Init != "" && !useMarker && lp.PromptRegex == "" {
		prompt = nil
	}
	if useMarker || ready == nil {
		ready = prompt // readiness = first prompt
	}

	args := lp.Args
	if len(args) == 0 && useMarker {
		args = pre.DefaultArgs
	}
	env := map[string]string{"TERM": "dumb", "PAGER": "cat", "GIT_PAGER": "cat"}
	for k, v := range pre.Env {
		env[k] = v
	}
	for k, v := range lp.Env {
		env[k] = v
	}
	rows, cols := lp.Rows, lp.Cols
	if rows <= 0 {
		rows = 40
	}
	if cols <= 0 {
		cols = 200
	}
	wait := lp.Wait
	if wait <= 0 {
		wait = 30 * time.Second
	}

	m.mu.Lock()
	for len(m.sessions) >= m.cfg.MaxSessions && m.evictExitedLocked() {
	}
	if len(m.sessions) >= m.cfg.MaxSessions {
		m.mu.Unlock()
		return nil, Result{}, Errorf(CodeLimitReached, "maximum of %d sessions reached; kill one first", m.cfg.MaxSessions)
	}
	id := newID()
	for m.sessions[id] != nil {
		id = newID()
	}
	m.sessions[id] = nil // reserve the slot
	m.mu.Unlock()

	initText := ""
	if useMarker {
		initText = pre.Init
	}
	s, err := newSession(startSpec{
		command: lp.Command, args: args, dir: lp.Cwd, tty: lp.TTY, rows: rows, cols: cols,
		env: mergeEnv(baseEnv(m.cfg.CleanEnv), env),
	}, sessionSpec{
		id: id, command: lp.Command, args: args, preset: presetName, prompt: prompt, ready: ready,
		initText: initText, marker: useMarker, maxBuf: m.cfg.MaxBuffer,
	})
	if err != nil {
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
		return nil, Result{}, Errorf(CodeSpawnFailed, "%v", err)
	}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()

	oo := m.OutputOpts()
	started := time.Now()
	s.opMu.Lock() // keeps the reaper and other calls away while we wait for startup
	unlocked := false
	unlock := func() {
		if !unlocked {
			unlocked = true
			s.touch()
			s.opMu.Unlock()
		}
	}
	defer unlock()
	if useMarker {
		_, _ = s.proc.Write([]byte(pre.Init + eol(true)))
	}
	if ready == nil { // no way to detect a prompt: take the banner once output goes quiet
		reason := s.await(ctx, WaitOpts{Timeout: wait, Idle: defaultIdle}, 0)
		if lp.Input != "" && reason != ReasonExited {
			unlock()
			res, err := s.Send(ctx, lp.Input, SendOpts{Newline: true, Wait: WaitOpts{Timeout: wait}, Output: oo})
			return s, res, err
		}
		return s, s.result(reason, "", nil, oo, started), nil
	}
	if res, ok := s.ensureReady(ctx, wait, oo, started); !ok {
		return s, res, nil
	}
	if lp.Input != "" {
		unlock()
		res, err := s.Send(ctx, lp.Input, SendOpts{Newline: true, Wait: WaitOpts{Timeout: wait}, Output: oo})
		return s, res, err
	}
	return s, s.result(ReasonPrompt, "", prompt, oo, started), nil
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

// Get looks a session up by id. Ids of sessions that existed but are gone
// produce an error that says what happened to them.
func (m *Manager) Get(id string) (*Session, error) {
	m.mu.Lock()
	s := m.sessions[id]
	why, gone := m.closed[id]
	m.mu.Unlock()
	if s != nil {
		return s, nil
	}
	if gone {
		return nil, Errorf(CodeSessionClosed, "session %s %s", id, why)
	}
	return nil, Errorf(CodeNotFound, "no session with id %q (use list to see sessions)", id)
}

// List returns all sessions sorted by id.
func (m *Manager) List() []Info {
	m.mu.Lock()
	ss := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		if s != nil {
			ss = append(ss, s)
		}
	}
	m.mu.Unlock()
	out := make([]Info, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.Info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// KillResult is the outcome of ending a session.
type KillResult struct {
	Code   int
	Signal string
	Forced bool
	Final  Result
}

// Kill terminates and removes a session.
func (m *Manager) Kill(id string, grace time.Duration) (KillResult, error) {
	s, err := m.Get(id)
	if err != nil {
		return KillResult{}, err
	}
	m.mu.Lock()
	delete(m.sessions, id)
	m.buryLocked(id, "was ended by kill; launch a new one")
	m.mu.Unlock()
	code, sig, forced, res := s.Kill(grace)
	return KillResult{Code: code, Signal: sig, Forced: forced, Final: res}, nil
}

// Close kills every session and stops the reaper.
func (m *Manager) Close() {
	m.once.Do(func() { close(m.stop) })
	m.mu.Lock()
	ss := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		if s != nil {
			ss = append(ss, s)
		}
	}
	m.sessions = map[string]*Session{}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range ss {
		wg.Add(1)
		go func(s *Session) { defer wg.Done(); s.Kill(500 * time.Millisecond) }(s)
	}
	wg.Wait()
}
