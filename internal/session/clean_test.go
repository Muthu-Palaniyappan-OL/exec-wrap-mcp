package session

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestClean(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "hello\n", "hello\n"},
		{"crlf", "a\r\nb\r\n", "a\nb\n"},
		{"csi color", "\x1b[31mred\x1b[0m", "red"},
		{"osc title", "\x1b]0;title\x07text", "text"},
		{"bracketed paste", "\x1b[?2004hprompt", "prompt"},
		{"carriage return overwrite", "loading 10%\rloading 99%\n", "loading 99%\n"},
		{"backspace", "ab\b\bcd", "cd"},
		{"bell and nul", "a\x07\x00b", "ab"},
		{"keeps marker", "\x1eewm:0\x1e", "\x1eewm:0\x1e"},
		{"invalid utf8", "a\xffb", "a\uFFFDb"},
	}
	for _, c := range cases {
		if got := Clean([]byte(c.in), true); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
	if got := Clean([]byte("\x1b[31mx\x1b[0m\r\n"), false); got != "\x1b[31mx\x1b[0m\n" {
		t.Errorf("raw mode: %q", got)
	}
}

func TestTruncate(t *testing.T) {
	s := strings.Repeat("a", 1000)
	if got, tr := Truncate(s, 2000); tr || got != s {
		t.Fatal("should not truncate")
	}
	got, tr := Truncate(s, 100)
	if !tr || !strings.Contains(got, "[... 900 bytes omitted ...]") {
		t.Fatalf("got %q", got)
	}
	if !strings.HasPrefix(got, strings.Repeat("a", 25)+"\n") {
		t.Fatal("head should be 25%")
	}
	u := strings.Repeat("é", 500)
	got, _ = Truncate(u, 101)
	if !utf8.ValidString(got) {
		t.Fatal("truncation split a rune")
	}
}

func TestInferPreset(t *testing.T) {
	for in, want := range map[string]string{
		"/bin/bash": "bash", "/usr/bin/zsh": "zsh", "sh": "sh", "python3.12": "python",
		"gdb": "gdb", `C:\Windows\System32\cmd.exe`: "cmd", "pwsh": "powershell", "mystery": "none",
	} {
		if got := InferPreset(in); got != want {
			t.Errorf("InferPreset(%q)=%q want %q", in, got, want)
		}
	}
}

func TestPromptRegexes(t *testing.T) {
	cases := []struct {
		preset, text string
		match        bool
	}{
		{"python", "42\n>>> ", true},
		{"python", ">>> ", true},
		{"python", "... ", true},
		{"python", "a >>> b", false},
		{"gdb", "Breakpoint 1\n(gdb) ", true},
		{"gdb", "(gdb) x", false},
		{"psql", "postgres=# ", true},
		{"psql", "postgres-# ", true},
		{"psql", "mydb=> ", true},
		{"node", "> ", true},
		{"cmd", "C:\\Users\\me>", true},
		{"powershell", "PS C:\\Users\\me> ", true},
		{"bash", "out\n\x1eewm:0\x1e", true},
		{"bash", "\x1eewm:0\x1e more", false},
	}
	for _, c := range cases {
		p, _ := lookupPreset(c.preset)
		re := mustCompile(t, p.Prompt)
		if got := re.MatchString(c.text); got != c.match {
			t.Errorf("%s %q: got %v want %v", c.preset, c.text, got, c.match)
		}
	}
}

func TestStripEcho(t *testing.T) {
	if got := stripEcho("python> x = 1\nout", "x = 1"); got != "out" {
		t.Fatalf("got %q", got)
	}
	if got := stripEcho("other\nout", "x = 1"); got != "other\nout" {
		t.Fatalf("got %q", got)
	}
}

func TestMergeEnv(t *testing.T) {
	got := mergeEnv([]string{"A=1", "B=2", "A=3"}, map[string]string{"B": "x", "C": "y"})
	want := map[string]bool{"A=3": true, "B=x": true, "C=y": true}
	if len(got) != 3 {
		t.Fatalf("got %v", got)
	}
	for _, g := range got {
		if !want[g] {
			t.Fatalf("unexpected %q in %v", g, got)
		}
	}
}

func mustCompile(t *testing.T, s string) *regexp.Regexp {
	t.Helper()
	re, err := regexp.Compile(s)
	if err != nil {
		t.Fatal(err)
	}
	return re
}
