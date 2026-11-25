//go:build !windows

package e2e

import (
	"os/exec"
	"strings"
	"testing"
)

// A debugger keeps state (here: the loaded target and a variable) across calls.
func TestLLDBSession(t *testing.T) {
	if _, err := exec.LookPath("lldb"); err != nil {
		t.Skip("lldb not installed")
	}
	c := newClient(t)
	out := c.mustCall("launch", map[string]any{"command": "lldb", "args": []string{"--no-lldbinit"}})
	id := out["session_id"].(string)
	if out["preset"] != "lldb" {
		t.Fatalf("preset = %v", out["preset"])
	}
	c.mustCall("send", map[string]any{"session_id": id, "input": "expression int $x = 40"})
	r := c.mustCall("send", map[string]any{"session_id": id, "input": "expression $x + 2"})
	if !strings.Contains(r["output"].(string), "42") || r["done_reason"] != "prompt" {
		t.Fatalf("got %v", r)
	}
	c.mustCall("kill", map[string]any{"session_id": id})
}
