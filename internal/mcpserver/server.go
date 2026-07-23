// Package mcpserver exposes the voice capabilities (pair, listen, speak,
// end_session, status) to AI agents over MCP Streamable HTTP.
package mcpserver

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mkrzywonski/aispeech/internal/engine"
	"github.com/mkrzywonski/aispeech/internal/session"
)

const version = "0.0.1"

// Options configures the server's timing behavior.
type Options struct {
	DefaultListenTimeout time.Duration
	MaxListenTimeout     time.Duration
}

type deps struct {
	reg  *session.Registry
	svc  *engine.Service
	opts Options
}

// NewHandler builds the MCP HTTP handler. A single logical server is shared
// across all connections; the SDK isolates each connection as its own session.
func NewHandler(reg *session.Registry, svc *engine.Service, opts Options) http.Handler {
	if opts.DefaultListenTimeout == 0 {
		opts.DefaultListenTimeout = 2 * time.Minute
	}
	if opts.MaxListenTimeout == 0 {
		opts.MaxListenTimeout = 10 * time.Minute
	}
	d := &deps{reg: reg, svc: svc, opts: opts}
	srv := d.build()
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
}

func (d *deps) build() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "aispeech", Version: version}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name: "pair",
		Description: "Authorize this voice session. The aispeech UI shows an 8-character " +
			"pairing code for this connection; ask the user to read it to you, then call " +
			"pair with that code. Until paired, listen and speak will not work.",
	}, d.pair)

	mcp.AddTool(s, &mcp.Tool{
		Name: "listen",
		Description: "Wait for the user to speak a command to this session and return the " +
			"transcript. Blocks until speech is routed here or the timeout elapses. If it " +
			"returns status \"timeout\", no speech arrived — call listen again to keep " +
			"waiting, or stop. Typically call listen after finishing a spoken exchange.",
	}, d.listen)

	mcp.AddTool(s, &mcp.Tool{
		Name: "speak",
		Description: "Speak a SHORT reply aloud to the user. Use terse confirmations and " +
			"answers only — do not read code, diffs, or long output aloud; let those scroll " +
			"in the terminal. Long text is truncated.",
	}, d.speak)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "end_session",
		Description: "Drop this voice session. listen and speak stop working until you pair again.",
	}, d.endSession)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "status",
		Description: "Report this session's voice state: whether it is paired, focused, and whether the microphone is currently active.",
	}, d.status)

	return s
}

// --- tool inputs/outputs ---

type pairIn struct {
	Code string `json:"code" jsonschema:"the 8-character pairing code shown in the aispeech UI"`
}
type pairOut struct {
	OK        bool   `json:"ok"`
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
}

type listenIn struct {
	TimeoutSeconds int `json:"timeout_seconds,omitempty" jsonschema:"how long to wait for speech, in seconds"`
}
type listenOut struct {
	Status  string `json:"status"` // ok | timeout | cancelled
	Text    string `json:"text,omitempty"`
	Session string `json:"session,omitempty"`
}

type speakIn struct {
	Text string `json:"text" jsonschema:"the short text to speak aloud"`
}
type speakOut struct {
	OK          bool `json:"ok"`
	SpokenChars int  `json:"spoken_chars"`
	Truncated   bool `json:"truncated"`
}

type emptyIn struct{}

type okOut struct {
	OK bool `json:"ok"`
}

type statusOut struct {
	Paired       bool     `json:"paired"`
	Name         string   `json:"name"`
	Focused      bool     `json:"focused"`
	ListeningNow bool     `json:"listening_now"`
	MicMode      string   `json:"mic_mode"`
	OtherSessions []string `json:"other_sessions"`
}

// --- handlers ---

func (d *deps) attach(req *mcp.CallToolRequest) session.SessionView {
	id := req.Session.ID()
	name := ""
	if ip := req.Session.InitializeParams(); ip != nil && ip.ClientInfo != nil {
		name = ip.ClientInfo.Name
	}
	d.reg.Attach(id, name)
	v, _ := d.reg.Get(id)
	return v
}

func (d *deps) pair(ctx context.Context, req *mcp.CallToolRequest, in pairIn) (*mcp.CallToolResult, pairOut, error) {
	d.attach(req)
	s, err := d.reg.Pair(req.Session.ID(), in.Code)
	if err != nil {
		return nil, pairOut{}, err // surfaced to the model as tool error text
	}
	return nil, pairOut{OK: true, SessionID: s.ID, Name: s.Name}, nil
}

func (d *deps) listen(ctx context.Context, req *mcp.CallToolRequest, in listenIn) (*mcp.CallToolResult, listenOut, error) {
	v := d.attach(req)
	if !v.Paired {
		return nil, listenOut{}, errUnpaired
	}
	timeout := d.opts.DefaultListenTimeout
	if in.TimeoutSeconds > 0 {
		timeout = time.Duration(in.TimeoutSeconds) * time.Second
	}
	if timeout > d.opts.MaxListenTimeout {
		timeout = d.opts.MaxListenTimeout
	}
	u, status := d.reg.Listen(ctx, req.Session.ID(), timeout)
	switch status {
	case "ok":
		return nil, listenOut{Status: "ok", Text: u.Text, Session: u.Target}, nil
	case "unpaired":
		return nil, listenOut{}, errUnpaired
	default: // timeout | cancelled
		return nil, listenOut{Status: status}, nil
	}
}

func (d *deps) speak(ctx context.Context, req *mcp.CallToolRequest, in speakIn) (*mcp.CallToolResult, speakOut, error) {
	v := d.attach(req)
	if !v.Paired {
		return nil, speakOut{}, errUnpaired
	}
	n, trunc, err := d.svc.Speak(ctx, in.Text)
	if err != nil {
		return nil, speakOut{}, err
	}
	return nil, speakOut{OK: true, SpokenChars: n, Truncated: trunc}, nil
}

func (d *deps) endSession(ctx context.Context, req *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, okOut, error) {
	d.reg.Detach(req.Session.ID())
	return nil, okOut{OK: true}, nil
}

func (d *deps) status(ctx context.Context, req *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, statusOut, error) {
	v := d.attach(req)
	views, _, _ := d.reg.Snapshot()
	others := make([]string, 0, len(views))
	for _, o := range views {
		if o.ID != v.ID && o.Paired {
			others = append(others, o.Name)
		}
	}
	return nil, statusOut{
		Paired:        v.Paired,
		Name:          v.Name,
		Focused:       v.Focused,
		ListeningNow:  v.Listening,
		MicMode:       d.svc.Mode().String(),
		OtherSessions: others,
	}, nil
}

var errUnpaired = fmt.Errorf(
	"this voice session isn't paired yet: ask the user for the 8-character pairing code shown " +
		"in the aispeech UI, then call the pair tool with it")
