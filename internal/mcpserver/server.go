// Package mcpserver exposes the voice capabilities (pair, listen, speak,
// end_session, status) to AI agents over MCP Streamable HTTP.
package mcpserver

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mkrzywonski/aispeech/internal/authz"
	"github.com/mkrzywonski/aispeech/internal/engine"
	"github.com/mkrzywonski/aispeech/internal/session"
)

// maxPairAttempts caps failed pair attempts per MCP connection.
const maxPairAttempts = 5

// Options configures the server.
type Options struct {
	Version              string // build version, reported to the agent
	DefaultListenTimeout time.Duration
	MaxListenTimeout     time.Duration
}

type deps struct {
	reg    *session.Registry
	svc    *engine.Service
	store  *authz.Store
	voices func() []string // installed TTS voices, for distinct per-session assignment
	opts   Options
}

// NewHandler builds the MCP HTTP handler. A single logical server is shared
// across all connections; the SDK isolates each connection as its own session.
// voices (may be nil) lists installed TTS voices so each session gets a distinct
// one.
func NewHandler(reg *session.Registry, svc *engine.Service, store *authz.Store, voices func() []string, opts Options) http.Handler {
	if opts.DefaultListenTimeout == 0 {
		opts.DefaultListenTimeout = 2 * time.Minute
	}
	if opts.MaxListenTimeout == 0 {
		opts.MaxListenTimeout = 10 * time.Minute
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	d := &deps{reg: reg, svc: svc, store: store, voices: voices, opts: opts}
	srv := d.build()
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
}

// serverInstructions is sent to the client at initialize and tells the model how
// to run an ongoing voice dialog (the hub cannot continue a turn on its own).
const serverInstructions = "This server is a voice channel for talking with the user out loud. " +
	"When a voice session is paired and you receive a spoken utterance, enter VOICE-DIALOG MODE " +
	"and stay in it: after handling each request, end your turn by calling `converse` (speak a " +
	"short reply AND wait for the next command in one step) — or `listen` if you have nothing to " +
	"say yet. Keep spoken replies terse; let detail scroll in the terminal. On a `timeout` status, " +
	"call `converse`/`listen` again to keep waiting. Leave voice-dialog mode only when the user " +
	"says to stop, the microphone is turned off, pairing is revoked, or the session ends."

func (d *deps) build() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "aispeech", Version: d.opts.Version},
		&mcp.ServerOptions{Instructions: serverInstructions})

	mcp.AddTool(s, &mcp.Tool{
		Name: "pair",
		Description: "Authorize this voice session with a pairing token. Ask the user to click " +
			"\"Copy pairing token\" in the aispeech browser UI and paste the token to you, then " +
			"call pair with it. The token comes ONLY from the user via this chat — never obtain " +
			"it by making HTTP requests to the hub. Until paired, listen and speak will not work.",
	}, d.pair)

	mcp.AddTool(s, &mcp.Tool{
		Name: "converse",
		Description: "Speak a short reply AND immediately wait for the user's next spoken command — " +
			"the natural way to stay in a voice dialog. Prefer this over speak-then-end-turn: after " +
			"handling a voice command, call converse with your reply to keep the conversation going. " +
			"Returns the next utterance (status \"ok\") or a terminal status (\"timeout\", " +
			"\"cancelled\"). On \"timeout\", call converse or listen again while voice mode is active.",
	}, d.converse)

	mcp.AddTool(s, &mcp.Tool{
		Name: "listen",
		Description: "Wait for the user's next spoken command and return the transcript, WITHOUT " +
			"speaking first. Use converse instead when you have a spoken reply. Blocks until speech " +
			"is routed here or the timeout elapses; status \"timeout\" means call again to keep " +
			"waiting. Stay in the listen/converse loop to keep the voice dialog alive.",
	}, d.listen)

	mcp.AddTool(s, &mcp.Tool{
		Name: "speak",
		Description: "Speak a SHORT reply aloud without waiting for a response. Use terse " +
			"confirmations and answers only — do not read code, diffs, or long output aloud; let " +
			"those scroll in the terminal. To reply AND keep listening, use converse instead. Long " +
			"text is truncated.",
	}, d.speak)

	mcp.AddTool(s, &mcp.Tool{
		Name: "play_sound",
		Description: "Play a short sound through the speaker for a notification — e.g. task done, " +
			"needs attention, an alarm. Either a built-in sound (name = chime, success, error, " +
			"alert, alarm, ding) or an absolute path to a WAV file. Respects the user's volume/mute. " +
			"Use sparingly; for spoken words use speak/converse instead.",
	}, d.playSound)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "end_session",
		Description: "Drop this voice session. listen and speak stop working until you pair again.",
	}, d.endSession)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "status",
		Description: "Report the aispeech build version and this session's voice state: whether it is paired, focused, and whether the microphone is currently active.",
	}, d.status)

	return s
}

// --- tool inputs/outputs ---

