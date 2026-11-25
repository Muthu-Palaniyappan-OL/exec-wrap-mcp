//go:build !windows

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Token budget: the default (text-only) mode must stay small.
func TestCompactTextMode(t *testing.T) {
	c := newTextClient(t)
	if n := len(c.toolsJSON()); n > 3000 {
		t.Fatalf("tool schemas are %d bytes; budget is 3000", n)
	}
	_, text, isErr := c.call("launch", map[string]any{"command": "/bin/bash"})
	if isErr || !strings.HasPrefix(text, "s_") || strings.Contains(text, "\n") {
		t.Fatalf("launch text = %q", text)
	}
	id := text
	if _, text, _ = c.call("send", map[string]any{"session_id": id, "input": "export ONE=1"}); text != "ok" {
		t.Fatalf("empty reply = %q, want ok", text)
	}
	out, text, _ := c.call("send", map[string]any{"session_id": id, "input": "echo $ONE"})
	if text != "1" || out != nil {
		t.Fatalf("reply = %q structured=%v; want exactly 1 and no structured content", text, out)
	}
	if _, text, _ = c.call("send", map[string]any{"session_id": id, "input": "false; echo x"}); text != "x" {
		t.Fatalf("got %q", text)
	}
	_, text, _ = c.call("send", map[string]any{"session_id": id, "input": "ls /nonexistent-dir"})
	if !strings.HasSuffix(text, "\n[exit 1]") {
		t.Fatalf("got %q", text)
	}
	if got := c.toolNames(); strings.Join(got, ",") != "kill,launch,list,send" {
		t.Fatalf("tools = %v", got)
	}
}

func TestLaunchWithInputIsOneCall(t *testing.T) {
	c := newTextClient(t)
	_, text, isErr := c.call("launch", map[string]any{"command": "python3", "args": []string{"-q"},
		"input": "d = {'name': 'Alice', 'age': 30}; d"})
	if isErr || !strings.HasSuffix(text, "{'name': 'Alice', 'age': 30}") {
		t.Fatalf("got %q", text)
	}
	id := strings.SplitN(text, "\n", 2)[0]
	_, text, _ = c.call("send", map[string]any{"session_id": id, "input": "d['age']"})
	if text != "30" {
		t.Fatalf("state lost: %q", text)
	}
}

func TestSlowStartingPython(t *testing.T) {
	c := newClient(t)
	out := c.mustCall("launch", map[string]any{"command": "python3", "wait_ms": 1000,
		"args": []string{"-i", "-c", "import time; time.sleep(4); print('loaded')"}})
	id := out["session_id"].(string)
	if out["done_reason"] != "starting" {
		t.Fatalf("launch = %v", out)
	}
	list := c.mustCall("list", nil)["sessions"].([]any)[0].(map[string]any)
	if list["state"] != "starting" {
		t.Fatalf("state = %v", list["state"])
	}
	// The send waits for readiness, then runs; startup noise stays out of the answer.
	r := send(t, c, id, "print(6*7)", nil)
	if r["output"] != "loaded\n42" && r["output"] != "42" {
		t.Fatalf("got %v", r)
	}
	if r["done_reason"] != "prompt" {
		t.Fatalf("got %v", r)
	}
}

func TestSlowStartingBash(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "slow.rc")
	if err := os.WriteFile(rc, []byte("sleep 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newClient(t)
	begin := time.Now()
	out := c.mustCall("launch", map[string]any{"command": "/bin/bash", "args": []string{"--noediting", "--rcfile", rc}})
	if out["done_reason"] != "prompt" || time.Since(begin) < 2*time.Second {
		t.Fatalf("launch should wait for the slow rc: %v after %v", out, time.Since(begin))
	}
	id := out["session_id"].(string)
	if got := send(t, c, id, "echo hello", nil)["output"]; got != "hello" {
		t.Fatalf("first command output polluted: %q", got)
	}
}

func TestSlowStartingShellDoesNotLoseFirstCommand(t *testing.T) {
	rc := filepath.Join(t.TempDir(), "slow.rc")
	if err := os.WriteFile(rc, []byte("sleep 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newClient(t)
	out := c.mustCall("launch", map[string]any{"command": "/bin/bash", "wait_ms": 500,
		"args": []string{"--noediting", "--rcfile", rc}})
	if out["done_reason"] != "starting" {
		t.Fatalf("launch = %v", out)
	}
	id := out["session_id"].(string)
	r := send(t, c, id, "echo hello", map[string]any{"wait_ms": 500})
	if r["done_reason"] != "starting" {
		t.Fatalf("expected starting, got %v", r)
	}
	if got := send(t, c, id, "echo hello", nil)["output"]; got != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestDeadProcessMessages(t *testing.T) {
	c := newClient(t, "--max-sessions", "2")
	id := launchBash(t, c)
	out := send(t, c, id, "kill -9 $$", nil)
	if out["exited"] != true || out["exit_signal"] != "SIGKILL" || out["exit_code"] != float64(137) {
		t.Fatalf("signal death = %v", out)
	}
	_, text, isErr := c.call("send", map[string]any{"session_id": id, "input": "echo hi"})
	if !isErr || !strings.Contains(text, "session_exited") {
		t.Fatalf("send to dead: %q", text)
	}
	// Exited sessions free their slot when a new launch needs it.
	b := launchBash(t, c)
	c3 := c.mustCall("launch", map[string]any{"command": "/bin/bash"})
	if c3["session_id"] == nil {
		t.Fatal("launch should evict the exited session")
	}
	_, text, isErr = c.call("send", map[string]any{"session_id": id, "input": "x"})
	if !isErr || !strings.Contains(text, "session_closed") || !strings.Contains(text, "cleaned up") {
		t.Fatalf("evicted id: %q", text)
	}
	_ = b
	_, text, isErr = c.call("send", map[string]any{"session_id": "s_00000000", "input": "x"})
	if !isErr || !strings.Contains(text, "not_found") {
		t.Fatalf("unknown id: %q", text)
	}
}

func TestReaperSparesBusySession(t *testing.T) {
	c := newClient(t, "--idle-ttl", "500ms")
	id := launchBash(t, c)
	out := send(t, c, id, "sleep 2; echo survived", map[string]any{"wait_ms": 10000})
	if out["output"] != "survived" {
		t.Fatalf("session reaped mid-call: %v", out)
	}
}
