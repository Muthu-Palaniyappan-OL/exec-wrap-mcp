//go:build !windows

package e2e

import (
	"strings"
	"testing"
	"time"
)

func launchBash(t *testing.T, c *client) string {
	t.Helper()
	out := c.mustCall("launch", map[string]any{"command": "/bin/bash"})
	if out["done_reason"] != "prompt" {
		t.Fatalf("launch done_reason = %v, want prompt: %v", out["done_reason"], out)
	}
	return out["session_id"].(string)
}

func send(t *testing.T, c *client, id, in string, extra map[string]any) map[string]any {
	args := map[string]any{"session_id": id, "input": in}
	for k, v := range extra {
		args[k] = v
	}
	return c.mustCall("send", args)
}

func TestStatePersistsAcrossCalls(t *testing.T) {
	c := newClient(t)
	id := launchBash(t, c)
	send(t, c, id, "export ONE=1", nil)
	out := send(t, c, id, "echo $ONE", nil)
	if got := out["output"]; got != "1" {
		t.Fatalf("output = %q, want 1 (%v)", got, out)
	}
	if out["done_reason"] != "prompt" {
		t.Fatalf("done_reason = %v", out["done_reason"])
	}
	send(t, c, id, "cd /tmp", nil)
	out = send(t, c, id, "pwd -P", nil)
	if !strings.HasSuffix(out["output"].(string), "/tmp") {
		t.Fatalf("cwd did not persist: %v", out)
	}
	out = send(t, c, id, "false", nil)
	if out["last_exit_status"] != float64(1) {
		t.Fatalf("last_exit_status = %v", out["last_exit_status"])
	}
}

func TestSessionsAreIsolated(t *testing.T) {
	c := newClient(t)
	a, b := launchBash(t, c), launchBash(t, c)
	send(t, c, a, "export WHO=alice", nil)
	send(t, c, b, "export WHO=bob", nil)
	if got := send(t, c, a, "echo $WHO", nil)["output"]; got != "alice" {
		t.Fatalf("a = %v", got)
	}
	if got := send(t, c, b, "echo $WHO", nil)["output"]; got != "bob" {
		t.Fatalf("b = %v", got)
	}
	list := c.mustCall("list", nil)
	if n := len(list["sessions"].([]any)); n != 2 {
		t.Fatalf("list = %d sessions", n)
	}
}

func TestMultiLineInput(t *testing.T) {
	c := newClient(t)
	id := launchBash(t, c)
	out := send(t, c, id, "for i in 1 2 3; do\necho n$i\ndone", nil)
	if out["output"] != "n1\nn2\nn3" {
		t.Fatalf("output = %q", out["output"])
	}
	out = send(t, c, id, "echo a\necho b", nil)
	if out["output"] != "a\nb" {
		t.Fatalf("output = %q", out["output"])
	}
}

func TestTimeoutThenRead(t *testing.T) {
	c := newClient(t)
	id := launchBash(t, c)
	out := send(t, c, id, "sleep 1; echo done-sleeping", map[string]any{"wait_ms": 300})
	if out["done_reason"] != "timeout" {
		t.Fatalf("done_reason = %v", out["done_reason"])
	}
	out = c.mustCall("send", map[string]any{"session_id": id, "wait_ms": 5000})
	if out["output"] != "done-sleeping" || out["done_reason"] != "prompt" {
		t.Fatalf("read = %v", out)
	}
}

func TestInterrupt(t *testing.T) {
	c := newClient(t)
	id := launchBash(t, c)
	out := send(t, c, id, "sleep 100", map[string]any{"wait_ms": 300})
	if out["done_reason"] != "timeout" {
		t.Fatalf("done_reason = %v", out["done_reason"])
	}
	out = c.mustCall("send", map[string]any{"session_id": id, "key": "ctrl-c"})
	if out["done_reason"] != "prompt" {
		t.Fatalf("after interrupt: %v", out)
	}
	if got := send(t, c, id, "echo alive", nil)["output"]; got != "alive" {
		t.Fatalf("got %v", got)
	}
}

func TestLargeOutputTruncated(t *testing.T) {
	c := newClient(t, "--max-output", "2000")
	id := launchBash(t, c)
	out := send(t, c, id, "seq 1 200000", nil)
	s := out["output"].(string)
	if out["truncated"] != true || !strings.Contains(s, "bytes omitted") || !strings.HasSuffix(s, "200000") || !strings.HasPrefix(s, "1\n") {
		t.Fatalf("truncated=%v len=%d head=%q", out["truncated"], len(s), s[:20])
	}
}