type pairIn struct {
	Token string `json:"token" jsonschema:"the pairing token the user copied from the aispeech UI and pasted to you"`
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

type converseIn struct {
	Text           string `json:"text" jsonschema:"the short reply to speak before waiting for the next command"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"how long to wait for the next command, in seconds"`
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
	Version       string   `json:"version"`
	Paired        bool     `json:"paired"`
	Name          string   `json:"name"`
	Focused       bool     `json:"focused"`
	ListeningNow  bool     `json:"listening_now"`
	MicMode       string   `json:"mic_mode"`
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
	if d.voices != nil {
		d.reg.AssignVoice(id, d.voices()) // distinct voice per session
	}
	v, _ := d.reg.Get(id)
	return v
}

func (d *deps) pair(ctx context.Context, req *mcp.CallToolRequest, in pairIn) (*mcp.CallToolResult, pairOut, error) {
	d.attach(req)
	id := req.Session.ID()
	if d.reg.PairAttemptsExceeded(id, maxPairAttempts) {
		return nil, pairOut{}, fmt.Errorf("too many failed pairing attempts; reconnect and try again")
	}
	cookie, ok := d.store.ConsumeToken(in.Token)
	if !ok {
		d.reg.NotePairFailure(id)
		// Generic failure: do not distinguish invalid/expired/consumed tokens.
		return nil, pairOut{}, fmt.Errorf("pairing failed: ask the user to click \"Copy pairing token\" in the aispeech UI and paste you a fresh token")
	}
	s, err := d.reg.Pair(id, cookie)
	if err != nil {
		return nil, pairOut{}, err
	}
	return nil, pairOut{OK: true, SessionID: s.ID, Name: s.Name}, nil
}

// timeout resolves a requested wait to the configured default/max window.
func (d *deps) timeout(seconds int) time.Duration {
	t := d.opts.DefaultListenTimeout
	if seconds > 0 {
		t = time.Duration(seconds) * time.Second
	}
	if t > d.opts.MaxListenTimeout {
		t = d.opts.MaxListenTimeout
	}
	return t
}

func waitResult(u session.Utterance, status string) (*mcp.CallToolResult, listenOut, error) {
	switch status {
	case "ok":
		return nil, listenOut{Status: "ok", Text: u.Text, Session: u.Target}, nil
	case "unpaired":
		return nil, listenOut{}, errUnpaired
	default: // timeout | cancelled
		return nil, listenOut{Status: status}, nil
	}
}

func (d *deps) listen(ctx context.Context, req *mcp.CallToolRequest, in listenIn) (*mcp.CallToolResult, listenOut, error) {
	v := d.attach(req)
	if !v.Paired {
		return nil, listenOut{}, errUnpaired
	}
	u, status := d.reg.Listen(ctx, req.Session.ID(), d.timeout(in.TimeoutSeconds))
	return waitResult(u, status)
}

// converse speaks a reply and then waits for the next command — the one-call way
// to stay in a voice dialog.
func (d *deps) converse(ctx context.Context, req *mcp.CallToolRequest, in converseIn) (*mcp.CallToolResult, listenOut, error) {
	v := d.attach(req)
	if !v.Paired {
		return nil, listenOut{}, errUnpaired
	}
	if strings.TrimSpace(in.Text) != "" {
		if _, _, err := d.svc.SpeakAs(ctx, req.Session.ID(), in.Text); err != nil {
			return nil, listenOut{}, fmt.Errorf("speak failed: %w", err)
		}
	}
	u, status := d.reg.Listen(ctx, req.Session.ID(), d.timeout(in.TimeoutSeconds))
	return waitResult(u, status)
}

func (d *deps) speak(ctx context.Context, req *mcp.CallToolRequest, in speakIn) (*mcp.CallToolResult, speakOut, error) {
	v := d.attach(req)
	if !v.Paired {
		return nil, speakOut{}, errUnpaired
	}
	n, trunc, err := d.svc.SpeakAs(ctx, req.Session.ID(), in.Text)
	if err != nil {
		return nil, speakOut{}, err
	}
	return nil, speakOut{OK: true, SpokenChars: n, Truncated: trunc}, nil
}

type playSoundIn struct {
	Sound string `json:"sound,omitempty" jsonschema:"a built-in sound: chime, success, error, alert, alarm, ding"`
	File  string `json:"file,omitempty" jsonschema:"absolute path to a WAV file to play instead of a built-in sound"`
}
type playSoundOut struct {
	OK     bool   `json:"ok"`
	Played string `json:"played"`
}

func (d *deps) playSound(ctx context.Context, req *mcp.CallToolRequest, in playSoundIn) (*mcp.CallToolResult, playSoundOut, error) {
	v := d.attach(req)
	if !v.Paired {
		return nil, playSoundOut{}, errUnpaired
	}
	played, err := d.svc.PlaySound(ctx, in.Sound, in.File)
	if err != nil {
		return nil, playSoundOut{}, err
	}
	return nil, playSoundOut{OK: true, Played: played}, nil
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
		Version:       d.opts.Version,
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
