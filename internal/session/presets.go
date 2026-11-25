package session

import (
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Preset describes how to recognise "the program is ready for input".
type Preset struct {
	Name   string
	Prompt string // regexp matched against the cleaned tail of the output
	// Init, when set, is written right after launch to install a
	// collision-proof prompt marker (shells only).
	Init string
	// DefaultArgs are used when the caller passes no args.
	DefaultArgs []string
	Env         map[string]string
}

const markerPrompt = "\x1eewm:(\\d+)\x1e\\z"

// markerRe matches the marker prompt anywhere (used to strip it from output).
var markerRe = regexp.MustCompile("\x1eewm:(\\d+)\x1e")

const posixInit = `stty -echo 2>/dev/null; PS1="$(printf '\036')ewm:\$?$(printf '\036')"; PS2=''; PROMPT_COMMAND=''`

var presets = map[string]Preset{
	"bash": {Name: "bash", Prompt: markerPrompt, Init: posixInit, DefaultArgs: []string{"--noediting"}},
	"sh":   {Name: "sh", Prompt: markerPrompt, Init: posixInit},
	"zsh": {Name: "zsh", Prompt: markerPrompt, DefaultArgs: []string{"-o", "no_zle"},
		Init: `stty -echo 2>/dev/null; PROMPT=$'\036ewm:%?\036'; RPROMPT=''; PS2=''; precmd_functions=()`},
	"python": {Name: "python", Prompt: `(?:^|\n)(?:>>> |\.\.\. )\z`,
		Env: map[string]string{"PYTHON_BASIC_REPL": "1", "PYTHONUNBUFFERED": "1"}},
	"gdb":        {Name: "gdb", Prompt: `\(gdb\) \z`},
	"lldb":       {Name: "lldb", Prompt: `\(lldb\) \z`},
	"psql":       {Name: "psql", Prompt: `(?m)^\S*[=\-(!'"$][#>] \z`},
	"node":       {Name: "node", Prompt: `(?:^|\n)(?:> |\.\.\. )\z`},
	"cmd":        {Name: "cmd", Prompt: `(?m)^[A-Za-z]:\\[^\n>]*>\z`},
	"powershell": {Name: "powershell", Prompt: `(?m)^PS [^\n>]*> ?\z`},
	"none":       {Name: "none"},
}

// PresetNames lists the valid preset names.
func PresetNames() []string {
	return []string{"bash", "zsh", "sh", "python", "gdb", "lldb", "psql", "node", "cmd", "powershell", "none"}
}

// InferPreset guesses a preset from the executable's base name.
func InferPreset(command string) string {
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(command, `\`, "/")))
	base = strings.TrimSuffix(base, ".exe")
	switch {
	case base == "bash", base == "sh", base == "dash", base == "ash":
		if base == "bash" {
			return "bash"
		}
		return "sh"
	case base == "zsh":
		return "zsh"
	case strings.HasPrefix(base, "python"), base == "py", base == "ipython":
		return "python"
	case base == "gdb":
		return "gdb"
	case base == "lldb":
		return "lldb"
	case base == "psql":
		return "psql"
	case base == "node", base == "nodejs":
		return "node"
	case base == "cmd":
		return "cmd"
	case base == "powershell", base == "pwsh":
		return "powershell"
	}
	return "none"
}

func lookupPreset(name string) (Preset, bool) {
	p, ok := presets[name]
	return p, ok
}

// eol is the line terminator written after input.
func eol(tty bool) string {
	if runtime.GOOS == "windows" {
		if tty {
			return "\r"
		}
		return "\r\n"
	}
	return "\n"
}
