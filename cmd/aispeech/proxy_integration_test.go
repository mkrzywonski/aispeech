package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mkrzywonski/aispeech/internal/authz"
	"github.com/mkrzywonski/aispeech/internal/engine"
	"github.com/mkrzywonski/aispeech/internal/mcpserver"
	"github.com/mkrzywonski/aispeech/internal/session"
)

// TestProxyBridge builds the aispeech binary and drives its `mcp-proxy` stdio
// bridge as an MCP client, verifying it transparently forwards pair/listen to a
// hub over HTTP and preserves the --name identity.
func TestProxyBridge(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary; skipped in -short")
	}
	bin := filepath.Join(t.TempDir(), "aispeech")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}

	reg := session.New()
	svc := engine.New(reg, nil, nil, nil, time.Minute, 600)
	store := authz.NewStore(time.Minute)
	ts := httptest.NewServer(mcpserver.NewHandler(reg, svc, store, mcpserver.Options{
		DefaultListenTimeout: 5 * time.Second, MaxListenTimeout: time.Minute,
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Spawn the proxy as the MCP server (stdio), pointed at the HTTP hub.
	cmd := exec.CommandContext(ctx, bin, "mcp-proxy", "--url", ts.URL, "--name", "claude")
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	cs, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect via proxy: %v", err)
	}
	defer cs.Close()

	// The proxy should mirror the hub's tools.
	names := map[string]bool{}
	for tool, err := range cs.Tools(ctx, nil) {
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		names[tool.Name] = true
	}
	for _, want := range []string{"pair", "converse", "listen", "speak", "status", "end_session"} {
		if !names[want] {
			t.Fatalf("proxy did not mirror tool %q (got %v)", want, names)
		}
	}

	// status attaches the session at the hub; the identity should be "claude".
	call(t, ctx, cs, "status", nil)
	views, _, _ := reg.Snapshot()
	if len(views) != 1 || views[0].ClientName != "claude" {
		t.Fatalf("hub session = %+v, want one named claude", views)
	}
	cookie := store.NewBrowser()
	code, _ := store.IssueToken(cookie)

	// Pair through the proxy.
	var pr struct {
		OK   bool   `json:"ok"`
		Name string `json:"name"`
	}
	decode(t, call(t, ctx, cs, "pair", map[string]any{"token": code}), &pr)
	if !pr.OK {
		t.Fatal("pair via proxy failed")
	}

	// listen through the proxy, deliver an utterance, expect it back.
	type lr struct {
		Status, Text string
	}
	res := make(chan lr, 1)
	go func() {
		var r lr
		decode(t, call(t, ctx, cs, "listen", map[string]any{"timeout_seconds": 5}), &r)
		res <- r
	}()
	time.Sleep(300 * time.Millisecond)
	svc.InjectTranscript("ship it")
	select {
	case r := <-res:
		if r.Status != "ok" || r.Text != "ship it" {
			t.Fatalf("listen via proxy = %+v", r)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("listen via proxy did not return")
	}
}

func call(t *testing.T, ctx context.Context, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("call %s returned error: %+v", name, res.Content)
	}
	return res
}

func decode(t *testing.T, res *mcp.CallToolResult, out any) {
	t.Helper()
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("decode: %v", err)
	}
}
