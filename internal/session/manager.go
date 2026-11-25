package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// Config holds server-wide limits and policy.
type Config struct {
	MaxSessions   int
	IdleTTL       time.Duration // 0 disables reaping
	MaxBuffer     int           // unread bytes retained per session
	AllowCommands []string      // globs; empty = allow all
	CleanEnv      bool
}

// DefaultConfig returns the documented defaults.
func DefaultConfig() Config {
	return Config{MaxSessions: 16, IdleTTL: 30 * time.Minute, MaxBuffer: 4 << 20}
}

// LaunchParams describes a program to start.
type LaunchParams struct {
	Command     string
	Args        []string
	Cwd         string
	Env         map[string]string
	Preset      string
	PromptRegex string
	TTY         bool
	Rows, Cols  int
	StartupWait time.Duration
	StartupIdle time.Duration
}

// Manager owns all sessions.
type Manager struct {
	cfg      Config
	mu       sync.Mutex
	sessions map[string]*Session
	stop     chan struct{}
	once     sync.Once
}

// NewManager creates a manager and starts the idle reaper.
func NewManager(cfg Config) *Manager {
	m := &Manager{cfg: cfg, sessions: map[string]*Session{}, stop: make(chan struct{})}
	if cfg.IdleTTL > 0 {
		go m.reaper()
	}
	return m
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
				if s == nil {
					continue
				}
				s.mu.Lock()
				idle := time.Since(s.lastUsed)
				s.mu.Unlock()
				if idle > m.cfg.IdleTTL {
					stale = append(stale, s)
				}
			}
			m.mu.Unlock()
			for _, s := range stale {
				m.remove(s.ID)
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

// Launch starts a program and waits for its first prompt/banner.
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
	promptSrc := pre.Prompt
	if lp.PromptRegex != "" {
		promptSrc = lp.PromptRegex
	}
	var prompt *regexp.Regexp
	if promptSrc != "" {
		var err error
		if prompt, err = regexp.Compile(promptSrc); err != nil {
			return nil, Result{}, Errorf(CodeInvalidArgument, "invalid prompt_regex: %v", err)
		}
	}
	// A shell marker only works on a tty and when the caller did not override the prompt.
	useMarker := pre.Init != "" && lp.TTY && lp.PromptRegex == ""
	if pre.Init != "" && !useMarker {
		if lp.PromptRegex == "" {
			prompt = nil
		}
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

	m.mu.Lock()
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

	s, err := newSession(id, startSpec{
		command: lp.Command, args: args, dir: lp.Cwd, tty: lp.TTY, rows: rows, cols: cols,
		env: mergeEnv(baseEnv(m.cfg.CleanEnv), env),
	}, lp.Command, args, presetName, prompt, useMarker, m.cfg.MaxBuffer)
	if err != nil {
		m.remove(id)
		return nil, Result{}, Errorf(CodeSpawnFailed, "%v", err)
	}
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()

	oo := OutputOpts{StripANSI: true, MaxBytes: 65536}
	started := time.Now()
	startupWait := lp.StartupWait
	if startupWait <= 0 {
		startupWait = time.Second
	}
	if useMarker {
		if startupWait < 5*time.Second {
			startupWait = 5 * time.Second
		}
		start := s.snapshotTotal()
		_, _ = s.proc.Write([]byte(pre.Init + eol(true)))
		reason := s.await(ctx, WaitOpts{Timeout: startupWait, Prompt: prompt}, start)
		res := s.result(reason, "", prompt, oo, started)
		if reason == ReasonPrompt {
			res.Output = "" // discard init noise
		}
		return s, res, nil
	}
	w := s.effective(WaitOpts{Timeout: startupWait, Idle: 300 * time.Millisecond})
	reason := s.await(ctx, w, 0)
	return s, s.result(reason, "", w.Prompt, oo, started), nil
}

func (m *Manager) remove(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

// Get looks a session up by id.
func (m *Manager) Get(id string) (*Session, error) {
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil {
		return nil, Errorf(CodeNotFound, "no session with id %q", id)
	}
	return s, nil
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

// Kill terminates and removes a session.
func (m *Manager) Kill(id string, grace time.Duration) (int, bool, Result, error) {
	s, err := m.Get(id)
	if err != nil {
		return 0, false, Result{}, err
	}
	m.remove(id)
	code, forced, res := s.Kill(grace)
	return code, forced, res, nil
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
