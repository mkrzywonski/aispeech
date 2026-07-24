// Package browseraudio bridges audio between the hub and a browser tab over a
// WebSocket, so the hub can use the browser machine's speaker and microphone
// (e.g. when reached over an ssh -L tunnel). It is transport-only: the engine's
// BrowserPlayer/BrowserRecorder adapters call into a Bridge, and the web layer
// feeds it accepted WebSocket connections. It knows nothing about MCP, sessions,
// or the engine.
//
// Protocol (one WebSocket per tab):
//   - Text frames are JSON control messages ({"type":...}).
//   - Binary frames are raw little-endian float32 mono PCM.
//
// Playback (hub → browser): a "play" control frame ({id,rate,n}) is followed by
// one binary frame of n samples; the browser plays it and replies {"type":
// "played","id":N}. "stop" interrupts the current clip.
//
// Capture (browser → hub): "capture" ({on:bool}) toggles the mic; the browser
// then sends, per utterance, "utt-start", one or more binary frames, "utt-end".
// The bridge assembles each utterance into a Clip on the Segments channel.
package browseraudio

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// SampleRate is the fixed capture/playback rate (whisper wants 16 kHz mono).
const SampleRate = 16000

// maxMessageBytes bounds a single WebSocket message. It must exceed the largest
// utterance clip (20 s * 16 kHz * float32 ≈ 1.28 MB) with generous margin.
const maxMessageBytes = 8 << 20 // 8 MiB

// Clip is one captured utterance as mono float32 PCM.
type Clip struct {
	PCM        []float32
	SampleRate int
}

type msg struct {
	Type string `json:"type"`
	ID   uint64 `json:"id,omitempty"`
	Rate int    `json:"rate,omitempty"`
	N    int    `json:"n,omitempty"`
	On   bool   `json:"on,omitempty"`
	Msg  string `json:"msg,omitempty"`
}

// conn is one connected browser tab.
type conn struct {
	ws      *websocket.Conn
	browser string
	writeMu sync.Mutex // serializes the (control+binary) write pair
	captBuf []float32  // accumulates the in-progress capture utterance
	inUtt   bool
}

func (c *conn) writeJSON(ctx context.Context, m msg) error {
	b, _ := json.Marshal(m)
	return c.ws.Write(ctx, websocket.MessageText, b)
}

// Bridge multiplexes browser audio over the set of connected tabs. The zero
// value is not usable; call New.
type Bridge struct {
	mu        sync.Mutex
	byBrowser map[string][]*conn // browser cookie -> its live connections
	outputID  string             // browser cookie that owns playback ("" = none)
	captureID string             // browser cookie that owns capture ("" = none)

	playID  atomic.Uint64
	pendMu  sync.Mutex
	pending map[uint64]chan struct{} // play id -> done signal

	segs chan Clip
}

// New returns a ready Bridge.
func New() *Bridge {
	return &Bridge{
		byBrowser: make(map[string][]*conn),
		pending:   make(map[uint64]chan struct{}),
		segs:      make(chan Clip, 8),
	}
}

// Segments delivers assembled capture utterances.
func (b *Bridge) Segments() <-chan Clip { return b.segs }

// Serve registers an accepted WebSocket connection for browserID and pumps its
// read loop until the connection closes or ctx is cancelled. It blocks; call it
// from the HTTP handler goroutine.
func (b *Bridge) Serve(ctx context.Context, ws *websocket.Conn, browserID string) error {
	// An utterance clip is one binary frame — up to ~1.3 MB (20 s * 16 kHz *
	// 4 bytes). The library's default 32 KiB read limit would close the socket
	// on the first real utterance, so raise it well past the max clip.
	ws.SetReadLimit(maxMessageBytes)
	c := &conn{ws: ws, browser: browserID}
	b.add(c)
	defer b.remove(c)
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return err
		}
		switch typ {
		case websocket.MessageBinary:
			b.onBinary(c, data)
		case websocket.MessageText:
			b.onText(c, data)
		}
	}
}

func (b *Bridge) add(c *conn) {
	b.mu.Lock()
	b.byBrowser[c.browser] = append(b.byBrowser[c.browser], c)
	b.mu.Unlock()
}

func (b *Bridge) remove(c *conn) {
	b.mu.Lock()
	live := b.byBrowser[c.browser]
	out := live[:0]
	for _, x := range live {
		if x != c {
			out = append(out, x)
		}
	}
	if len(out) == 0 {
		delete(b.byBrowser, c.browser)
	} else {
		b.byBrowser[c.browser] = out
	}
	b.mu.Unlock()
}

// latest returns the most recently registered connection for browserID.
func (b *Bridge) latest(browserID string) *conn {
	b.mu.Lock()
	defer b.mu.Unlock()
	live := b.byBrowser[browserID]
	if len(live) == 0 {
		return nil
	}
	return live[len(live)-1]
}

// --- ownership ---

// ClaimOutput makes browserID the playback endpoint. Returns false if that
// browser has no live connection.
func (b *Bridge) ClaimOutput(browserID string) bool {
	if b.latest(browserID) == nil {
		return false
	}
	b.mu.Lock()
	b.outputID = browserID
	b.mu.Unlock()
	return true
}