func TestExitAndKill(t *testing.T) {
	c := newClient(t)
	id := launchBash(t, c)
	out := send(t, c, id, "exit 7", nil)
	if out["exited"] != true || out["exit_code"] != float64(7) {
		t.Fatalf("exit: %v", out)
	}
	_, text, isErr := c.call("send", map[string]any{"session_id": id, "input": "echo x"})
	if !isErr || !strings.Contains(text, "session_exited") {
		t.Fatalf("send to dead session: %v %q", isErr, text)
	}
	k := c.mustCall("kill", map[string]any{"session_id": id})
	if k["exit_code"] != float64(7) {
		t.Fatalf("kill: %v", k)
	}
	_, text, isErr = c.call("kill", map[string]any{"session_id": id})
	if !isErr || !strings.Contains(text, "session_closed") || !strings.Contains(text, "kill") {
		t.Fatalf("second kill: %v %q", isErr, text)
	}
}

func TestKillRunningShell(t *testing.T) {
	c := newClient(t)
	id := launchBash(t, c)
	k := c.mustCall("kill", map[string]any{"session_id": id})
	if k["forced"] == true {
		t.Fatalf("expected graceful termination: %v", k)
	}
}

func TestPythonREPL(t *testing.T) {
	c := newClient(t)
	out := c.mustCall("launch", map[string]any{"command": "python3", "args": []string{"-q"}})
	id := out["session_id"].(string)
	if out["preset"] != "python" {
		t.Fatalf("preset = %v", out["preset"])
	}
	send(t, c, id, "x = 21", nil)
	if got := send(t, c, id, "print(x * 2)", nil)["output"]; got != "42" {
		t.Fatalf("got %q", got)
	}
}

func TestNoPromptFallsBackToIdle(t *testing.T) {
	c := newClient(t)
	out := c.mustCall("launch", map[string]any{"command": "cat", "pipe": true})
	id := out["session_id"].(string)
	r := send(t, c, id, "hello", nil)
	if r["output"] != "hello" || r["done_reason"] != "idle" {
		t.Fatalf("got %v", r)
	}
}

func TestUntilRegex(t *testing.T) {
	c := newClient(t)
	id := launchBash(t, c)
	out := send(t, c, id, "echo start; sleep 1; echo READY; sleep 2", map[string]any{"until": "READY"})
	if out["done_reason"] != "until" || !strings.Contains(out["output"].(string), "READY") {
		t.Fatalf("got %v", out)
	}
}

func TestPolicyAndLimits(t *testing.T) {
	c := newClient(t, "--max-sessions", "1", "--allow-command", "/bin/*")
	_, text, isErr := c.call("launch", map[string]any{"command": "python3"})
	if !isErr || !strings.Contains(text, "not_allowed") {
		t.Fatalf("allowlist: %v %q", isErr, text)
	}
	launchBash(t, c)
	_, text, isErr = c.call("launch", map[string]any{"command": "/bin/bash"})
	if !isErr || !strings.Contains(text, "limit_reached") {
		t.Fatalf("limit: %v %q", isErr, text)
	}
}

func TestIdleReaping(t *testing.T) {
	c := newClient(t, "--idle-ttl", "400ms")
	id := launchBash(t, c)
	time.Sleep(1500 * time.Millisecond)
	_, text, isErr := c.call("send", map[string]any{"session_id": id, "input": "echo hi"})
	if !isErr || !strings.Contains(text, "session_closed") || !strings.Contains(text, "without use") {
		t.Fatalf("expected reaped session: %v %q", isErr, text)
	}
}

func TestBadArguments(t *testing.T) {
	c := newClient(t)
	_, text, isErr := c.call("launch", map[string]any{"command": "/definitely/not/here"})
	if !isErr || !strings.Contains(text, "spawn_failed") {
		t.Fatalf("spawn: %v %q", isErr, text)
	}
	id := launchBash(t, c)
	_, text, isErr = c.call("send", map[string]any{"session_id": id, "input": "x", "until": "("})
	if !isErr || !strings.Contains(text, "invalid_argument") {
		t.Fatalf("regex: %v %q", isErr, text)
	}
}
