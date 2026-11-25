//go:build windows

package e2e

import (
	"strings"
	"testing"
)

func TestCmdStatePersists(t *testing.T) {
	c := newClient(t)
	out := c.mustCall("launch", map[string]any{"command": "cmd.exe"})
	id := out["session_id"].(string)
	c.mustCall("send", map[string]any{"session_id": id, "input": "set ONE=1"})
	r := c.mustCall("send", map[string]any{"session_id": id, "input": "echo %ONE%"})
	if !strings.Contains(r["output"].(string), "1") {
		t.Fatalf("output = %v", r)
	}
	c.mustCall("kill", map[string]any{"session_id": id})
}

func TestPipeMode(t *testing.T) {
	c := newClient(t)
	out := c.mustCall("launch", map[string]any{"command": "cmd.exe", "pipe": true})
	id := out["session_id"].(string)
	r := c.mustCall("send", map[string]any{"session_id": id, "input": "echo hello"})
	if !strings.Contains(r["output"].(string), "hello") {
		t.Fatalf("output = %v", r)
	}
	c.mustCall("kill", map[string]any{"session_id": id})
}