// ClaimInput makes browserID the capture endpoint.
func (b *Bridge) ClaimInput(browserID string) bool {
	if b.latest(browserID) == nil {
		return false
	}
	b.mu.Lock()
	b.captureID = browserID
	b.mu.Unlock()
	return true
}

// ReleaseOutput/ReleaseInput drop browser ownership of a direction.
func (b *Bridge) ReleaseOutput() { b.mu.Lock(); b.outputID = ""; b.mu.Unlock() }
func (b *Bridge) ReleaseInput()  { b.mu.Lock(); b.captureID = ""; b.mu.Unlock() }

func (b *Bridge) outputConn() *conn {
	b.mu.Lock()
	id := b.outputID
	b.mu.Unlock()
	if id == "" {
		return nil
	}
	return b.latest(id)
}

func (b *Bridge) captureConn() *conn {
	b.mu.Lock()
	id := b.captureID
	b.mu.Unlock()
	if id == "" {
		return nil
	}
	return b.latest(id)
}

// OutputConnected reports whether the claimed playback endpoint has a live tab.
func (b *Bridge) OutputConnected() bool { return b.outputConn() != nil }

// InputConnected reports whether the claimed capture endpoint has a live tab.
func (b *Bridge) InputConnected() bool { return b.captureConn() != nil }

// --- playback ---

// Play sends one PCM clip to the playback endpoint and blocks until the browser
// reports it finished, the context is cancelled, or Stop interrupts it. It
// returns nil when no browser output is claimed/connected (the clip is dropped),
// so it never blocks the speak() path on a missing tab.
func (b *Bridge) Play(ctx context.Context, pcm []float32, sampleRate int) error {
	c := b.outputConn()
	if c == nil || len(pcm) == 0 {
		return nil
	}
	id := b.playID.Add(1)
	done := make(chan struct{})
	b.pendMu.Lock()
	b.pending[id] = done
	b.pendMu.Unlock()
	defer func() {
		b.pendMu.Lock()
		delete(b.pending, id)
		b.pendMu.Unlock()
	}()

	c.writeMu.Lock()
	err := c.writeJSON(ctx, msg{Type: "play", ID: id, Rate: sampleRate, N: len(pcm)})
	if err == nil {
		err = c.ws.Write(ctx, websocket.MessageBinary, floatsToBytes(pcm))
	}
	c.writeMu.Unlock()
	if err != nil {
		return err
	}

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop interrupts the current playback clip (best-effort) and releases any Play
// waiting on an ack.
func (b *Bridge) Stop() {
	if c := b.outputConn(); c != nil {
		c.writeMu.Lock()
		_ = c.writeJSON(context.Background(), msg{Type: "stop"})
		c.writeMu.Unlock()
	}
	// Unblock every pending Play; the interrupted clip will not be acked.
	b.pendMu.Lock()
	for id, done := range b.pending {
		close(done)
		delete(b.pending, id)
	}
	b.pendMu.Unlock()
}

// --- capture ---

// StartCapture tells the capture endpoint to begin sending utterances.
func (b *Bridge) StartCapture() { b.captureControl(true) }

// StopCapture tells the capture endpoint to stop.
func (b *Bridge) StopCapture() { b.captureControl(false) }

func (b *Bridge) captureControl(on bool) {
	if c := b.captureConn(); c != nil {
		c.writeMu.Lock()
		_ = c.writeJSON(context.Background(), msg{Type: "capture", On: on})
		c.writeMu.Unlock()
	}
}

func (b *Bridge) onText(c *conn, data []byte) {
	var m msg
	if json.Unmarshal(data, &m) != nil {
		return
	}
	switch m.Type {
	case "played":
		b.pendMu.Lock()
		if done, ok := b.pending[m.ID]; ok {
			close(done)
			delete(b.pending, m.ID)
		}
		b.pendMu.Unlock()
	case "utt-start":
		c.captBuf = nil
		c.inUtt = true
	case "utt-end":
		if c.inUtt {
			b.emit(c.captBuf)
			c.captBuf = nil
			c.inUtt = false
		}
	}
}

func (b *Bridge) onBinary(c *conn, data []byte) {
	// Only accept capture audio from the claimed capture endpoint.
	if cc := b.captureConn(); cc != c {
		return
	}
	c.captBuf = append(c.captBuf, bytesToFloats(data)...)
	c.inUtt = true
}

func (b *Bridge) emit(pcm []float32) {
	if len(pcm) == 0 {
		return
	}
	select {
	case b.segs <- Clip{PCM: pcm, SampleRate: SampleRate}:
	case <-time.After(time.Second):
		// Drop rather than block the read loop if the consumer is stalled.
	}
}

// --- PCM (de)serialization ---

func floatsToBytes(pcm []float32) []byte {
	buf := make([]byte, len(pcm)*4)
	for i, s := range pcm {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(s))
	}
	return buf
}

func bytesToFloats(b []byte) []float32 {
	n := len(b) / 4
	out := make([]float32, n)
	for i := 0; i < n; i++ {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}
