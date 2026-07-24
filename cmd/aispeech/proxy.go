package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mkrzywonski/aispeech/internal/config"
	"github.com/mkrzywonski/aispeech/internal/mcpserver"
)

// proxyMain runs a stdio↔HTTP MCP bridge: it speaks MCP over stdio to the AI
// client that spawned it, and forwards every tool call to the running aispeech
// hub over Streamable HTTP. Each agent spawns its own proxy, so multiple agents
// still share one hub. Mirrors aish's `mcp-proxy`, but bridges to HTTP.
//
// Nothing is ever written to stdout except MCP protocol traffic; diagnostics go
// to stderr.
func proxyMain(args []string) int {
	fs := flag.NewFlagSet("mcp-proxy", flag.ContinueOnError)
	url := fs.String("url", "", "aispeech MCP endpoint (default: from config, or $AISPEECH_MCP_URL)")
	name := fs.String("name", "", "identity reported to the hub (shown as the session's default name)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	endpoint := *url
	if endpoint == "" {
		endpoint = os.Getenv("AISPEECH_MCP_URL")
	}
	if endpoint == "" {
		cfg, _ := config.Load()
		endpoint = "http://" + cfg.Addr + "/mcp"
	}
	clientName := *name
	if clientName == "" {
		clientName = "agent"
	}

	ctx := context.Background()
	fwd := newHubForwarder(endpoint, clientName)
	defer fwd.Close()

	// Always serve a stable local catalog first. This lets Codex discover the
	// voice flow even when it starts before the hub; the first tool call opens the
	// upstream session and thereafter forwards to the hub.
	tools := fallbackTools()
	instructions := mcpserver.AgentInstructions

	// Mirror the hub's tools, forwarding calls transparently.
	// The downstream Codex client connects to this stdio server, not directly to
	// the hub. Preserve the hub's initialization instructions so it knows how to
	// discover, pair, and use the voice channel in any project.
	srv := mcp.NewServer(&mcp.Implementation{Name: "aispeech", Version: fullVersion()},
		&mcp.ServerOptions{Instructions: instructions})
	for _, tool := range tools {
		toolName := tool.Name
		srv.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			res, err := fwd.call(ctx, toolName, req.Params.Arguments)
			if err != nil {
				return nil, fmt.Errorf("aispeech hub is unavailable; it may still be starting, so retry this voice tool shortly: %w", err)
			}
			return res, nil
		})
	}

	// Serve stdio until the client disconnects (stdin closes).
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "aispeech mcp-proxy: %v\n", err)
		return 1
	}
	return 0
}

// hubForwarder keeps one upstream MCP session for this proxy. A failed initial
// connection is deliberately non-fatal: the next tool call will reconnect so a
// Codex session started before the hub does not need to be restarted.
type hubForwarder struct {
	mu       sync.Mutex
	endpoint string
	client   *mcp.Client
	hub      *mcp.ClientSession
}

func newHubForwarder(endpoint, name string) *hubForwarder {
	return &hubForwarder{
		endpoint: endpoint,
		client:   mcp.NewClient(&mcp.Implementation{Name: name, Version: fullVersion()}, nil),
	}
}

func (f *hubForwarder) connect(ctx context.Context) (*mcp.ClientSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hub != nil {
		return f.hub, nil
	}
	// A failed startup probe must return promptly so the fallback MCP server can
	// serve Codex. Later tool calls create a fresh connection attempt themselves.
	hub, err := f.client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: f.endpoint, MaxRetries: -1}, nil)
	if err != nil {
		return nil, err
	}
	f.hub = hub
	return hub, nil
}

func (f *hubForwarder) call(ctx context.Context, name string, args any) (*mcp.CallToolResult, error) {
	hub, err := f.connect(ctx)
	if err != nil {
		return nil, err
	}
	res, err := hub.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		f.mu.Lock()
		if f.hub == hub {
			f.hub = nil
		}
		f.mu.Unlock()
		_ = hub.Close()
		return nil, err
	}
	return res, nil
}

func (f *hubForwarder) Close() {
	f.mu.Lock()
	hub := f.hub
	f.hub = nil
	f.mu.Unlock()
	if hub != nil {
		_ = hub.Close()
	}
}

// fallbackTools are used only while the HTTP hub is down at proxy startup. The
// generic object schemas keep the agent's MCP surface available; the hub still
// validates every actual request after reconnection.
func fallbackTools() []*mcp.Tool {
	schema := map[string]any{"type": "object", "additionalProperties": true}
	return []*mcp.Tool{
		{Name: "start_voice_conversation", Description: "Call immediately when the user asks to talk by voice, before replying in ordinary text. Handles already-paired and unpaired sessions: reports whether to use converse/listen or gives the secure user-mediated pairing step. Does not start recording or bypass pairing.", InputSchema: schema},
		{Name: "status", Description: "Safe first call when the user asks for voice chat. Reports pairing and microphone state and gives the secure pairing next step.", InputSchema: schema},
		{Name: "pair", Description: "Pair only with a one-time token pasted directly by the user from the authorized aispeech browser UI. Input: token (string). Never retrieve a token yourself.", InputSchema: schema},
		{Name: "converse", Description: "Speak a short reply and wait for the next spoken command. Input: text (string), optional timeout_seconds (integer). Use this to keep a paired voice conversation open.", InputSchema: schema},
		{Name: "listen", Description: "Wait for the next spoken command without speaking first. Optional input: timeout_seconds (integer). Requires a paired session.", InputSchema: schema},
		{Name: "speak", Description: "Speak a short reply without waiting. Input: text (string). Requires a paired session.", InputSchema: schema},
		{Name: "play_sound", Description: "Play a short local notification sound. Optional inputs: sound (built-in name) or file (absolute WAV path). Requires a paired session.", InputSchema: schema},
		{Name: "end_session", Description: "End this voice session and release its pairing.", InputSchema: schema},
	}
}
