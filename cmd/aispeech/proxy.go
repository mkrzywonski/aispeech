package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mkrzywonski/aispeech/internal/config"
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

	// Connect to the hub, identifying as the downstream agent so the hub labels
	// the session correctly.
	client := mcp.NewClient(&mcp.Implementation{Name: clientName, Version: fullVersion()}, nil)
	hub, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint}, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aispeech mcp-proxy: cannot reach hub at %s: %v\n", endpoint, err)
		return 1
	}
	defer hub.Close()

	// Mirror whatever tools the hub exposes, forwarding calls transparently.
	lt, err := hub.ListTools(ctx, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aispeech mcp-proxy: list tools: %v\n", err)
		return 1
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "aispeech", Version: fullVersion()}, nil)
	for _, tool := range lt.Tools {
		toolName := tool.Name
		srv.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return hub.CallTool(ctx, &mcp.CallToolParams{Name: toolName, Arguments: req.Params.Arguments})
		})
	}

	// Serve stdio until the client disconnects (stdin closes).
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "aispeech mcp-proxy: %v\n", err)
		return 1
	}
	return 0
}
