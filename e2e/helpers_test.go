package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var binPath string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ewm-e2e")
	if err != nil {
		panic(err)
	}
	name := "exec-wrap-mcp"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binPath = filepath.Join(dir, name)
	out, err := exec.Command("go", "build", "-o", binPath, "..").CombinedOutput()
	if err != nil {
		panic(string(out))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

type client struct {
	t  *testing.T
	cs *mcp.ClientSession
}

// newClient starts the server with --structured so tests can assert on fields.
func newClient(t *testing.T, args ...string) *client {
	t.Helper()
	return newTextClient(t, append([]string{"--structured"}, args...)...)
}

// newTextClient starts the server in its default compact, text-only mode.
func newTextClient(t *testing.T, args ...string) *client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	c := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, nil)
	cs, err := c.Connect(ctx, &mcp.CommandTransport{Command: exec.Command(binPath, args...)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return &client{t: t, cs: cs}
}

func (c *client) toolsJSON() []byte {
	c.t.Helper()
	res, err := c.cs.ListTools(context.Background(), nil)
	if err != nil {
		c.t.Fatal(err)
	}
	b, _ := json.Marshal(res.Tools)
	return b
}

// call invokes a tool and decodes structured output into a map.
func (c *client) call(name string, args map[string]any) (map[string]any, string, bool) {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	res, err := c.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		c.t.Fatalf("%s: %v", name, err)
	}
	text := ""
	for _, ct := range res.Content {
		if tc, ok := ct.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	var out map[string]any
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		_ = json.Unmarshal(b, &out)
	}
	return out, text, res.IsError
}

func (c *client) mustCall(name string, args map[string]any) map[string]any {
	c.t.Helper()
	out, text, isErr := c.call(name, args)
	if isErr {
		c.t.Fatalf("%s returned error: %s", name, text)
	}
	return out
}

func (c *client) toolNames() []string {
	c.t.Helper()
	res, err := c.cs.ListTools(context.Background(), nil)
	if err != nil {
		c.t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	sort.Strings(names)
	return names
}
