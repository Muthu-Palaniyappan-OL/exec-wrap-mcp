// Command exec-wrap-mcp is an MCP server that keeps interactive programs
// (shells, debuggers, REPLs) alive as persistent sessions.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Muthu-Palaniyappan-OL/exec-wrap-mcp/internal/server"
	"github.com/Muthu-Palaniyappan-OL/exec-wrap-mcp/internal/session"
)

// Set by goreleaser via -ldflags.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	cfg := session.DefaultConfig()
	var allow multiFlag
	var showVersion, logIO bool
	flag.IntVar(&cfg.MaxSessions, "max-sessions", cfg.MaxSessions, "maximum number of concurrent sessions")
	flag.DurationVar(&cfg.IdleTTL, "idle-ttl", cfg.IdleTTL, "kill sessions unused for this long (0 disables)")
	flag.IntVar(&cfg.MaxBuffer, "max-buffer", cfg.MaxBuffer, "unread output bytes retained per session")
	flag.Var(&allow, "allow-command", "glob of permitted executables (repeatable; default: allow all)")
	flag.BoolVar(&cfg.CleanEnv, "clean-env", false, "pass only a minimal environment (PATH, HOME, USER, LANG, TERM) to children")
	flag.BoolVar(&logIO, "log-io", false, "log session input/output to stderr (may expose secrets)")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "exec-wrap-mcp %s: MCP server (stdio) for persistent interactive sessions.\n\nUsage: exec-wrap-mcp [flags]\n\n", version)
		flag.PrintDefaults()
	}
	flag.Parse()
	cfg.AllowCommands = allow
	if version == "dev" { // `go install ...@vX.Y.Z` embeds the module version
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			version = bi.Main.Version
		}
	}

	if showVersion {
		fmt.Printf("exec-wrap-mcp %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	logger := log.New(os.Stderr, "exec-wrap-mcp: ", log.LstdFlags)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	m := session.NewManager(cfg)
	defer m.Close()
	s := server.New(m, server.Options{Version: version, LogIO: logIO, Logger: logger})

	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, &mcp.StdioTransport{}) }()
	select {
	case err := <-done:
		if err != nil && ctx.Err() == nil {
			logger.Printf("server stopped: %v", err)
		}
	case <-ctx.Done():
	}
	m.Close()
}
