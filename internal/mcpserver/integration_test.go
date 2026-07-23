package mcpserver_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mkrzywonski/aispeech/internal/engine"
	"github.com/mkrzywonski/aispeech/internal/mcpserver"
	"github.com/mkrzywonski/aispeech/internal/session"
)

// TestPairListenDeliver drives the full path an agent would take: connect,
// discover it's unpaired, pair with the UI code, then listen and receive a
// routed utterance.
func TestPairListenDeliver(t *testing.T) {
	reg := session.New()
	svc := engine.New(reg, nil, nil, nil, time.Minute, 600)

	ts := httptest.NewServer(mcpserver.NewHandler(reg, svc, mcpserver.Options{
		DefaultListenTimeout: 5 * time.Second,
		MaxListenTimeout:     time.Minute,
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "claude", Version: "test"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()

	// listen before pairing must be refused (and this also attaches the session).
	if res := callToolRaw(t, ctx, cs, "listen", nil); !res.IsError {
		t.Fatal("unpaired listen should error")
	}

	// Grab the pairing code the UI would show for this pending session.
	views, _, _ := reg.Snapshot()
	if len(views) != 1 {
		t.Fatalf("want 1 session, got %d", len(views))
	}
	code := views[0].PairingCode
	if code == "" {
		t.Fatal("no pairing code generated")
	}

	// Pair.
	var pr struct {
		OK   bool   `json:"ok"`
		Name string `json:"name"`
	}
	structured(t, ctx, cs, "pair", map[string]any{"code": code}, &pr)
	if !pr.OK {
		t.Fatal("pair not ok")
	}

	// Start listening; deliver an utterance; expect it back.
	type listenResult struct {
		Status  string `json:"status"`
		Text    string `json:"text"`
		Session string `json:"session"`
	}
	resCh := make(chan listenResult, 1)
	go func() {
		var lr listenResult
		structured(t, ctx, cs, "listen", map[string]any{"timeout_seconds": 5}, &lr)
		resCh <- lr
	}()

	time.Sleep(200 * time.Millisecond) // let listen register
	svc.InjectTranscript("run the tests")

	select {
	case lr := <-resCh:
		if lr.Status != "ok" || lr.Text != "run the tests" {
			t.Fatalf("bad listen result: %+v", lr)
		}
		if lr.Session != pr.Name {
			t.Fatalf("routed to %q, want %q", lr.Session, pr.Name)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("listen did not return")
	}
}

func structured(t *testing.T, ctx context.Context, cs *mcp.ClientSession, name string, args map[string]any, out any) {
	t.Helper()
	res := callToolRaw(t, ctx, cs, name, args)
	if res.IsError {
		t.Fatalf("%s returned error: %+v", name, res.Content)
	}
	if err := decodeStructured(res, out); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}

func decodeStructured(res *mcp.CallToolResult, out any) error {
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func callToolRaw(t *testing.T, ctx context.Context, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}
